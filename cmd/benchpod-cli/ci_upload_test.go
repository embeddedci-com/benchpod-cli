package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/spf13/cobra"
)

// fakeRunsServer is embeddedci.com's runs API: it keeps the uploads and refuses file names the
// server refuses.
type fakeRunsServer struct {
	mu       sync.Mutex
	runJSON  []byte
	files    map[string][]byte
	auth     []string
	queries  []string
	failRun  bool
	compared string
	// compareBody, when set, is the compare reply.
	compareBody string
}

func startRunsServer(t *testing.T) (*fakeRunsServer, *httptest.Server) {
	t.Helper()
	f := &fakeRunsServer{files: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs":
			if f.failRun {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"failed to store the run"}`))
				return
			}
			f.runJSON = body
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r-1","url":"https://example.test/run/r-1","build_id":"b-9"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/runs/r-1/files":
			name := r.URL.Query().Get("name")
			if err := ci.ValidUploadName(name); err != nil || name == "run.json" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"bad name"}`))
				return
			}
			f.files[name] = body
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs":
			f.queries = append(f.queries, r.URL.RawQuery)
			_, _ = w.Write([]byte(`{"runs":[{"id":"r-2","suite":"s","dut":"d","outcome":"failed","valid":true,"started":"2026-11-18T14:02:11Z",
			  "counts":{"passed":3,"failed":1,"error":0},"tests_ref":"main","device_name":"benchpod-a1b2c3"}],"next_before":"2026-11-18T14:02:11Z","org_pods":2}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/runs/r-2":
			_, _ = w.Write([]byte(`{"schema_version":1,"run_id":"r-2","id":"r-2","url":"https://example.test/run/r-2","key":{"suite":"s","dut":"d"},
			  "health":{"valid":true},"inputs":{"pod":{"name":"benchpod-a1b2c3","firmware":"3.10.13"}},
			  "results":{"outcome":"failed","counts":{"passed":1,"failed":1},"cases":[
			    {"id":"boot","outcome":"passed","measurements":[{"name":"boot_ms","value":640,"unit":"ms","max":600,"outcome":"failed"}]},
			    {"id":"t::x","outcome":"failed","message":"assert 1 == 2"}]}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/compare"):
			f.compared = r.URL.Path + "?" + r.URL.RawQuery
			if f.compareBody != "" {
				_, _ = w.Write([]byte(f.compareBody))
				return
			}
			if r.URL.Query().Get("with") == "baseline:none" {
				_, _ = w.Write([]byte(`{"found":false,"a":null,"b":"r-2","message":"no earlier valid run of s on d to compare with"}`))
				return
			}
			_, _ = w.Write([]byte(`{"found":true,"a":"r-1","b":"r-2","comparable":true,"reasons":[],"flags":[],"inputs_changed":[],
			  "cases":{"newly_failing":["t::x"],"fixed":[],"still_failing":[],"new":[],"removed":[],"newly_skipped":[]},
			  "measurements":[],"slower":[],"calls_slower":[],"a_run":{"id":"r-1"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("BENCHPOD_API_BASE", srv.URL)
	t.Setenv("BENCHPOD_API_KEY", "eci_test")
	return f, srv
}

// noSignIn makes sure no credential is found.
func noSignIn(t *testing.T) {
	t.Setenv("BENCHPOD_API_KEY", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestCIRunUploads(t *testing.T) {
	fs, _ := startRunsServer(t)
	f := newCIFixture(t, `version: 1
suite: s
dut: d
steps:
  - id: boot
    power_cycle: {until: "NEVER", timeout: 100ms, off_ms: 0}
  - id: ok
    power_on: {}
`)
	f.withUpload = true
	exit, run := f.run()
	if exit.code != ci.ExitFailed {
		t.Fatalf("exit %d: an upload never changes it", exit.code)
	}
	if run.Uploaded == nil || run.Uploaded.ID != "r-1" || run.Uploaded.URL != "https://example.test/run/r-1" || run.Uploaded.BuildID != "b-9" || !strings.HasPrefix(run.RunID, "local-") {
		t.Fatalf("uploaded %+v run_id %s", run.Uploaded, run.RunID)
	}
	var sent ci.Run
	if err := json.Unmarshal(fs.runJSON, &sent); err != nil || sent.RunID != run.RunID || sent.Uploaded != nil {
		t.Errorf("run.json sent: %v %+v", err, sent.Uploaded)
	}
	var names []string
	for n := range fs.files {
		names = append(names, n)
	}
	got := strings.Join(names, ",")
	boot := caseByID(run, "boot")
	for _, want := range []string{"junit.xml", "logs/boot.log", "logs/cleanup.log", boot.Trace} {
		if _, ok := fs.files[want]; !ok {
			t.Errorf("not uploaded: %s (got %s)", want, got)
		}
	}
	if _, ok := fs.files["run.json"]; ok || strings.Contains(got, "parts/") || strings.Contains(got, "trace/ok") {
		t.Errorf("uploaded too much: %s", got)
	}
	for _, a := range fs.auth {
		if a != "ApiKey eci_test" {
			t.Errorf("auth %q", a)
		}
	}
	if !strings.Contains(f.stdout.String(), "Run URL: https://example.test/run/r-1") {
		t.Errorf("summary:\n%s", f.stdout.String())
	}
}

func TestCIRunUploadModes(t *testing.T) {
	fs, _ := startRunsServer(t)
	// A failed upload is a warning; the exit code is the run's.
	fs.failRun = true
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - wait: 1ms\n")
	f.withUpload = true
	exit, run := f.run()
	if exit.code != 0 || run.Uploaded != nil || !strings.Contains(f.stderr.String(), "upload failed") {
		t.Fatalf("exit %d uploaded %+v\n%s", exit.code, run.Uploaded, f.stderr.String())
	}
	// upload: false and --no-upload send nothing.
	fs.failRun = false
	for _, c := range []struct {
		yaml     string
		noUpload bool
	}{{"upload: false\n", false}, {"", true}} {
		f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\n"+c.yaml+"steps:\n  - wait: 1ms\n")
		f.withUpload, f.opts.noUpload = true, c.noUpload
		fs.runJSON = nil
		if _, run := f.run(); run.Uploaded != nil || fs.runJSON != nil {
			t.Errorf("uploaded with %q / --no-upload=%v", c.yaml, c.noUpload)
		}
	}
	// auto without a sign-in: a note, no upload; true without one: a setup error before the pod.
	noSignIn(t)
	f = newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - wait: 1ms\n")
	f.withUpload = true
	if exit, _ := f.run(); exit.code != 0 || !strings.Contains(f.stdout.String(), "Not uploaded") {
		t.Errorf("auto: exit %d\n%s", exit.code, f.stdout.String())
	}
	f = newCIFixture(t, "version: 1\nsuite: s\ndut: d\nupload: true\nsteps:\n  - power_on: {}\n")
	f.withUpload = true
	exit, _ = f.run()
	if exit.code != ci.ExitSetup || !strings.Contains(exit.msg, "upload: true") || len(f.pod.cmds()) != 0 {
		t.Errorf("true: %+v cmds %v", exit, f.pod.cmds())
	}
}

func execCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCIUploadCommand(t *testing.T) {
	fs, _ := startRunsServer(t)
	dir := t.TempDir()
	r := &ci.Run{RunID: "local-x", Key: ci.Key{Suite: "s", DUT: "d"}, Health: ci.Health{Valid: true}, Keep: ci.Keep{Traces: "failed"}}
	r.Finish(false)
	ci.WriteJSON(filepath.Join(dir, "run.json"), r)
	os.WriteFile(filepath.Join(dir, "junit.xml"), []byte("<testsuite/>"), 0o644)
	out, err := execCmd(t, newCIUploadCmd(), dir)
	if err != nil || !strings.Contains(out, "Run URL: https://example.test/run/r-1") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := fs.files["junit.xml"]; !ok {
		t.Error("junit.xml not uploaded")
	}
	back, _ := ci.ReadRun(dir)
	if back.Uploaded == nil || back.Uploaded.ID != "r-1" {
		t.Fatalf("run.json not updated: %+v", back.Uploaded)
	}
	if _, err := execCmd(t, newCIUploadCmd(), dir); err == nil || !strings.Contains(err.Error(), "uploaded already") {
		t.Errorf("second upload: %v", err)
	}
	if _, err := execCmd(t, newCIUploadCmd(), dir, "--again"); err != nil {
		t.Errorf("--again: %v", err)
	}
}

func TestCIListShowDiffRemote(t *testing.T) {
	fs, _ := startRunsServer(t)
	out, err := execCmd(t, newCIListCmd(), "--suite", "s", "--pod", "benchpod-a1b2c3", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	if fs.queries[0] != "device=benchpod-a1b2c3&limit=5&suite=s" {
		t.Errorf("query %s", fs.queries[0])
	}
	for _, want := range []string{"POD", "benchpod-a1b2c3", "r-2", "failed", "More: --before 2026-11-18T14:02:11Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	out, err = execCmd(t, newCIShowCmd(), "r-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"s (dut d): FAILED", "https://example.test/run/r-2", "t::x (failed)", "assert 1 == 2", "boot_ms", "640 ms", "max 600"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	out, err = execCmd(t, newCIDiffCmd(), "r-2")
	if err != nil || fs.compared != "/api/runs/r-2/compare?with=previous" || !strings.Contains(out, "Newly failing (1)") {
		t.Errorf("diff previous: %v %s\n%s", err, fs.compared, out)
	}
	if _, err := execCmd(t, newCIDiffCmd(), "r-1", "r-2"); err != nil || fs.compared != "/api/runs/r-2/compare?with=r-1" {
		t.Errorf("diff a b: %v %s", err, fs.compared)
	}
	out, err = execCmd(t, newCIDiffCmd(), "r-2", "--baseline", "none")
	if err != nil || !strings.Contains(out, "no earlier valid run") {
		t.Errorf("diff baseline: %v\n%s", err, out)
	}
	dir := t.TempDir()
	if _, err := execCmd(t, newCIDiffCmd(), dir, "r-2"); err == nil || !strings.Contains(err.Error(), "not one of each") {
		t.Errorf("mixed: %v", err)
	}
}

func TestCIShowLocal(t *testing.T) {
	dir := t.TempDir()
	r := &ci.Run{RunID: "local-x", Key: ci.Key{Suite: "s", DUT: "d"}, Health: ci.Health{Valid: true},
		Uploaded: &ci.Uploaded{ID: "r-1", URL: "https://example.test/run/r-1"},
		Results:  ci.Results{Cases: []ci.Case{{ID: "boot", Outcome: ci.Failed, Message: "no match"}}}}
	r.Finish(false)
	ci.WriteJSON(filepath.Join(dir, "run.json"), r)
	out, err := execCmd(t, newCIShowCmd(), dir)
	if err != nil || !strings.Contains(out, "boot (failed)") || !strings.Contains(out, "https://example.test/run/r-1") {
		t.Errorf("%v\n%s", err, out)
	}
}
