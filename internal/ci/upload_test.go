package ci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidUploadName(t *testing.T) {
	good := []string{"run.json", "junit.xml", "logs/hil.log", "logs/hil.attempt2.junit.xml", "att/cli-0001-wiring_get.json", "att/0001-la.json.zst",
		"trace/" + CaseSlug("tests/test_can_hil.py::test_ack[param one/two]") + ".ndjson", "trace/" + CaseSlug(strings.Repeat("é", 100)) + "-attempt2.ndjson"}
	for _, n := range good {
		if err := ValidUploadName(n); err != nil {
			t.Errorf("%q refused: %v", n, err)
		}
	}
	for _, n := range []string{"parts/hil.json", "trace/a/b", "trace/.x", "logs/a b.log", "../run.json", "trace/", "x.txt"} {
		if ValidUploadName(n) == nil {
			t.Errorf("%q accepted", n)
		}
	}
	if CaseSlug("") == "" {
		t.Error("an empty id gets a hash, like the SDK's slug")
	}
}

func TestUploadFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	write("junit.xml", "<testsuite/>")
	write("logs/boot.log", "x")
	write("logs/cleanup.log", "x")
	write("parts/hil.json", "{}")
	write("trace/boot.ndjson", `{"t":1,"op":"command","cmd":"wiring_get","att":"att/cli-0001-wiring_get.json"}`+"\n")
	write("trace/ok.ndjson", `{"t":1,"op":"command","att":"att/cli-0002-x.json"}`+"\n")
	write("att/cli-0001-wiring_get.json", "{}")
	write("att/cli-0002-x.json", "{}")
	r := &Run{Keep: Keep{Traces: "failed"}, Results: Results{Cases: []Case{
		{ID: "boot", Outcome: Failed, Trace: "trace/boot.ndjson"},
		{ID: "ok", Outcome: Passed, Trace: "trace/ok.ndjson"},
		{ID: "gone", Outcome: Errored, Trace: "trace/missing.ndjson"},
		{ID: "bad", Outcome: Failed, Trace: "trace/a b.ndjson"},
	}}}
	files, skipped := UploadFiles(dir, r)
	if got := strings.Join(files, ","); got != "junit.xml,logs/boot.log,logs/cleanup.log,trace/boot.ndjson,att/cli-0001-wiring_get.json" {
		t.Errorf("failed policy: %s", got)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "trace/a b.ndjson") {
		t.Errorf("skipped %v", skipped)
	}
	r.Keep.Traces = "all"
	files, _ = UploadFiles(dir, r)
	if !strings.Contains(strings.Join(files, ","), "trace/ok.ndjson") || !strings.Contains(strings.Join(files, ","), "att/cli-0002-x.json") {
		t.Errorf("all policy: %v", files)
	}
	r.Keep.Traces = "none"
	files, _ = UploadFiles(dir, r)
	if strings.Contains(strings.Join(files, ","), "trace/") {
		t.Errorf("none policy: %v", files)
	}
}

func TestSetUploadedKeepsFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.json")
	os.WriteFile(path, []byte(`{"schema_version":1,"run_id":"local-x","future":{"a":1}}`), 0o644)
	if err := SetUploaded(path, Uploaded{ID: "r-1", URL: "https://x/run/r-1"}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	data, _ := os.ReadFile(path)
	json.Unmarshal(data, &doc)
	if doc["run_id"] != "local-x" || doc["future"] == nil || doc["uploaded"].(map[string]any)["id"] != "r-1" {
		t.Errorf("%s", data)
	}
}
