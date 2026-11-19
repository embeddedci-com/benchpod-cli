package ci

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fp(v float64) *float64 { return &v }

func TestRunOutcomeAndExitCodes(t *testing.T) {
	cases := []struct {
		valid    bool
		counts   Counts
		required bool
		want     string
		exit     int
	}{
		{true, Counts{Passed: 3}, false, Passed, 0},
		{true, Counts{Passed: 3, Skipped: 2, XFailed: 1}, false, Passed, 0},
		{true, Counts{Passed: 3, Failed: 1}, false, Failed, 2},
		{true, Counts{Passed: 3}, true, Failed, 2},
		{true, Counts{Failed: 1, Error: 1}, false, Errored, 2},
		{false, Counts{Passed: 3}, false, Invalid, 4},
		{false, Counts{Error: 3}, false, Invalid, 4},
	}
	for _, c := range cases {
		got := RunOutcome(c.valid, c.counts, c.required)
		if got != c.want || ExitCode(got) != c.exit {
			t.Errorf("RunOutcome(%v, %+v, %v) = %s (exit %d), want %s (exit %d)", c.valid, c.counts, c.required, got, ExitCode(got), c.want, c.exit)
		}
	}
}

func TestFinishCountsAndNormalizes(t *testing.T) {
	ms := make([]Measurement, MaxMeasurementsPerCase+5)
	for i := range ms {
		ms[i] = Measurement{Name: "m", Value: float64(i)}
	}
	r := &Run{Health: Health{Valid: true}, Results: Results{Cases: []Case{
		{ID: "a", Outcome: Passed, Measurements: ms},
		{ID: "b", Outcome: "weird"},
		{ID: "c", Outcome: Skipped, Message: strings.Repeat("x", MaxMessageBytes+10)},
	}}}
	r.Finish(false)
	if r.Results.Counts.Passed != 1 || r.Results.Counts.Error != 1 || r.Results.Counts.Skipped != 1 {
		t.Errorf("counts %+v", r.Results.Counts)
	}
	if r.Results.Outcome != Errored {
		t.Errorf("outcome %s", r.Results.Outcome)
	}
	if r.Health.MeasurementsDropped != 5 || len(r.Results.Cases[0].Measurements) != MaxMeasurementsPerCase {
		t.Errorf("dropped %d", r.Health.MeasurementsDropped)
	}
	if len(r.Results.Cases[2].Message) > MaxMessageBytes {
		t.Errorf("message not capped")
	}
	if r.Results.Cases[1].Calls == nil || r.Results.Cases[1].Measurements == nil || r.Results.Cases[1].Attempt != 1 {
		t.Errorf("not normalized: %+v", r.Results.Cases[1])
	}
	data, _ := json.Marshal(r)
	for _, want := range []string{`"reasons":[]`, `"calls":{}`, `"measurements":[]`, `"snapshot":{}`, `"schema_version":1`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("run.json lacks %s", want)
		}
	}
}

func TestMeasurementLimits(t *testing.T) {
	m := Measurement{Name: "boot_ms", Value: 700, Unit: "ms"}
	ApplyExpect(&m, Expect{Max: fp(600), Drift: fp(0.2)})
	if m.Outcome != Failed || *m.Drift != 0.2 {
		t.Fatalf("%+v", m)
	}
	c := Case{ID: "boot", Outcome: Passed, Measurements: []Measurement{m}}
	FailOnLimits(&c)
	if c.Outcome != Failed || !strings.Contains(c.Message, "boot_ms = 700 ms, outside max 600") {
		t.Errorf("%+v", c)
	}
	r := Measurement{Value: 1}
	r.Evaluate()
	if r.Outcome != "recorded" {
		t.Errorf("no limits: %s", r.Outcome)
	}
	in := Measurement{Value: 10, Min: fp(5), Max: fp(20)}
	in.Evaluate()
	if in.Outcome != Passed {
		t.Errorf("within: %s", in.Outcome)
	}
}

func TestCaseSlug(t *testing.T) {
	if got := CaseSlug("boot"); got != "boot" {
		t.Errorf("plain: %s", got)
	}
	got := CaseSlug("tests/test_can_hil.py::test_ack")
	if !strings.HasPrefix(got, "tests_test_can_hil.py__test_ack-") || len(got) != len("tests_test_can_hil.py__test_ack-")+8 {
		t.Errorf("changed: %s", got)
	}
	long := CaseSlug(strings.Repeat("a", 200))
	if len(long) != 129 {
		t.Errorf("long: %d", len(long))
	}
}

func TestWriteAndReadRun(t *testing.T) {
	dir := t.TempDir()
	r := &Run{RunID: NewRunID(time.Now()), Key: Key{Suite: "s", DUT: "d"}, Health: Health{Valid: true}}
	r.Finish(false)
	if err := WriteJSON(filepath.Join(dir, "run.json"), r); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRun(dir)
	if err != nil || got.RunID != r.RunID || got.Results.Outcome != Passed {
		t.Fatalf("%+v %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover temp files: %v", entries)
	}
	if !strings.HasPrefix(r.RunID, "local-") || len(r.RunID) != len("local-20261118T140211Z-3f9a") {
		t.Errorf("run id %s", r.RunID)
	}
}

func TestCanonicalSHA(t *testing.T) {
	a := CanonicalSHA256(map[string]any{"b": 1, "a": map[string]any{"y": 2, "x": 1}})
	b := CanonicalSHA256(map[string]any{"a": map[string]any{"x": 1, "y": 2}, "b": 1})
	if a == "" || a != b {
		t.Errorf("%s != %s", a, b)
	}
	if CanonicalSHA256(map[string]any{}) != "" || CanonicalSHA256(nil) != "" {
		t.Error("empty should be \"\"")
	}
}

const junitXML = `<?xml version="1.0" encoding="utf-8"?>
<testsuites><testsuite name="pytest" tests="5">
 <testcase classname="tests.test_can" name="test_ok" time="0.120"/>
 <testcase classname="tests.test_can.TestBus" name="test_ack" time="0.220">
   <failure message="assert 0x12 == 0x13">long traceback</failure>
   <system-out>captured</system-out>
 </testcase>
 <testcase classname="tests.test_can" name="test_err" time="1"><error message="fixture failed"/></testcase>
 <testcase classname="tests.test_can" name="test_skip" time="0"><skipped message="no DAC on this board"/></testcase>
 <testcase classname="tests.test_can" name="test_xf" time="0"><skipped type="pytest.xfail" message="known"/></testcase>
 <testcase classname="tests.test_can" name="test_prop" time="0.01">
   <properties><property name="settle_ms" value="840"/><property name="note" value="text"/></properties>
 </testcase>
</testsuite></testsuites>`

func TestParseJUnit(t *testing.T) {
	cases, err := ParseJUnit([]byte(junitXML), "hil", "junit")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 6 {
		t.Fatalf("%d cases", len(cases))
	}
	want := []struct{ id, outcome string }{
		{"tests.test_can::test_ok", Passed}, {"tests.test_can.TestBus::test_ack", Failed}, {"tests.test_can::test_err", Errored},
		{"tests.test_can::test_skip", Skipped}, {"tests.test_can::test_xf", XFailed}, {"tests.test_can::test_prop", Passed},
	}
	for i, w := range want {
		if cases[i].ID != w.id || cases[i].Outcome != w.outcome || cases[i].Step != "hil" || cases[i].Kind != "junit" {
			t.Errorf("case %d = %+v, want %+v", i, cases[i], w)
		}
	}
	if cases[0].DurationMS != 120 || cases[2].DurationMS != 1000 {
		t.Errorf("durations %v %v", cases[0].DurationMS, cases[2].DurationMS)
	}
	if !strings.Contains(cases[1].Message, "assert 0x12 == 0x13") || !strings.Contains(cases[1].Message, "long traceback") || cases[1].Log != "captured" {
		t.Errorf("failure %+v", cases[1])
	}
	if cases[3].SkipReason != "no DAC on this board" {
		t.Errorf("skip reason %q", cases[3].SkipReason)
	}
	if m := cases[5].Measurements; len(m) != 1 || m[0].Name != "settle_ms" || m[0].Value != 840 || m[0].Source != "junit" {
		t.Errorf("properties %+v", m)
	}
	py, _ := ParseJUnit([]byte(junitXML), "hil", "pytest")
	if py[1].ID != "tests/test_can.py::TestBus::test_ack" || py[0].ID != "tests/test_can.py::test_ok" {
		t.Errorf("pytest ids %s %s", py[0].ID, py[1].ID)
	}
	single, err := ParseJUnit([]byte(`<testsuite name="x"><testcase name="t1"/></testsuite>`), "s", "junit")
	if err != nil || len(single) != 1 || single[0].ID != "t1" {
		t.Errorf("testsuite root: %+v %v", single, err)
	}
	if _, err := ParseJUnit([]byte("not xml <"), "s", "junit"); err == nil {
		t.Error("bad xml accepted")
	}
}

func TestWriteJUnitRoundTrip(t *testing.T) {
	in := []Case{
		{ID: "boot", Step: "boot", Outcome: Passed, DurationMS: 812, Measurements: []Measurement{{Name: "boot_ms", Value: 640}}},
		{ID: "t::x", Step: "hil", Outcome: Failed, Message: "boom\nline 2", Attempt: 2},
		{ID: "t::s", Step: "hil", Outcome: Skipped, SkipReason: "why"},
		{ID: "t::e", Step: "hil", Outcome: Errored, Message: "err"},
	}
	var buf bytes.Buffer
	if err := WriteJUnit(&buf, "suite", in, 5000); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`<testsuite name="suite" tests="4" failures="1" errors="1" skipped="1" time="5.000">`, `name="t::x [attempt 2]"`, `<property name="boot_ms" value="640">`} {
		if !strings.Contains(out, want) {
			t.Errorf("junit lacks %s:\n%s", want, out)
		}
	}
	back, err := ParseJUnit(buf.Bytes(), "", "junit")
	if err != nil || len(back) != 4 || back[1].Outcome != Failed || back[2].Outcome != Skipped || back[3].Outcome != Errored {
		t.Errorf("round trip %+v %v", back, err)
	}
}

const goTestJSON = `{"Action":"start","Package":"x/hwe2e"}
{"Action":"run","Package":"x/hwe2e","Test":"TestCloud"}
{"Action":"output","Package":"x/hwe2e","Test":"TestCloud","Output":"=== RUN   TestCloud\n"}
{"Action":"run","Package":"x/hwe2e","Test":"TestCloud/status"}
{"Action":"output","Package":"x/hwe2e","Test":"TestCloud/status","Output":"    cloud_test.go:40: status: got 500\n"}
{"Action":"output","Package":"x/hwe2e","Test":"TestCloud/status","Output":"--- FAIL: TestCloud/status (0.30s)\n"}
{"Action":"fail","Package":"x/hwe2e","Test":"TestCloud/status","Elapsed":0.3}
{"Action":"fail","Package":"x/hwe2e","Test":"TestCloud","Elapsed":1.25}
{"Action":"run","Package":"x/hwe2e","Test":"TestPing"}
{"Action":"pass","Package":"x/hwe2e","Test":"TestPing","Elapsed":0.01}
{"Action":"run","Package":"x/hwe2e","Test":"TestNoPod"}
{"Action":"output","Package":"x/hwe2e","Test":"TestNoPod","Output":"    hw_test.go:12: needs BENCHPOD_HW_ADDR\n"}
{"Action":"skip","Package":"x/hwe2e","Test":"TestNoPod","Elapsed":0}
{"Action":"run","Package":"x/hwe2e","Test":"TestHang"}
{"Action":"output","Package":"x/hwe2e","Test":"TestHang","Output":"panic: test timed out\n"}
{"Action":"fail","Package":"x/hwe2e","Elapsed":2}
not json
{"Action":"output","Package":"x/broken","Output":"broken.go:3:1: syntax error\n"}
{"Action":"fail","Package":"x/broken","Elapsed":0}
`

func TestParseGoTestJSON(t *testing.T) {
	cases := ParseGoTestJSON([]byte(goTestJSON), "cloud-api")
	got := map[string]Case{}
	var ids []string
	for _, c := range cases {
		got[c.ID] = c
		ids = append(ids, c.ID)
		if c.Kind != "gotest" || c.Step != "cloud-api" {
			t.Errorf("kind/step %+v", c)
		}
	}
	if strings.Join(ids, ",") != "x/hwe2e/TestCloud,x/hwe2e/TestCloud/status,x/hwe2e/TestPing,x/hwe2e/TestNoPod,x/hwe2e/TestHang,x/broken" {
		t.Fatalf("ids %v", ids)
	}
	if c := got["x/hwe2e/TestCloud/status"]; c.Outcome != Failed || c.Message != "cloud_test.go:40: status: got 500" || c.DurationMS != 300 {
		t.Errorf("status %+v", c)
	}
	if got["x/hwe2e/TestPing"].Outcome != Passed {
		t.Error("ping")
	}
	if c := got["x/hwe2e/TestNoPod"]; c.Outcome != Skipped || c.SkipReason != "needs BENCHPOD_HW_ADDR" {
		t.Errorf("skip %+v", c)
	}
	if c := got["x/hwe2e/TestHang"]; c.Outcome != Errored || !strings.Contains(c.Message, "timed out") {
		t.Errorf("hang %+v", c)
	}
	if c := got["x/broken"]; c.Outcome != Errored || !strings.Contains(c.Message, "syntax error") {
		t.Errorf("build failure %+v", c)
	}
}

func TestMergePart(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "parts"), 0o755)
	part := `{"schema_version":1,"step":"hil","kind":"pytest","cases":[
	  {"id":"t::a","outcome":"passed","duration_ms":5},
	  {"id":"t::b","step":"hil","kind":"pytest","outcome":"failed","message":"x","attempt":1}],
	 "health":{"lost_samples":0,"busy_retries":2,"reconnects":1,"measurements_dropped":3,"trace_truncated":true,"reasons":["slow pod"]},
	 "tools":{"sdk":"3.0.2","python":"3.12.4","pytest":"8.3.2"},
	 "pod":{"name":"benchpod-a1b2c3","board":"analog-v3r2","firmware":"3.10.11","connection":"lan"}}`
	os.WriteFile(PartPath(dir, "hil"), []byte(part), 0o644)
	p, ok, err := ReadPart(dir, "hil")
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, ok, _ := ReadPart(dir, "nope"); ok {
		t.Error("missing part read")
	}
	r := &Run{Health: Health{Valid: true, BusyRetries: 1, Reasons: []string{"first"}}, Inputs: Inputs{Tools: Tools{CLI: "0.9.3", SDK: ""}}}
	r.Results.Cases = []Case{{ID: "boot", Step: "boot", Outcome: Passed}}
	r.MergePart(p, "hil", 2)
	cs := r.Results.Cases
	if len(cs) != 3 || cs[1].Step != "hil" || cs[1].Kind != "pytest" || cs[1].Attempt != 2 || cs[2].Attempt != 1 {
		t.Errorf("cases %+v", cs)
	}
	h := r.Health
	if h.BusyRetries != 3 || h.Reconnects != 1 || h.MeasurementsDropped != 3 || !h.TraceTruncated || strings.Join(h.Reasons, "|") != "first|slow pod" || !h.Valid {
		t.Errorf("health %+v", h)
	}
	if r.Inputs.Tools.CLI != "0.9.3" || r.Inputs.Tools.SDK != "3.0.2" || r.Inputs.Tools.Pytest != "8.3.2" {
		t.Errorf("tools %+v", r.Inputs.Tools)
	}
	if r.Inputs.Pod.Name != "benchpod-a1b2c3" {
		t.Errorf("pod %+v", r.Inputs.Pod)
	}
	// A pod block the runner already has wins; lost samples invalidate.
	r2 := &Run{Health: Health{Valid: true}, Inputs: Inputs{Pod: Pod{Name: "mine"}}}
	p.Health.LostSamples = 7
	r2.MergePart(p, "hil", 1)
	if r2.Inputs.Pod.Name != "mine" || r2.Health.Valid || r2.Health.LostSamples != 7 {
		t.Errorf("r2 %+v %+v", r2.Inputs.Pod, r2.Health)
	}
}

func TestRecorder(t *testing.T) {
	dir := t.TempDir()
	rec := NewRecorder(dir, time.Now(), "failed")
	rec.Begin()
	rec.Record("command", "status", map[string]any{"cmd": "status"}, json.RawMessage(`{"version":"3.10.11"}`), 3*time.Millisecond, nil)
	rec.Record("command", "status", map[string]any{"cmd": "status"}, nil, 5*time.Millisecond, nil)
	rec.Record("command", "wifi_set", map[string]any{"cmd": "wifi_set", "ssid": "x", "password": "hunter2", "nested": map[string]any{"Token": "t", "key": "short"}, "key": strings.Repeat("k", 20)}, nil, time.Millisecond, os.ErrDeadlineExceeded)
	big := strings.Repeat("y", MaxInlineJSON+10)
	rec.Record("command", "wiring_get", map[string]any{"cmd": "wiring_get"}, map[string]any{"data": big}, time.Millisecond, nil)
	c := Case{ID: "tests/x.py::t", Outcome: Failed, Attempt: 1}
	if err := rec.End(&c); err != nil {
		t.Fatal(err)
	}
	if st := c.Calls["status"]; st.N != 2 || st.P50MS != 3 || st.MaxMS != 5 || st.Errors != 0 {
		t.Errorf("status stats %+v", st)
	}
	if c.Calls["wifi_set"].Errors != 1 {
		t.Errorf("errors %+v", c.Calls)
	}
	if c.Trace == "" {
		t.Fatal("failed case trace not kept")
	}
	data, err := os.ReadFile(filepath.Join(dir, c.Trace))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "hunter2") || strings.Contains(text, `"Token":"t"`) || strings.Contains(text, strings.Repeat("k", 20)) {
		t.Errorf("secret in trace:\n%s", text)
	}
	if !strings.Contains(text, `"key":"short"`) || !strings.Contains(text, `"att":"att/cli-0001-wiring_get.json"`) {
		t.Errorf("trace:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "att", "cli-0001-wiring_get.json")); err != nil {
		t.Errorf("attachment: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var first map[string]any
	json.Unmarshal([]byte(lines[0]), &first)
	if first["op"] != "command" || first["cmd"] != "status" || first["ok"] != true || first["ms"].(float64) != 3 {
		t.Errorf("line %v", first)
	}
	// A passing case is not kept under "failed", but its calls are.
	rec.Begin()
	rec.Record("command", "ping", nil, nil, time.Millisecond, nil)
	ok := Case{ID: "boot", Outcome: Passed}
	rec.End(&ok)
	if ok.Trace != "" || ok.Calls["ping"].N != 1 {
		t.Errorf("passing case %+v", ok)
	}
}

func TestPercentile(t *testing.T) {
	s := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if percentile(s, 0.5) != 5 || percentile(s, 0.95) != 10 || percentile([]float64{4}, 0.95) != 4 {
		t.Error("percentile")
	}
}

func baseRun(id string) *Run {
	return &Run{RunID: id, Key: Key{Suite: "s", DUT: "d"}, Health: Health{Valid: true},
		Inputs: Inputs{Pod: Pod{Name: "p1", Board: "analog-v3r2", Firmware: "3.10.8", Connection: "lan"}, Wiring: Wiring{SHA256: "w"}}}
}

func TestDiff(t *testing.T) {
	a, b := baseRun("A"), baseRun("B")
	a.Results.Cases = []Case{
		{ID: "boot", Outcome: Passed, DurationMS: 800, Measurements: []Measurement{{Name: "boot_ms", Value: 500, Unit: "ms", Drift: fp(0.2), Outcome: Passed}},
			Calls: map[string]CallStats{"status": {N: 2, P50MS: 3}}},
		{ID: "t::fix", Outcome: Failed},
		{ID: "t::still", Outcome: Failed},
		{ID: "t::break", Outcome: Passed, DurationMS: 100},
		{ID: "t::gone", Outcome: Passed},
		{ID: "t::skip", Outcome: Passed},
		{ID: "t::m", Outcome: Passed, Measurements: []Measurement{{Name: "x", Value: 10}, {Name: "y", Value: 0}}},
	}
	b.Inputs.Pod.Firmware = "3.10.11"
	b.Inputs.Pod.Name = "p2"
	b.Results.Cases = []Case{
		{ID: "boot", Outcome: Passed, DurationMS: 1300, Measurements: []Measurement{{Name: "boot_ms", Value: 650, Unit: "ms", Outcome: Passed}},
			Calls: map[string]CallStats{"status": {N: 2, P50MS: 6}}},
		{ID: "t::fix", Outcome: Passed},
		{ID: "t::still", Outcome: Errored},
		{ID: "t::break", Outcome: Passed, Attempt: 1},
		{ID: "t::break", Outcome: Failed, Attempt: 2, DurationMS: 400},
		{ID: "t::new", Outcome: Passed},
		{ID: "t::skip", Outcome: Skipped},
		{ID: "t::m", Outcome: Failed, Measurements: []Measurement{{Name: "x", Value: 30, Outcome: Failed}, {Name: "y", Value: 1}}},
	}
	d := Compare(a, b)
	if !d.Comparable || strings.Join(d.Flags, ",") != "pod differs" {
		t.Fatalf("comparable %v flags %v reasons %v", d.Comparable, d.Flags, d.Reasons)
	}
	if len(d.InputsChanged) != 2 || d.InputsChanged[0].Field != "pod.firmware" || d.InputsChanged[1].Field != "pod.name" {
		t.Errorf("inputs %+v", d.InputsChanged)
	}
	c := d.Cases
	check := func(name string, got []string, want string) {
		if strings.Join(got, ",") != want {
			t.Errorf("%s = %v, want %s", name, got, want)
		}
	}
	check("newly_failing", c.NewlyFailing, "t::break,t::m")
	check("fixed", c.Fixed, "t::fix")
	check("still_failing", c.StillFailing, "t::still")
	check("new", c.New, "t::new")
	check("removed", c.Removed, "t::gone")
	check("newly_skipped", c.NewlySkipped, "t::skip")
	if len(d.Measurements) != 3 {
		t.Fatalf("measurements %+v", d.Measurements)
	}
	// Sorted limit, drift, within (as embeddedci.com does).
	if m := d.Measurements[0]; m.Name != "x" || m.Status != "limit" || *m.Change != 2 {
		t.Errorf("x %+v", m)
	}
	if m := d.Measurements[1]; m.Name != "boot_ms" || m.Status != "drift" || math.Abs(*m.Change-0.3) > 1e-9 {
		t.Errorf("boot_ms %+v", m)
	}
	if m := d.Measurements[2]; m.Change != nil || m.Status != "within" {
		t.Errorf("y %+v", m)
	}
	if len(d.Slower) != 2 || d.Slower[0].Case != "boot" || d.Slower[1].Case != "t::break" {
		t.Errorf("slower %+v", d.Slower)
	}
	if len(d.CallsSlower) != 1 || d.CallsSlower[0].Cmd != "status" {
		t.Errorf("calls %+v", d.CallsSlower)
	}
	if !d.Regressed() {
		t.Error("regressed")
	}
	var buf bytes.Buffer
	d.WriteText(&buf)
	for _, want := range []string{"Newly failing (2)", "pod.firmware", "+30.0%", "Slower pod calls"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("text lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestDiffIncomparable(t *testing.T) {
	a, b := baseRun("A"), baseRun("B")
	b.Key.DUT = "other"
	b.Health.Valid = false
	b.Health.Reasons = []string{"pod rebooted during the run"}
	b.Inputs.Wiring.SHA256 = "x"
	d := Compare(a, b)
	if d.Comparable || len(d.Reasons) != 3 {
		t.Fatalf("%+v", d)
	}
	data, _ := json.Marshal(d)
	if !bytes.Contains(data, []byte(`"newly_failing":[]`)) || !bytes.Contains(data, []byte(`"comparable":false`)) {
		t.Errorf("json %s", data)
	}
	var buf bytes.Buffer
	d.WriteText(&buf)
	if !strings.Contains(buf.String(), "incomparable") {
		t.Errorf("%s", buf.String())
	}
}
