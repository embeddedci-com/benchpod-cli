package ci

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ── what of a run folder goes to embeddedci.com ─────────────────────────────

// Upload limits of embeddedci.com.
const (
	MaxUploadRunJSON  = 20 << 20  // run.json
	MaxUploadFile     = 64 << 20  // one file
	MaxUploadRunFiles = 512 << 20 // all files of a run
)

// Uploaded is run.json's "uploaded": the run's id and page on embeddedci.com.
type Uploaded struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	BuildID string `json:"build_id,omitempty"`
}

var uploadDirs = map[string]bool{"trace": true, "att": true, "logs": true}

// ValidUploadName is embeddedci.com's rule for a run file's name: run.json, junit.xml, or
// "<trace|att|logs>/<name>" with name of [A-Za-z0-9._-], not starting with a dot, at most 200
// characters in all.
func ValidUploadName(name string) error {
	if name == "run.json" || name == "junit.xml" {
		return nil
	}
	if len(name) > 200 {
		return errors.New("file name is too long")
	}
	dir, base, ok := strings.Cut(name, "/")
	if !ok || !uploadDirs[dir] {
		return errors.New("file must be run.json, junit.xml or under trace/, att/ or logs/")
	}
	if base == "" || base[0] == '.' {
		return errors.New("invalid file name")
	}
	for _, c := range base {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return errors.New("file names may only use letters, digits, '.', '_' and '-' (one directory level)")
		}
	}
	return nil
}

var attRefRE = regexp.MustCompile(`"att"\s*:\s*"(att/[^"]+)"`)

// UploadFiles is what of the run folder dir goes up next to run.json, in order: junit.xml, the
// logs, the traces the keep policy keeps (all: every case's; failed: those of failed and errored
// cases; none: no traces) and the attachments those traces point to. Files that are missing or
// whose names embeddedci.com refuses are left out and listed in skipped.
func UploadFiles(dir string, r *Run) (files, skipped []string) {
	add := func(name string) {
		if err := ValidUploadName(name); err != nil {
			skipped = append(skipped, name+": "+err.Error())
			return
		}
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil || !st.Mode().IsRegular() {
			return
		}
		files = append(files, name)
	}
	add("junit.xml")
	if entries, err := os.ReadDir(filepath.Join(dir, "logs")); err == nil {
		var names []string
		for _, e := range entries {
			if e.Type().IsRegular() {
				names = append(names, "logs/"+e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			add(n)
		}
	}
	keep := r.Keep.Traces
	if keep == "" {
		keep = "failed"
	}
	seen := map[string]bool{}
	var traces []string
	for _, c := range r.Results.Cases {
		if c.Trace == "" || seen[c.Trace] || keep == "none" || (keep == "failed" && !c.Failing()) {
			continue
		}
		seen[c.Trace] = true
		traces = append(traces, c.Trace)
	}
	var atts []string
	for _, t := range traces {
		before := len(files)
		add(t)
		if len(files) == before {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(t)))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for sc.Scan() {
			for _, m := range attRefRE.FindAllSubmatch(sc.Bytes(), -1) {
				if a := string(m[1]); !seen[a] {
					seen[a] = true
					atts = append(atts, a)
				}
			}
		}
	}
	for _, a := range atts {
		add(a)
	}
	return files, skipped
}

// SetUploaded writes "uploaded" into the run.json at path, keeping every other field as it is.
func SetUploaded(path string, u Uploaded) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	doc["uploaded"] = raw
	return WriteJSON(path, doc)
}
