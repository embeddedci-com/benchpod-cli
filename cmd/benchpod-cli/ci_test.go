package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── a fake pod for the runner ───────────────────────────────────────────────

type fakeCIPod struct {
	mu        sync.Mutex
	reqs      []map[string]any
	version   string
	uart      *io.PipeWriter
	boot      string // what the DUT prints after power on
	noUART    bool
	flashErr  error
	flashed   []ciFlash
	statusErr error
	uptimes   []int64 // successive status uptime_ms values
	refuse    map[string]error
	profile   map[string]any
}

func newFakeCIPod() *fakeCIPod {
	return &fakeCIPod{version: "3.10.12", boot: "booting\r\nAPP_OK\r\n", refuse: map[string]error{},
		profile: map[string]any{"la_mv": 3300, "efuse": 1, "uart_rx": 5, "uart_tx": 4, "uart_baud": 115200, "swd_target": "target/stm32f4x.cfg"}}
}

func (p *fakeCIPod) cmds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.reqs {
		s := r["cmd"].(string)
		if s == "target_power" {
			s += fmt.Sprintf(":%v:%v", r["efuse"], r["state"])
		}
		out = append(out, s)
	}
	return out
}

func (p *fakeCIPod) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	cmd := req["cmd"].(string)
	if err := p.refuse[cmd]; err != nil {
		return nil, err
	}
	switch cmd {
	case "status":
		if p.statusErr != nil {
			return nil, p.statusErr
		}
		st := map[string]any{"version": p.version, "board_rev": "v3", "analog": true, "gateware": 58, "features_rev": 4, "la_vccio_mv": 3300}
		p.mu.Lock()
		if len(p.uptimes) > 0 {
			st["uptime_ms"] = p.uptimes[0]
			if len(p.uptimes) > 1 {
				p.uptimes = p.uptimes[1:]
			}
		}
		p.mu.Unlock()
		return json.Marshal(st)
	case "wiring_list":
		return json.RawMessage(`{"default":"node","profiles":[{"name":"node","rev":3,"bytes":100}]}`), nil
	case "wiring_get":
		data, _ := json.Marshal(p.profile)
		return json.Marshal(map[string]any{"rev": 3, "total": len(data), "data_b64": base64.RawURLEncoding.EncodeToString(data)})
	case "calibrate":
		return json.RawMessage(`{"offset":{"current_in":12}}`), nil
	case "target_power":
		if req["state"] == 1 {
			p.mu.Lock()
			w := p.uart
			p.mu.Unlock()
			if w != nil {
				go func() {
					time.Sleep(30 * time.Millisecond)
					_, _ = io.WriteString(w, p.boot)
				}()
			}
		}
		return json.RawMessage(`{"efuse":1,"enabled":1}`), nil
	case "target_status":
		return json.RawMessage(`{"efuse1":{"enabled":0},"efuse2":{"enabled":0}}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func (p *fakeCIPod) Chunks(_ context.Context, req map[string]any) ([]json.RawMessage, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	return []json.RawMessage{
		json.RawMessage(`{"status":"ok","t_us":[],"more":true}`),
		json.RawMessage(`{"status":"chunk","stats":{"avg_ua":12500,"min_ua":9000,"peak_ua":40000,"avg_mv":4990,"n":1000,"fault":false},"more":false}`),
	}, nil
}

type pipeRW struct {
	*io.PipeReader
	w *io.PipeWriter
}

func (p pipeRW) Write(b []byte) (int, error) { return len(b), nil }
func (p pipeRW) Close() error                { p.w.Close(); return p.PipeReader.Close() }

func (p *fakeCIPod) OpenUART(_ context.Context, req map[string]any) (io.ReadWriteCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	if p.noUART {
		return nil, errNeedsDirectLink
	}
	r, w := io.Pipe()
	p.uart = w
	return pipeRW{r, w}, nil
}

func (p *fakeCIPod) Flash(_ context.Context, fw ciFlash) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flashed = append(p.flashed, fw)
	return p.flashErr
}

func (p *fakeCIPod) Transport() string { return "lan" }

type fakeLease struct {
	id        string
	supported bool
	mu        sync.Mutex
	lostC     chan struct{}
	lost      string
	released  bool
	dropped   bool
}

func (l *fakeLease) ID() string      { return l.id }
func (l *fakeLease) Supported() bool { return l.supported }
func (l *fakeLease) Lost() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}
func (l *fakeLease) MarkLost(why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == "" {
		l.lost = why
	}
	if l.lostC == nil {
		l.lostC = make(chan struct{})
	}
	select {
	case <-l.lostC:
	default:
		close(l.lostC)
	}
}

func (l *fakeLease) LostC() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lostC == nil {
		l.lostC = make(chan struct{})
	}
	return l.lostC
}
func (l *fakeLease) Release(context.Context) { l.released = true }
func (l *fakeLease) Verify(context.Context)  {}
func (l *fakeLease) Drop()                   { l.dropped = true }

// ciFixture is a run folder, a YAML and a fake pod.
type ciFixture struct {
	t              *testing.T
	dir            string
	pod            *fakeCIPod
	lease          *fakeLease
	opts           ciOptions
	stdout, stderr bytes.Buffer
	leaseErr       error
	connErr        error
	join           *tcpclient.LeaseJoin
	withUpload     bool // the run uploads (to BENCHPOD_API_BASE) as the real command does
}

func newCIFixture(t *testing.T, yaml string) *ciFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture's child processes are shell scripts")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ci.DefaultFile), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return &ciFixture{t: t, dir: dir, pod: newFakeCIPod(), lease: &fakeLease{id: "L-test", supported: true},
		opts: ciOptions{file: filepath.Join(dir, ci.DefaultFile), out: filepath.Join(dir, "run"), repeat: 1}}
}

func (f *ciFixture) run() (*ciExit, *ci.Run) {
	f.t.Helper()
	cfg, err := ci.Load(f.opts.file, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	steps, err := cfg.Select(f.opts.steps)
	if err != nil {
		f.t.Fatal(err)
	}
	r := &ciRunner{cfg: cfg, opts: f.opts, steps: steps, stdout: &f.stdout, stderr: &f.stderr}
	if f.withUpload {
		r.signedIn = ciSignedIn
		r.upload = func(ctx context.Context, dir string) (*ci.Uploaded, error) {
			return uploadRunFolder(ctx, dir, &f.stderr)
		}
		r.compare = func(ctx context.Context, id, with string) (json.RawMessage, error) {
			api, cred, err := runsAPI(ctx)
			if err != nil {
				return nil, err
			}
			return api.CompareRun(ctx, cred, id, with)
		}
	}
	r.connect = func(context.Context) (*ciConnected, error) {
		if f.connErr != nil {
			return nil, f.connErr
		}
		return &ciConnected{pod: f.pod, target: &ciPodTarget{Name: "benchpod-a1b2c3", Why: "test"}, conn: "127.0.0.1:8080", hwAddr: "127.0.0.1:8080"}, nil
	}
	r.takeLease = func(_ context.Context, _ *ciConnected, join *tcpclient.LeaseJoin) (ciLease, error) {
		f.join = join
		if f.leaseErr != nil {
			return nil, f.leaseErr
		}
		return f.lease, nil
	}
	exit := r.execute(context.Background())
	run, err := ci.ReadRun(f.opts.out)
	if err != nil {
		f.t.Fatalf("run.json: %v\nstderr:\n%s", err, f.stderr.String())
	}
	return exit, run
}

func caseByID(run *ci.Run, id string) *ci.Case {
	for i := range run.Results.Cases {
		if run.Results.Cases[i].ID == id {
			return &run.Results.Cases[i]
		}
	}
	return nil
}

// fakePython is a shell script standing in for python: it answers the import check and, as
// `-m pytest`, writes a parts file (or a JUnit file when NO_PARTS is set) and records its env.
const fakePython = `#!/bin/sh
if [ "$1" = "-c" ]; then echo "3.0.9"; echo "3.12.4"; exit 0; fi
env | grep -E '^(EMBEDDEDCI_|BENCHPOD_)' | sort > "$EMBEDDEDCI_RUN_DIR/child-env-$EMBEDDEDCI_RUN_STEP.txt"
junit=""
while [ $# -gt 0 ]; do if [ "$1" = "--junitxml" ]; then junit="$2"; fi; shift; done
if [ -n "$NO_PARTS" ]; then
  if [ -z "$NO_JUNIT" ]; then
    printf '<testsuites><testsuite><testcase classname="tests.test_a" name="test_ok" time="0.1"/><testcase classname="tests.test_a" name="test_bad" time="0.2"><failure message="boom"/></testcase></testsuite></testsuites>' > "$junit"
  fi
  exit ${PYTEST_EXIT:-1}
fi
cat > "$EMBEDDEDCI_RUN_DIR/parts/$EMBEDDEDCI_RUN_STEP.json" <<EOF
{"schema_version":1,"step":"$EMBEDDEDCI_RUN_STEP","kind":"pytest","cases":[
 {"id":"tests/test_a.py::test_ok","outcome":"passed","duration_ms":12,"measurements":[{"name":"settle_ms","value":840,"unit":"ms","outcome":"recorded","source":"measure"}]},
 {"id":"tests/test_a.py::test_skip","outcome":"skipped","skip_reason":"no DAC"}],
 "health":{"busy_retries":1,"reasons":[]},"tools":{"sdk":"3.0.9","python":"3.12.4","pytest":"8.3.2"}}
EOF
exit 0
`

func writeFakePython(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fakepython")
	if err := os.WriteFile(path, []byte(fakePython), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCIRunFull(t *testing.T) {
	f := newCIFixture(t, "")
	py := writeFakePython(t, f.dir)
	os.WriteFile(filepath.Join(f.dir, "fw.elf"), []byte("firmware image"), 0o644)
	yaml := fmt.Sprintf(`version: 1
suite: node-hil
dut: node
wiring: node
require: {pod_firmware: ">=3.10.8"}
firmware: {file: fw.elf, flash: {target: target/stm32f4x.cfg}}
env: {SUITE_VAR: "x"}
keep: {traces: all}
steps:
  - id: boot
    power_cycle: {until: "APP_OK", timeout: 2s, off_ms: 10}
    required: true
    expect: {boot_ms: {max: 5000, drift: 20%%}}
  - id: idle
    measure_power: {duration: 10ms}
    expect: {mean_ma: {min: 5, max: 20}, peak_ma: {max: 30}}
  - id: hil
    pytest: {path: tests, python: %s}
    expect: {"tests/test_a.py::test_ok/settle_ms": {max: 1000}}
  - id: shell
    run: test "$SUITE_VAR" = x && test -n "$BENCHPOD_LEASE"
  - id: junit
    run: printf '<testsuite><testcase classname="c" name="t1"/></testsuite>' > out.xml
    results: junit:out.xml
  - id: gotest
    run: printf '{"Action":"run","Package":"p","Test":"TestA"}\n{"Action":"pass","Package":"p","Test":"TestA","Elapsed":0.5}\n'
    results: gotest-json
  - power_off: {}
`, py)
	os.WriteFile(f.opts.file, []byte(yaml), 0o644)
	f.pod.uptimes = []int64{1000, 2000}
	exit, run := f.run()
	if exit.code != ci.ExitFailed {
		t.Fatalf("exit %d (want 2: peak_ma over its limit)\n%s\n%s", exit.code, f.stdout.String(), f.stderr.String())
	}
	if run.Results.Outcome != ci.Failed || !run.Health.Valid {
		t.Errorf("outcome %s valid %v %v", run.Results.Outcome, run.Health.Valid, run.Health.Reasons)
	}
	var ids []string
	for _, c := range run.Results.Cases {
		ids = append(ids, c.ID+"="+c.Outcome)
	}
	want := "flash=passed,boot=passed,idle=failed,tests/test_a.py::test_ok=passed,tests/test_a.py::test_skip=skipped,shell=passed,c::t1=passed,p/TestA=passed,power-off=passed"
	if strings.Join(ids, ",") != want {
		t.Fatalf("cases\n got %s\nwant %s", strings.Join(ids, ","), want)
	}
	boot := caseByID(run, "boot")
	if len(boot.Measurements) != 1 || boot.Measurements[0].Name != "boot_ms" || boot.Measurements[0].Value <= 0 || *boot.Measurements[0].Drift != 0.2 || boot.Measurements[0].Source != "native" {
		t.Errorf("boot measurements %+v", boot.Measurements)
	}
	if boot.Calls["target_power"].N != 2 || boot.Trace == "" || boot.Calls["uart_proxy_start"].N != 1 {
		t.Errorf("boot calls %+v trace %q", boot.Calls, boot.Trace)
	}
	if _, err := os.Stat(filepath.Join(f.opts.out, boot.Trace)); err != nil {
		t.Errorf("trace file: %v", err)
	}
	idle := caseByID(run, "idle")
	if !strings.Contains(idle.Message, "peak_ma = 40 mA, outside max 30") || len(idle.Measurements) != 4 {
		t.Errorf("idle %+v", idle)
	}
	ok := caseByID(run, "tests/test_a.py::test_ok")
	if ok.Step != "hil" || ok.Kind != "pytest" || ok.Measurements[0].Max == nil || *ok.Measurements[0].Max != 1000 || ok.Measurements[0].Outcome != ci.Passed {
		t.Errorf("pytest case %+v", ok)
	}
	if run.Inputs.Tools.SDK != "3.0.9" || run.Inputs.Tools.Pytest != "8.3.2" || run.Inputs.Tools.CLI != version || run.Health.BusyRetries != 1 {
		t.Errorf("tools %+v health %+v", run.Inputs.Tools, run.Health)
	}
	in := run.Inputs
	if in.DUTFirmware.File != "fw.elf" || in.DUTFirmware.Size != 14 || in.DUTFirmware.SHA256 != ci.SHA256Hex([]byte("firmware image")) {
		t.Errorf("firmware %+v", in.DUTFirmware)
	}
	if in.Pod.Name != "benchpod-a1b2c3" || in.Pod.Firmware != "3.10.12" || in.Pod.Board != "analog-v3" || in.Pod.Gateware != 58 || in.Pod.FeaturesRev != 4 || in.Pod.Connection != "lan" || in.Pod.CalibrationSHA256 == "" {
		t.Errorf("pod %+v", in.Pod)
	}
	if in.Wiring.Name != "node" || in.Wiring.SHA256 != ci.CanonicalSHA256(in.Wiring.Snapshot) || in.Wiring.Snapshot["uart_rx"].(float64) != 5 {
		t.Errorf("wiring %+v", in.Wiring)
	}
	if run.Key.Suite != "node-hil" || run.Key.DefinitionSHA256 != ci.SHA256Hex([]byte(yaml)) || run.Keep.Traces != "all" {
		t.Errorf("key %+v", run.Key)
	}
	if len(f.pod.flashed) != 1 || f.pod.flashed[0].File != filepath.Join(f.dir, "fw.elf") || f.pod.flashed[0].Overrides.Target != "target/stm32f4x.cfg" {
		t.Errorf("flashed %+v", f.pod.flashed)
	}
	// Child env (spec section 4).
	env, _ := os.ReadFile(filepath.Join(f.opts.out, "child-env-hil.txt"))
	for _, want := range []string{"BENCHPOD_LEASE=L-test", "BENCHPOD_CONNECTION=127.0.0.1:8080", "BENCHPOD_HW_ADDR=127.0.0.1:8080",
		"BENCHPOD_WIRING_PROFILE=node", "EMBEDDEDCI_KEEP_TRACES=all", "EMBEDDEDCI_RUN_STEP=hil", "EMBEDDEDCI_RUN_ID=" + run.RunID, "EMBEDDEDCI_RUN_DIR=" + f.opts.out} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("child env lacks %s:\n%s", want, env)
		}
	}
	// Cleanup ran after the steps: DAC stop, both rails off, sensors off.
	cmds := strings.Join(f.pod.cmds(), ",")
	if !strings.Contains(cmds, "dac_stop,target_power:1:0,target_power:2:0,sensor_stop") {
		t.Errorf("cleanup missing: %s", cmds)
	}
	if !f.lease.released {
		t.Error("lease not released")
	}
	if f.join == nil || !strings.HasPrefix(f.join.ID, "L-") || f.join.Purpose != "node-hil" || f.join.TTL != 300 || !strings.HasPrefix(f.join.Holder, "benchpod ci: ") || len(f.join.Holder) > 40 {
		t.Errorf("lease join %+v", f.join)
	}
	for _, name := range []string{"junit.xml", "logs/boot.log", "logs/hil.log", "logs/cleanup.log", "logs/shell.log"} {
		if _, err := os.Stat(filepath.Join(f.opts.out, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.Contains(f.stdout.String(), "FAILED") || !strings.Contains(f.stdout.String(), "idle (failed)") {
		t.Errorf("summary:\n%s", f.stdout.String())
	}
}

func TestCIRequiredStepStopsTheRun(t *testing.T) {
	f := newCIFixture(t, `version: 1
suite: s
dut: d
steps:
  - id: boot
    power_cycle: {until: "NEVER", timeout: 200ms, off_ms: 0}
    required: true
  - id: later
    power_on: {}
`)
	exit, run := f.run()
	if exit.code != ci.ExitFailed || run.Results.Outcome != ci.Failed {
		t.Fatalf("exit %d outcome %s", exit.code, run.Results.Outcome)
	}
	boot, later := caseByID(run, "boot"), caseByID(run, "later")
	if boot.Outcome != ci.Failed || !strings.Contains(boot.Message, "no match for /NEVER/") || !strings.Contains(boot.Message, "APP_OK") {
		t.Errorf("boot %+v", boot)
	}
	if boot.Log == "" || boot.Trace == "" {
		t.Errorf("failed case keeps log and trace (keep: failed): %+v", boot)
	}
	if later.Outcome != ci.Skipped || !strings.Contains(later.SkipReason, "required step boot failed") {
		t.Errorf("later %+v", later)
	}
	// --keep-going runs it anyway; the run still fails.
	f2 := newCIFixture(t, string(must(os.ReadFile(f.opts.file))))
	f2.opts.keepGoing = true
	exit, run = f2.run()
	if caseByID(run, "later").Outcome != ci.Passed || exit.code != ci.ExitFailed {
		t.Errorf("keep-going: %+v exit %d", caseByID(run, "later"), exit.code)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestCIBusyLeaseIsInvalid(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - power_on: {}\n")
	f.leaseErr = &ciBusyError{msg: "the pod is in use: web: alice holds this pod (demo, 40 s left)"}
	exit, run := f.run()
	if exit.code != ci.ExitInvalid || run.Results.Outcome != ci.Invalid || !strings.Contains(exit.msg, "alice") {
		t.Fatalf("exit %+v outcome %s", exit, run.Results.Outcome)
	}
	if strings.Contains(strings.Join(f.pod.cmds(), ","), "target_power") {
		t.Errorf("drove the pod: %v", f.pod.cmds())
	}
}

func TestCIRequireNotMet(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nrequire: {pod_firmware: \">=3.11\"}\nsteps:\n  - power_on: {}\n")
	exit, run := f.run()
	if exit.code != ci.ExitInvalid || run.Health.Valid || !strings.Contains(run.Health.Reasons[0], "does not meet require.pod_firmware") {
		t.Fatalf("exit %+v health %+v", exit, run.Health)
	}
	if strings.Contains(strings.Join(f.pod.cmds(), ","), "target_power") || !f.lease.released {
		t.Errorf("cmds %v released %v", f.pod.cmds(), f.lease.released)
	}
}

func TestCILeaseLostAndRebootAreInvalid(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - power_on: {}\n  - power_off: {}\n")
	f.pod.refuse["target_power"] = &tcpclient.PodError{Reason: "lease lost: stopped by alice", Code: "BP0026", Args: map[string]any{"by": "alice"}}
	f.pod.uptimes = []int64{50000, 10}
	exit, run := f.run()
	if exit.code != ci.ExitInvalid || !run.Health.LeaseLost {
		t.Fatalf("exit %d health %+v", exit.code, run.Health)
	}
	reasons := strings.Join(run.Health.Reasons, "|")
	if !strings.Contains(reasons, "stopped by alice") || !strings.Contains(reasons, "rebooted") {
		t.Errorf("reasons %s", reasons)
	}
	if c := caseByID(run, "power-off"); c.Outcome != ci.Skipped {
		t.Errorf("after the loss %+v", c)
	}
	if !f.lease.dropped || !strings.Contains(reasons, "stopped by alice") {
		t.Errorf("cleanup did not drop the lease: %v %s", f.lease.dropped, reasons)
	}
	if !strings.Contains(strings.Join(f.pod.cmds(), ","), "dac_stop") {
		t.Errorf("no cleanup after the loss: %v", f.pod.cmds())
	}
}

func TestCIPytestFallbacks(t *testing.T) {
	f := newCIFixture(t, "")
	py := writeFakePython(t, f.dir)
	os.WriteFile(f.opts.file, []byte("version: 1\nsuite: s\ndut: none\nsteps:\n  - pytest: {python: "+py+"}\n"), 0o644)
	t.Setenv("NO_PARTS", "1")
	exit, run := f.run()
	if exit.code != ci.ExitFailed || len(run.Results.Cases) != 2 || run.Results.Cases[1].ID != "tests/test_a.py::test_bad" || run.Results.Cases[1].Outcome != ci.Failed {
		t.Fatalf("junit fallback: exit %d cases %+v", exit.code, run.Results.Cases)
	}
	if !strings.Contains(f.stderr.String(), "wrote no results") {
		t.Errorf("no note about the missing parts file:\n%s", f.stderr.String())
	}
	// No parts, no JUnit: the exit status decides.
	t.Setenv("NO_JUNIT", "1")
	t.Setenv("PYTEST_EXIT", "3")
	f2 := newCIFixture(t, "")
	py2 := writeFakePython(t, f2.dir)
	os.WriteFile(f2.opts.file, []byte("version: 1\nsuite: s\ndut: none\nsteps:\n  - pytest: {python: "+py2+"}\n"), 0o644)
	exit, run = f2.run()
	if exit.code != ci.ExitFailed || len(run.Results.Cases) != 1 || run.Results.Cases[0].Outcome != ci.Errored || !strings.Contains(run.Results.Cases[0].Message, "internal error") {
		t.Fatalf("exit-code fallback: %+v", run.Results.Cases)
	}
	// A python without embeddedci: the step errors with the fix.
	f3 := newCIFixture(t, "version: 1\nsuite: s\ndut: none\nsteps:\n  - pytest: {python: /bin/false}\n")
	_, run = f3.run()
	if c := run.Results.Cases[0]; c.Outcome != ci.Errored || !strings.Contains(c.Message, "pip install embeddedci") {
		t.Errorf("import check %+v", c)
	}
}

func TestCIRunStepFailures(t *testing.T) {
	f := newCIFixture(t, `version: 1
suite: s
dut: none
steps:
  - id: fails
    run: echo oops; exit 3
  - id: nofile
    run: "true"
    results: junit:missing.xml
  - id: slow
    run: sleep 5
    timeout: 300ms
`)
	exit, run := f.run()
	if exit.code != ci.ExitFailed {
		t.Fatalf("exit %d", exit.code)
	}
	if c := caseByID(run, "fails"); c.Outcome != ci.Failed || !strings.Contains(c.Message, "exit status 3") || !strings.Contains(c.Log, "oops") {
		t.Errorf("fails %+v", c)
	}
	if c := caseByID(run, "nofile"); c.Outcome != ci.Errored || !strings.Contains(c.Message, "wrote no") {
		t.Errorf("nofile %+v", c)
	}
	if c := caseByID(run, "slow"); c.Outcome != ci.Errored || !strings.Contains(c.Message, "timed out") || c.DurationMS > 4000 {
		t.Errorf("slow %+v", c)
	}
}

func TestCIRepeat(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nfirmware: {build_id: abc}\nsteps:\n  - wait: 1ms\n")
	f.opts.repeat = 3
	exit, run := f.run()
	if exit.code != 0 {
		t.Fatalf("exit %d\n%s", exit.code, f.stderr.String())
	}
	var got []string
	for _, c := range run.Results.Cases {
		got = append(got, fmt.Sprintf("%s#%d", c.ID, c.Attempt))
	}
	if strings.Join(got, ",") != "flash#1,wait#1,wait#2,wait#3" || len(f.pod.flashed) != 1 || f.pod.flashed[0].BuildID != "abc" {
		t.Errorf("cases %v flashed %+v", got, f.pod.flashed)
	}
	if run.Inputs.DUTFirmware.BuildID != "abc" {
		t.Errorf("build id %+v", run.Inputs.DUTFirmware)
	}
}

func TestCIWiringProfileMissing(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nwiring: other\nsteps:\n  - power_on: {}\n")
	exit, _ := f.run()
	if exit.code != ci.ExitSetup || !strings.Contains(exit.msg, `no wiring profile "other"`) || !strings.Contains(exit.msg, "node") {
		t.Fatalf("exit %+v", exit)
	}
}

func TestCIConnectFailureIsSetupError(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - power_on: {}\n")
	f.connErr = errors.New("no pod found")
	exit, run := f.run()
	if exit.code != ci.ExitSetup || run.Results.Outcome != ci.Errored {
		t.Fatalf("exit %+v outcome %s", exit, run.Results.Outcome)
	}
}

func TestCIJSONOutput(t *testing.T) {
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - wait: 1ms\n")
	f.opts.json = true
	f.run()
	var r ci.Run
	if err := json.Unmarshal(f.stdout.Bytes(), &r); err != nil || r.Results.Outcome != ci.Passed {
		t.Fatalf("stdout is not run.json: %v\n%s", err, f.stdout.String())
	}
}

// ── lease requests (fake pod commander) ─────────────────────────────────────

type scriptedCmd struct {
	mu    sync.Mutex
	reqs  []map[string]any
	reply func(req map[string]any) (json.RawMessage, error)
}

func (s *scriptedCmd) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.reply(req)
}

func (s *scriptedCmd) all() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.reqs...)
}

func TestLocalLeaseRequests(t *testing.T) {
	t.Cleanup(func() { tcpclient.SetLeaseJoin(nil) })
	renewErr := error(nil)
	var mu sync.Mutex
	cmd := &scriptedCmd{reply: func(req map[string]any) (json.RawMessage, error) {
		mu.Lock()
		defer mu.Unlock()
		if req["cmd"] == "lease_renew" && renewErr != nil {
			return nil, renewErr
		}
		return json.RawMessage(`{"lease":{"id":"L-x","left_s":300,"source":"local"}}`), nil
	}}
	join := &tcpclient.LeaseJoin{ID: "L-x", Holder: "benchpod ci: me", Purpose: "suite", TTL: 300}
	var warn bytes.Buffer
	l, err := acquireLocalLease(context.Background(), cmd, join, 0, 20*time.Millisecond, &warn)
	if err != nil || !l.Supported() {
		t.Fatalf("%v %v", l, err)
	}
	time.Sleep(70 * time.Millisecond)
	mu.Lock()
	renewErr = &tcpclient.PodError{Reason: "lease not held", Code: "BP0027"}
	mu.Unlock()
	time.Sleep(70 * time.Millisecond)
	if !strings.Contains(l.Lost(), "lease not held") {
		t.Errorf("lost %q", l.Lost())
	}
	l.Release(context.Background())
	reqs := cmd.all()
	acq := reqs[0]
	if acq["cmd"] != "lease_acquire" || acq["id"] != "L-x" || acq["holder"] != "benchpod ci: me" || acq["purpose"] != "suite" || acq["ttl_s"] != 300 {
		t.Errorf("acquire %v", acq)
	}
	renew := reqs[1]
	if renew["cmd"] != "lease_renew" || renew["id"] != "L-x" || renew["ttl_s"] != 300 || len(renew) != 3 {
		t.Errorf("renew %v", renew)
	}
	last := reqs[len(reqs)-1]
	if last["cmd"] != "lease_release" || last["id"] != "L-x" || len(last) != 2 {
		t.Errorf("release %v", last)
	}
}

func TestLocalLeaseBusyAndUnsupported(t *testing.T) {
	t.Cleanup(func() { tcpclient.SetLeaseJoin(nil) })
	join := &tcpclient.LeaseJoin{ID: "L-y", TTL: 300}
	busy := &scriptedCmd{reply: func(map[string]any) (json.RawMessage, error) {
		return nil, &tcpclient.PodError{Reason: "busy: web: alice holds this pod (demo, 40 s left)", Code: "BP0025",
			Args: map[string]any{"holder": "web: alice", "purpose": "demo", "left_s": float64(40)}}
	}}
	_, err := acquireLocalLease(context.Background(), busy, join, 0, time.Minute, io.Discard)
	var be *ciBusyError
	if !errors.As(err, &be) || !strings.Contains(be.msg, "web: alice holds this pod (demo, 40 s left)") || !strings.Contains(be.msg, "--wait") {
		t.Fatalf("busy: %v", err)
	}
	old := &scriptedCmd{reply: func(map[string]any) (json.RawMessage, error) {
		return nil, &tcpclient.PodError{Reason: "unknown cmd", Code: "BP0002"}
	}}
	var warn bytes.Buffer
	l, err := acquireLocalLease(context.Background(), old, join, 0, time.Minute, &warn)
	if err != nil || l.Supported() || !strings.Contains(warn.String(), "no lease") {
		t.Fatalf("unsupported: %v %v %q", l, err, warn.String())
	}
	l.Release(context.Background())
	if n := len(old.all()); n != 1 {
		t.Errorf("an unsupported lease sends nothing more: %d requests", n)
	}
}

func TestChoosePod(t *testing.T) {
	if _, _, err := choosePodFrom(nil, nil); err == nil || !strings.Contains(err.Error(), "--pod") {
		t.Errorf("none: %v", err)
	}
	one := []podCandidate{{Name: "benchpod-aaaaaa", Addr: "10.0.0.1:8080"}}
	if c, why, err := choosePodFrom(one, nil); err != nil || c.Addr != "10.0.0.1:8080" || why != "the only pod found" {
		t.Errorf("one: %v %s %v", c, why, err)
	}
	three := []podCandidate{{Name: "a", Addr: "1"}, {Name: "b", Addr: "2"}, {Name: "c", Addr: "3"}}
	free := func(c podCandidate) (bool, string, error) {
		switch c.Name {
		case "a":
			return false, "web: alice", nil
		case "b":
			return false, "", errors.New("timeout")
		}
		return true, "", nil
	}
	if c, why, err := choosePodFrom(three, free); err != nil || c.Name != "c" || !strings.Contains(why, "c is free") {
		t.Errorf("free: %v %s %v", c, why, err)
	}
	_, _, err := choosePodFrom(three[:2], free)
	var be *ciBusyError
	if !errors.As(err, &be) || !strings.Contains(be.msg, "a (in use by web: alice)") || !strings.Contains(be.msg, "b (timeout)") {
		t.Errorf("all busy: %v", err)
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "BENCHPOD_HW_ADDR=old"}, map[string]string{"B": "3", "C": "4"}, map[string]string{"BENCHPOD_HW_ADDR": "\x00"})
	if strings.Join(got, ",") != "A=1,B=3,C=4" {
		t.Errorf("%v", got)
	}
}

// ── the commands' exit codes ────────────────────────────────────────────────

func TestCICommandExitCodes(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.yaml")
	os.WriteFile(good, []byte("version: 1\nsuite: s\ndut: none\nsteps:\n  - wait: 1s\n"), 0o644)
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(bad, []byte("version: 1\nsteps:\n  - wait: soon\n"), 0o644)
	if code := run([]string{"ci", "validate", "-f", good}); code != 0 {
		t.Errorf("validate good: %d", code)
	}
	if code := run([]string{"ci", "validate", "-f", bad}); code != 1 {
		t.Errorf("validate bad: %d", code)
	}
	if code := run([]string{"ci", "run", "-f", bad}); code != 1 {
		t.Errorf("run bad: %d", code)
	}
	if code := run([]string{"ci", "run", "--no-such-flag"}); code != 1 {
		t.Errorf("usage error under ci: %d", code)
	}
	if code := run([]string{"ci", "-f", filepath.Join(dir, "nope.yaml")}); code != 1 {
		t.Errorf("missing file: %d", code)
	}
	// diff of two run folders
	mk := func(name, outcome string) string {
		d := filepath.Join(dir, name)
		os.MkdirAll(d, 0o755)
		r := &ci.Run{RunID: name, Key: ci.Key{Suite: "s", DUT: "d"}, Health: ci.Health{Valid: true},
			Results: ci.Results{Cases: []ci.Case{{ID: "t", Outcome: outcome}}}}
		r.Finish(false)
		ci.WriteJSON(filepath.Join(d, "run.json"), r)
		return d
	}
	a, b := mk("a", ci.Passed), mk("b", ci.Failed)
	out := filepath.Join(dir, "diff.json")
	if code := run([]string{"ci", "diff", a, filepath.Join(b, "run.json"), "--json", "--output-filename", out}); code != 0 {
		t.Errorf("diff: %d", code)
	}
	if code := run([]string{"ci", "diff", a, filepath.Join(dir, "missing")}); code != 1 {
		t.Errorf("diff missing: %d", code)
	}
}

func TestCIDiffText(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, outcome string) string {
		d := filepath.Join(dir, name)
		os.MkdirAll(d, 0o755)
		r := &ci.Run{RunID: name, Key: ci.Key{Suite: "s", DUT: "d"}, Health: ci.Health{Valid: true},
			Results: ci.Results{Cases: []ci.Case{{ID: "t::x", Outcome: outcome}}}}
		r.Finish(false)
		ci.WriteJSON(filepath.Join(d, "run.json"), r)
		return d
	}
	cmd := newCIDiffCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{mk("a", ci.Passed), mk("b", ci.Failed)})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Newly failing (1)") || !strings.Contains(buf.String(), "t::x") {
		t.Errorf("%s", buf.String())
	}
}

// slowPython writes its parts file (one case done), then runs on as if the tests went on.
const slowPython = `#!/bin/sh
if [ "$1" = "-c" ]; then echo "3.0.9"; echo "3.12.4"; exit 0; fi
cat > "$EMBEDDEDCI_RUN_DIR/parts/$EMBEDDEDCI_RUN_STEP.json" <<EOF
{"schema_version":1,"step":"$EMBEDDEDCI_RUN_STEP","kind":"pytest","cases":[{"id":"tests/test_a.py::test_first","outcome":"passed"}],
 "health":{"reasons":["the pod lease was stopped by Alice Example during the run"]}}
EOF
sleep 30
`

func TestCILeaseLostStopsTheRunningStep(t *testing.T) {
	f := newCIFixture(t, "")
	py := filepath.Join(f.dir, "slowpython")
	os.WriteFile(py, []byte(slowPython), 0o755)
	os.WriteFile(f.opts.file, []byte("version: 1\nsuite: s\ndut: d\nsteps:\n  - id: hil\n    pytest: {python: "+py+"}\n  - id: after\n    power_on: {}\n"), 0o644)
	go func() {
		time.Sleep(700 * time.Millisecond) // the web app stops the run while pytest runs
		f.lease.MarkLost("the pod lease was stopped by Alice Example during the run")
	}()
	start := time.Now()
	exit, run := f.run()
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("the step ran on after the lease was lost: %s", took)
	}
	if exit.code != ci.ExitInvalid || run.Results.Outcome != ci.Invalid || !run.Health.LeaseLost {
		t.Fatalf("exit %d outcome %s health %+v", exit.code, run.Results.Outcome, run.Health)
	}
	if c := caseByID(run, "tests/test_a.py::test_first"); c == nil || c.Outcome != ci.Passed {
		t.Errorf("the step's cases so far: %+v", run.Results.Cases)
	}
	if c := caseByID(run, "hil"); c == nil || c.Outcome != ci.Errored || !strings.Contains(c.Message, "stopped by Alice Example") {
		t.Errorf("hil %+v", c)
	}
	if c := caseByID(run, "after"); c == nil || c.Outcome != ci.Skipped {
		t.Errorf("after %+v", c)
	}
	if len(run.Health.Reasons) != 1 || run.Health.Reasons[0] != "the pod lease was stopped by Alice Example during the run" {
		t.Errorf("reasons %q", run.Health.Reasons)
	}
	if !strings.Contains(strings.Join(f.pod.cmds(), ","), "dac_stop") {
		t.Errorf("no cleanup: %v", f.pod.cmds())
	}
}
