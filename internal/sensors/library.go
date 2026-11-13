package sensors

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The public sensor library (github.com/embeddedci-com/benchpod-sensors). The CLI pins a version,
// PinnedVersion, and embeds its files, so the default works offline and never changes under a
// script. Another version, or "latest", is downloaded from the library's GitHub release archive
// once and cached per version under the user cache directory, the same cache the Python SDK uses.
// Without a network the cache serves, then the embedded copy, with a warning.

const (
	// PinnedVersion is the library version this CLI release pins and embeds.
	PinnedVersion = "0.1.0"
	// LibraryEnv selects another version (latest, 0.2.0, ...).
	LibraryEnv = "BENCHPOD_SENSOR_LIBRARY"
	// CacheEnv moves the download cache.
	CacheEnv = "BENCHPOD_SENSOR_CACHE"
	// Repo is the library's GitHub repository.
	Repo = "embeddedci-com/benchpod-sensors"

	maxArchiveBytes = 32 << 20
)

// GitHubURL and GitHubAPI are where the library comes from (variables so tests can use a fake).
var (
	GitHubURL = "https://github.com"
	GitHubAPI = "https://api.github.com"
)

//go:embed bundled
var bundledFS embed.FS

var versionRE = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)$`)

var entryDirs = []string{"sensors", "stimuli"}

// Entry is one file of the library.
type Entry struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
}

// Index is a library's library.json.
type Index struct {
	Format  int     `json:"format"`
	Version string  `json:"version"`
	Entries []Entry `json:"entries"`
}

// Library is one version of the sensor library.
type Library struct {
	Version string
	// Source is "bundled" (embedded in the CLI), "cache" or "download".
	Source string
	// Warning says why a fallback version is used; "" otherwise.
	Warning string
	Entries []Entry
	files   fs.FS
}

// Find returns the entry called name, or at that library path, or nil.
func (l *Library) Find(name string) *Entry {
	key := strings.TrimSpace(name)
	for i := range l.Entries {
		if l.Entries[i].Name == key || l.Entries[i].Path == key {
			return &l.Entries[i]
		}
	}
	return nil
}

// Get is Find with an error naming the entries when there is no such entry.
func (l *Library) Get(name string) (*Entry, error) {
	if e := l.Find(name); e != nil {
		return e, nil
	}
	names := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	list := strings.Join(names, ", ")
	if list == "" {
		list = "none"
	}
	return nil, fmt.Errorf("the sensor library %s has no entry %q (entries: %s)", l.Version, name, list)
}

// Text returns entry name's file as it is in the library (YAML).
func (l *Library) Text(name string) (string, error) {
	e, err := l.Get(name)
	if err != nil {
		return "", err
	}
	b, err := fs.ReadFile(l.files, e.Path)
	return string(b), err
}

// Load returns entry name as a Document (data segment files inlined).
func (l *Library) Load(name string) (Document, error) {
	e, err := l.Get(name)
	if err != nil {
		return Document{}, err
	}
	b, err := fs.ReadFile(l.files, e.Path)
	if err != nil {
		return Document{}, err
	}
	doc, err := ParseText(string(b))
	if err != nil {
		return Document{}, fmt.Errorf("%s: %w", e.Path, err)
	}
	dir := path.Dir(e.Path)
	return doc, InlineDataFilesFrom(doc, func(name string) ([]byte, error) {
		return fs.ReadFile(l.files, path.Join(dir, name))
	})
}

// FormatDoc is the library's docs/format.md, "" when this copy lacks it.
func (l *Library) FormatDoc() string {
	b, _ := fs.ReadFile(l.files, "docs/format.md")
	return string(b)
}

// BuildIndex is the library.json of a library tree: every definition and stimulus under
// sensors/ and stimuli/ with its name, kind and label.
func BuildIndex(fsys fs.FS, version string) Index {
	idx := Index{Format: 1, Version: version, Entries: []Entry{}}
	for _, top := range entryDirs {
		var files []string
		_ = fs.WalkDir(fsys, top, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			switch path.Ext(p) {
			case ".yaml", ".yml", ".json":
				files = append(files, p)
			}
			return nil
		})
		sort.Strings(files)
		for _, p := range files {
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				continue
			}
			doc, err := ParseText(string(b))
			if err != nil {
				log.Printf("sensor library %s: skipping %s: %v", version, p, err)
				continue
			}
			name := doc.Name()
			if name == "" {
				name = strings.TrimSuffix(path.Base(p), path.Ext(p))
			}
			idx.Entries = append(idx.Entries, Entry{Path: p, Name: name, Kind: doc.Kind(), Label: doc.str("label")})
		}
	}
	return idx
}

func openLibrary(version, source, warning string, fsys fs.FS) *Library {
	var idx Index
	b, err := fs.ReadFile(fsys, "library.json")
	if err != nil || json.Unmarshal(b, &idx) != nil {
		idx = BuildIndex(fsys, version)
	}
	return &Library{Version: version, Source: source, Warning: warning, Entries: idx.Entries, files: fsys}
}

// Bundled is the library version embedded in the CLI.
func Bundled() *Library {
	sub, _ := fs.Sub(bundledFS, "bundled")
	return openLibrary(PinnedVersion, "bundled", "", sub)
}

// NormalizeVersion turns "v0.2.0" into "0.2.0" and "default" into PinnedVersion; "latest" stays.
func NormalizeVersion(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "latest":
		return "latest", nil
	case "", "default", "pinned":
		return PinnedVersion, nil
	}
	m := versionRE.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("a sensor library version is 'latest' or a release such as %q, got %q", PinnedVersion, v)
	}
	return m[1], nil
}

// RequestedVersion is the version to use: flag, else BENCHPOD_SENSOR_LIBRARY, else the config's
// sensor_library, else PinnedVersion.
func RequestedVersion(flag, config string) (string, error) {
	for _, c := range []string{flag, os.Getenv(LibraryEnv), config} {
		if strings.TrimSpace(c) != "" {
			return NormalizeVersion(c)
		}
	}
	return PinnedVersion, nil
}

// CacheRoot is where downloaded versions live: BENCHPOD_SENSOR_CACHE, else
// <user cache>/embeddedci/benchpod-sensors.
func CacheRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv(CacheEnv)); v != "" {
		return v, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "embeddedci", "benchpod-sensors"), nil
}

// Options control Open.
type Options struct {
	// Version as asked for ("" = RequestedVersion's default chain already applied by the caller).
	Version string
	// Offline never touches the network.
	Offline bool
	// Client is the HTTP client (default: 30 s timeout).
	Client *http.Client
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// errNotFound is a release the library does not have.
var errNotFound = errors.New("not found")

func get(c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "benchpod-cli (sensor library)")
	req.Header.Set("Accept", "application/vnd.github+json, */*")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxArchiveBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, maxArchiveBytes)
	}
	return b, nil
}

func versionLess(a, b string) bool {
	pa, pb := splitVersion(a), splitVersion(b)
	for i := 0; i < 3; i++ {
		if pa.nums[i] != pb.nums[i] {
			return pa.nums[i] < pb.nums[i]
		}
	}
	if (pa.pre == "") != (pb.pre == "") {
		return pa.pre != "" // a pre-release sorts first
	}
	return pa.pre < pb.pre
}

type versionParts struct {
	nums [3]int
	pre  string
}

func splitVersion(v string) versionParts {
	var p versionParts
	core, pre, _ := strings.Cut(v, "-")
	core, _, _ = strings.Cut(core, "+")
	p.pre = pre
	for i, s := range strings.SplitN(core, ".", 3) {
		p.nums[i], _ = strconv.Atoi(s)
	}
	return p
}

// LatestVersion is the newest released library version on GitHub: the latest release, else the
// highest v* tag that is not a pre-release.
func LatestVersion(c *http.Client) (string, error) {
	if c == nil {
		c = httpClient
	}
	b, err := get(c, GitHubAPI+"/repos/"+Repo+"/releases/latest")
	if err == nil {
		var rel struct {
			TagName string `json:"tag_name"`
		}
		if json.Unmarshal(b, &rel) == nil && versionRE.MatchString(rel.TagName) {
			return NormalizeVersion(rel.TagName)
		}
	} else if !errors.Is(err, errNotFound) {
		return "", err
	}
	b, err = get(c, GitHubAPI+"/repos/"+Repo+"/tags?per_page=100")
	if err != nil {
		return "", err
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &tags); err != nil {
		return "", err
	}
	best := ""
	for _, t := range tags {
		if !versionRE.MatchString(t.Name) || strings.Contains(t.Name, "-") {
			continue
		}
		v, _ := NormalizeVersion(t.Name)
		if best == "" || versionLess(best, v) {
			best = v
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s has no released versions yet", Repo)
	}
	return best, nil
}

// safeMember is the path below the archive's top folder, or "" for anything that is not a
// library file.
func safeMember(name string) string {
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return ""
	}
	parts := strings.Split(path.Clean(name), "/")
	if len(parts) < 2 {
		return ""
	}
	for _, p := range parts {
		if p == ".." || p == "" {
			return ""
		}
	}
	rel := strings.Join(parts[1:], "/")
	top := parts[1]
	if top != "sensors" && top != "stimuli" && top != "docs" {
		return ""
	}
	switch path.Ext(rel) {
	case ".yaml", ".yml", ".json", ".csv", ".md":
		return rel
	}
	return ""
}

// Download fetches release version into the cache and returns its folder.
func Download(c *http.Client, version string) (string, error) {
	if c == nil {
		c = httpClient
	}
	root, err := CacheRoot()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/%s/archive/refs/tags/v%s.tar.gz", GitHubURL, Repo, version)
	data, err := get(c, url)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, "."+version+"-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(tmp)
		}
	}()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		rel := safeMember(h.Name)
		if rel == "" {
			continue
		}
		dest := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return "", err
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxArchiveBytes))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(dest, b, 0o644); err != nil {
			return "", err
		}
	}
	idx := BuildIndex(os.DirFS(tmp), version)
	if len(idx.Entries) == 0 {
		return "", fmt.Errorf("%s: the archive has no sensor definitions", url)
	}
	b, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, "library.json"), append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	final := filepath.Join(root, version)
	_ = os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	ok = true
	return final, nil
}

// CachedVersions are the versions in the download cache, oldest first.
func CachedVersions() []string {
	root, err := CacheRoot()
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && versionRE.MatchString(e.Name()) {
			if _, err := os.Stat(filepath.Join(root, e.Name(), "library.json")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[i], out[j]) })
	return out
}

func cached(version string) *Library {
	root, err := CacheRoot()
	if err != nil {
		return nil
	}
	dir := filepath.Join(root, version)
	if _, err := os.Stat(filepath.Join(dir, "library.json")); err != nil {
		return nil
	}
	return openLibrary(version, "cache", "", os.DirFS(dir))
}

func fallback(version, reason string) *Library {
	if version != "latest" {
		if lib := cached(version); lib != nil {
			return lib
		}
	} else if vs := CachedVersions(); len(vs) > 0 {
		newest := vs[len(vs)-1]
		lib := cached(newest)
		lib.Warning = fmt.Sprintf("sensor library 'latest': %s; using the newest cached version %s", reason, newest)
		return lib
	}
	lib := Bundled()
	lib.Warning = fmt.Sprintf("sensor library %s: %s; using the bundled version %s", version, reason, PinnedVersion)
	return lib
}

// Open returns the library version opts.Version (already normalized: a release, "latest" or
// PinnedVersion). The pinned version is the embedded copy; others come from the cache or are
// downloaded. A failed download falls back to the cache, then the embedded copy, with Warning
// set; a release that does not exist is an error.
func Open(opts Options) (*Library, error) {
	want, err := NormalizeVersion(opts.Version)
	if err != nil {
		return nil, err
	}
	if want == PinnedVersion {
		return Bundled(), nil
	}
	if want != "latest" {
		if lib := cached(want); lib != nil {
			return lib, nil
		}
	}
	if opts.Offline {
		return fallback(want, "offline"), nil
	}
	c := opts.Client
	if c == nil {
		c = httpClient
	}
	version := want
	if want == "latest" {
		if version, err = LatestVersion(c); err != nil {
			return fallback(want, "could not ask GitHub for the latest release ("+err.Error()+")"), nil
		}
		if version == PinnedVersion {
			return Bundled(), nil
		}
		if lib := cached(version); lib != nil {
			return lib, nil
		}
	}
	dir, err := Download(c, version)
	if errors.Is(err, errNotFound) {
		return nil, fmt.Errorf("the sensor library has no release %s (github.com/%s/releases)", version, Repo)
	}
	if err != nil {
		return fallback(want, "download failed ("+err.Error()+")"), nil
	}
	return openLibrary(version, "download", "", os.DirFS(dir)), nil
}
