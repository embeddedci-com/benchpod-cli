package sensors

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func mustJSON(t *testing.T, d Document) string {
	t.Helper()
	b, err := d.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestYAML12Scalars(t *testing.T) {
	d, err := ParseText("on: conversion\noff: 1\nyes: no\nflag: true\nx: 012\no: 0o17\ne: 1e5\n" +
		"d: 2026-01-01\nh: 0x76\nq: \"012\"\nn: ~\n0x10: k\n")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"on":"conversion","off":1,"yes":"no","flag":true,"x":12,"o":15,"e":100000,` +
		`"d":"2026-01-01","h":118,"q":"012","n":null,"16":"k"}`
	if got := mustJSON(t, d); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, err := ParseText("a: 1\na: 2\n"); err == nil || !strings.Contains(err.Error(), "duplicate key 'a'") {
		t.Fatalf("duplicate key: %v", err)
	}
	if _, err := ParseText(`{"a":1,"a":2}`); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("duplicate JSON key: %v", err)
	}
}

func TestKeyOrderIsKept(t *testing.T) {
	// constants and functions may use earlier ones, so the pod needs them in file order.
	d, err := ParseText("format: 1\nconstants:\n  Z: 1\n  A: Z + 1\n  M: A * 2\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, d); got != `{"format":1,"constants":{"Z":1,"A":"Z + 1","M":"A * 2"}}` {
		t.Fatal(got)
	}
	j, err := ParseText(`{"b":1,"a":{"z":0.5,"y":[1,2]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := mustJSON(t, j); got != `{"b":1,"a":{"z":0.5,"y":[1,2]}}` {
		t.Fatal(got)
	}
}

// The bundled definitions convert to the same JSON as the library's tools/yaml2json.py.
func TestBundledMatchesYaml2json(t *testing.T) {
	lib := Bundled()
	if lib.Version != PinnedVersion || len(lib.Entries) != 13 {
		t.Fatalf("bundled: %s %d entries", lib.Version, len(lib.Entries))
	}
	for _, e := range lib.Entries {
		doc, err := lib.Load(e.Name)
		if err != nil {
			t.Fatal(err)
		}
		golden, err := os.ReadFile(filepath.Join("testdata", "golden", e.Name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		got := mustJSON(t, doc)
		_ = json.Unmarshal([]byte(got), &a)
		_ = json.Unmarshal(golden, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s differs from yaml2json.py:\n%s\n%s", e.Name, got, golden)
		}
		check := CheckDefinition
		if e.Kind == "stimulus" {
			check = CheckStimulus
		}
		if err := check(doc); err != nil {
			t.Errorf("%s: %v", e.Name, err)
		}
	}
	if !strings.HasPrefix(lib.FormatDoc(), "# BenchPod sensor definition format, version 1") {
		t.Fatal("no format doc")
	}
}

func TestHeaderErrorsAreListedTogether(t *testing.T) {
	d, _ := ParseText("format: 2\nkind: can\nname: Bad Name\nparams:\n  - {}\n")
	err := CheckDefinition(d)
	for _, part := range []string{"format: 2", `kind: "can"`, `name: "Bad Name"`, "params[0]: needs a key"} {
		if err == nil || !strings.Contains(err.Error(), part) {
			t.Errorf("missing %q in %v", part, err)
		}
	}
	s, _ := ParseText("format: 1\nkind: stimulus\nname: s\n")
	if err := CheckDefinition(s); err == nil || !strings.Contains(err.Error(), "this is a stimulus") {
		t.Fatal(err)
	}
	if err := CheckStimulus(s); err != nil {
		t.Fatal(err)
	}
	big, _ := ParseText(`{"format":1,"kind":"i2c","name":"x","description":"` + strings.Repeat("x", 33000) + `"}`)
	if err := CheckDefinition(big); err == nil || !strings.Contains(err.Error(), "more than the pod's 32768") {
		t.Fatal(err)
	}
}

func TestStimulusDataFilesAreInlined(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "trace.csv"), []byte("t,value\n0,20\n10,25.5\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "s.yaml"), []byte("format: 1\nkind: stimulus\nname: warm\nsignals:\n"+
		"  temperature_c:\n    - { shape: data, file: trace.csv, interpolate: linear }\n"+
		"    - shape: repeat\n      count: 2\n      segments:\n        - { shape: data, file: trace.csv }\n"), 0o644)
	d, err := ReadFile(filepath.Join(dir, "s.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"format":1,"kind":"stimulus","name":"warm","signals":{"temperature_c":[` +
		`{"shape":"data","interpolate":"linear","points":[[0,20],[10,25.5]]},` +
		`{"shape":"repeat","count":2,"segments":[{"shape":"data","points":[[0,20],[10,25.5]]}]}]}}`
	if got := mustJSON(t, d); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestUploadChunksFitACloudCommand(t *testing.T) {
	for _, size := range []int{1, 500, 2000, 32768} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i * 7)
		}
		reqs, err := UploadRequests("sensor_define", data, strings.Repeat("x", 16), true)
		if err != nil {
			t.Fatal(err)
		}
		var out []byte
		for i, r := range reqs {
			b, _ := json.Marshal(r)
			if len(b) > MaxCommandBytes-CommandReserveBytes {
				t.Fatalf("size %d chunk %d: %d bytes", size, i, len(b))
			}
			if r["offset"] != len(out) || r["total"] != size {
				t.Fatalf("offset %v total %v", r["offset"], r["total"])
			}
			chunk, _ := base64.StdEncoding.DecodeString(r["data_b64"].(string))
			if len(chunk) > MaxChunkBytes {
				t.Fatalf("chunk of %d bytes, at most %d", len(chunk), MaxChunkBytes)
			}
			out = append(out, chunk...)
			_, dry := r["dry_run"]
			if dry != (i == len(reqs)-1) {
				t.Fatalf("dry_run on chunk %d of %d", i, len(reqs))
			}
		}
		if !bytes.Equal(out, data) {
			t.Fatalf("size %d: reassembled data differs", size)
		}
	}
	reqs, _ := UploadRequests("sensor_stimulus", []byte("{}"), "", false)
	if _, ok := reqs[0]["instance"]; ok || len(reqs) != 1 {
		t.Fatal(reqs)
	}
}

func TestPinsAddressesAndValues(t *testing.T) {
	for in, want := range map[string]any{"2": 2, "la14": 14, "PIN3": 3, "SDA_LINE": "SDA_LINE"} {
		got, err := PinValue("sda", in)
		if err != nil || got != want {
			t.Fatalf("%s: %v %v", in, got, err)
		}
	}
	if _, err := PinValue("sda", "15"); err == nil {
		t.Fatal("15 accepted")
	}
	if v, _ := PinValue("output", "Current"); v != "current" {
		t.Fatal(v)
	}
	if err := Pins("uart", map[string]any{"rx": 3}); err == nil || !strings.Contains(err.Error(), "needs pins tx") {
		t.Fatal(err)
	}
	if err := Pins("i2c", map[string]any{"sda": 2, "scl": 1, "mosi": 3}); err == nil || !strings.Contains(err.Error(), "not mosi") {
		t.Fatal(err)
	}
	if err := Pins("spi", map[string]any{"sclk": 1, "mosi": 2, "miso": 3, "cs": 4, "drdy": 5}); err == nil ||
		!strings.Contains(err.Error(), "no data-ready pin") {
		t.Fatal(err)
	}
	pins := map[string]any{"sda": "SDA_LINE", "scl": 1, "output": "dac"}
	if err := ResolvePins(pins, func(n string) (int, bool) { return 7, n == "SDA_LINE" }); err != nil || pins["sda"] != 7 || pins["output"] != "dac" {
		t.Fatal(pins, err)
	}
	if err := ResolvePins(map[string]any{"sda": "NOPE"}, nil); err == nil || !strings.Contains(err.Error(), "wiring profile") {
		t.Fatal(err)
	}
	if !InstanceRE.MatchString(strings.Repeat("a", 16)) || InstanceRE.MatchString(strings.Repeat("a", 17)) {
		t.Fatal("instance names are 1..16 characters")
	}
	if a, _ := Address("119"); a != "0x77" {
		t.Fatal(a)
	}
	if _, err := Address("0x400"); err == nil {
		t.Fatal("0x400 accepted")
	}
	v, err := Values([]string{"temperature_c=31.5", " humidity_pct = 60"})
	if err != nil || v["temperature_c"] != 31.5 || v["humidity_pct"] != 60.0 {
		t.Fatal(v, err)
	}
	if _, err := Values([]string{"t=hot"}); err == nil {
		t.Fatal("hot accepted")
	}
	if !HasCapability(json.RawMessage(`{"caps":["dac","sensors"]}`), "sensors") || HasCapability(json.RawMessage(`{"caps":["dac"]}`), "sensors") {
		t.Fatal("caps")
	}
}

// -- library -------------------------------------------------------------------

func archive(t *testing.T, version string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, text := range files {
		p := name
		if !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "..") {
			p = "benchpod-sensors-" + version + "/" + name
		}
		_ = tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(text)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(text))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

type fakeGitHub struct {
	mu       sync.Mutex
	archives map[string][]byte
	latest   string
	tags     []string
	requests []string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{archives: map[string][]byte{}}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.URL.RequestURI())
		repo := "/repos/" + Repo
		switch {
		case r.URL.Path == repo+"/releases/latest":
			if f.latest == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"tag_name":"v%s"}`, f.latest)
		case r.URL.Path == repo+"/tags":
			var tags []map[string]string
			for _, tg := range f.tags {
				tags = append(tags, map[string]string{"name": tg})
			}
			_ = json.NewEncoder(w).Encode(tags)
		case strings.HasPrefix(r.URL.Path, "/"+Repo+"/archive/refs/tags/v"):
			v := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"+Repo+"/archive/refs/tags/v"), ".tar.gz")
			if b, ok := f.archives[v]; ok {
				_, _ = w.Write(b)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	oldURL, oldAPI := GitHubURL, GitHubAPI
	GitHubURL, GitHubAPI = ts.URL, ts.URL
	t.Cleanup(func() { GitHubURL, GitHubAPI = oldURL, oldAPI })
	t.Setenv(CacheEnv, t.TempDir())
	t.Setenv(LibraryEnv, "")
	return f
}

const tinyDef = "format: 1\nkind: i2c\nname: %s\nlabel: \"%s\"\nparams: []\n"

func TestPinnedVersionNeedsNoNetwork(t *testing.T) {
	gh := newFakeGitHub(t)
	for _, v := range []string{"", "0.1.0", "v0.1.0", "default"} {
		lib, err := Open(Options{Version: v})
		if err != nil || lib.Source != "bundled" || lib.Version != "0.1.0" {
			t.Fatalf("%q: %+v %v", v, lib, err)
		}
	}
	if len(gh.requests) != 0 {
		t.Fatal(gh.requests)
	}
}

func TestDownloadOnceThenCache(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.archives["0.2.0"] = archive(t, "0.2.0", map[string]string{
		"sensors/i2c/new.yaml": fmt.Sprintf(tinyDef, "newsensor", "New"),
		"stimuli/warm.yaml":    "format: 1\nkind: stimulus\nname: warm\nsignals: {}\n",
		"docs/format.md":       "# format\n",
		"tools/evil.py":        "print(1)",
		"../escape.yaml":       fmt.Sprintf(tinyDef, "escape", "x"),
	})
	lib, err := Open(Options{Version: "0.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{Path: "sensors/i2c/new.yaml", Name: "newsensor", Kind: "i2c", Label: "New"},
		{Path: "stimuli/warm.yaml", Name: "warm", Kind: "stimulus"}}
	if lib.Source != "download" || !reflect.DeepEqual(lib.Entries, want) || lib.FormatDoc() != "# format\n" {
		t.Fatalf("%+v", lib)
	}
	root, _ := CacheRoot()
	if _, err := os.Stat(filepath.Join(root, "0.2.0", "tools")); err == nil {
		t.Fatal("tools/ extracted")
	}
	again, err := Open(Options{Version: "0.2.0"})
	if err != nil || again.Source != "cache" || len(gh.requests) != 1 {
		t.Fatalf("%+v %v %v", again, err, gh.requests)
	}
	if txt, _ := again.Text("newsensor"); !strings.Contains(txt, "newsensor") {
		t.Fatal(txt)
	}
}

func TestLatestFromReleaseThenTags(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.archives["0.3.0"] = archive(t, "0.3.0", map[string]string{"sensors/a.yaml": fmt.Sprintf(tinyDef, "a", "A")})
	gh.latest = "0.3.0"
	lib, err := Open(Options{Version: "latest"})
	if err != nil || lib.Version != "0.3.0" {
		t.Fatalf("%+v %v", lib, err)
	}
	gh.latest = ""
	gh.tags = []string{"v0.2.0", "v0.10.0", "v0.9.1", "junk", "v1.0.0-rc1"}
	if v, err := LatestVersion(nil); err != nil || v != "0.10.0" {
		t.Fatal(v, err)
	}
}

func TestOfflineFallsBackToCacheThenBundled(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.archives["0.2.0"] = archive(t, "0.2.0", map[string]string{"sensors/a.yaml": fmt.Sprintf(tinyDef, "a", "A")})
	if _, err := Open(Options{Version: "0.2.0"}); err != nil {
		t.Fatal(err)
	}
	GitHubURL, GitHubAPI = "http://127.0.0.1:9", "http://127.0.0.1:9" // nothing listens
	lib, err := Open(Options{Version: "latest"})
	if err != nil || lib.Version != "0.2.0" || lib.Source != "cache" || !strings.Contains(lib.Warning, "newest cached") {
		t.Fatalf("%+v %v", lib, err)
	}
	lib, err = Open(Options{Version: "0.4.0"})
	if err != nil || lib.Source != "bundled" || !strings.Contains(lib.Warning, "using the bundled version 0.1.0") {
		t.Fatalf("%+v %v", lib, err)
	}
	if lib, _ := Open(Options{Version: "0.2.0", Offline: true}); lib.Source != "cache" {
		t.Fatal(lib.Source)
	}
}

func TestMissingReleaseAndVersionSelection(t *testing.T) {
	newFakeGitHub(t)
	if _, err := Open(Options{Version: "9.9.9"}); err == nil || !strings.Contains(err.Error(), "no release 9.9.9") {
		t.Fatal(err)
	}
	if _, err := NormalizeVersion("../../etc"); err == nil {
		t.Fatal("path accepted")
	}
	t.Setenv(LibraryEnv, "latest")
	if v, _ := RequestedVersion("", "0.2.0"); v != "latest" {
		t.Fatal(v)
	}
	if v, _ := RequestedVersion("0.3.0", "0.2.0"); v != "0.3.0" {
		t.Fatal(v)
	}
	t.Setenv(LibraryEnv, "")
	if v, _ := RequestedVersion("", "0.2.0"); v != "0.2.0" {
		t.Fatal(v)
	}
	if v, _ := RequestedVersion("", ""); v != PinnedVersion {
		t.Fatal(v)
	}
}
