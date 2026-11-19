package ci

import (
	"strings"
	"testing"
)

func TestGateConfig(t *testing.T) {
	cfg, err := Parse([]byte(`version: 1
suite: s
dut: d
baseline: main
gate: drift
steps:
  - id: boot
    power_cycle: {until: OK}
    expect: {boot_ms: {drift: 20%, fail_on_drift: false}}
  - id: idle
    measure_power: {}
    expect: {mean_ma: {drift: 10%}}
  - id: hil
    pytest: tests
    expect: {"t.py::test_a/settle_ms": {drift: 15%, fail_on_drift: true}}
`), "f.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		caseID, name string
		want         bool
	}{{"boot", "boot_ms", false}, {"idle", "mean_ma", true}, {"t.py::test_a", "settle_ms", true}, {"t.py::other", "x", true}} {
		if got := cfg.GatesDrift(c.caseID, c.name); got != c.want {
			t.Errorf("GatesDrift(%s, %s) = %v", c.caseID, c.name, got)
		}
	}
	cfg.Gate = ""
	if cfg.GatesDrift("idle", "mean_ma") || !cfg.GatesDrift("t.py::test_a", "settle_ms") {
		t.Error("without gate: only fail_on_drift gates")
	}
	for yaml, want := range map[string]string{
		"version: 1\nsuite: s\ndut: d\ngate: always\nsteps: [{wait: 1s}]\n":                                           "gate must be drift",
		"version: 1\nsuite: s\ndut: d\nsteps:\n  - measure_power: {}\n    expect: {mean_ma: {fail_on_drift: true}}\n": "fail_on_drift needs a drift allowance",
	} {
		if _, err := Parse([]byte(yaml), "f.yaml", noEnv); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v, want %q", err, want)
		}
	}
}

func TestStepSummaryAndGate(t *testing.T) {
	ch := 0.3
	d := &Diff{Found: true, A: "r-base", Comparable: true, Measurements: []MeasureChange{
		{Case: "idle", Name: "mean_ma", A: 10, B: 13, Unit: "mA", Change: &ch, Status: "drift"},
		{Case: "boot", Name: "boot_ms", A: 500, B: 510, Unit: "ms", Status: "within"},
	}, Cases: DiffCases{NewlyFailing: []string{"t::x"}}}
	cfg := &Config{Gate: "drift"}
	b := &Baseline{Branch: "main", Found: true, Diff: d, Gated: GateBaseline(cfg, d)}
	if len(b.Gated) != 1 || b.Gated[0].Name != "mean_ma" {
		t.Fatalf("gated %+v", b.Gated)
	}
	max := 600.0
	r := &Run{Key: Key{Suite: "node|hil", DUT: "node"}, Health: Health{Valid: true}, Inputs: Inputs{Pod: Pod{Name: "benchpod-a1b2c3"}},
		Results: Results{Outcome: Failed, DurationMS: 4200, Counts: Counts{Passed: 3, Failed: 1}, Cases: []Case{
			{ID: "t::x", Outcome: Failed, Message: "assert 1 == 2\nmore"},
			{ID: "boot", Outcome: Passed, Measurements: []Measurement{{Name: "boot_ms", Value: 700, Unit: "ms", Max: &max, Outcome: Failed}}},
		}}}
	md := StepSummary(r, "https://example.test/run/r-1", b)
	for _, want := range []string{"### benchpod ci: node\\|hil on node: FAILED", "3 passed, 1 failed, 0 error, 0 skipped in 4.2 s on benchpod-a1b2c3",
		"Run: https://example.test/run/r-1", "| `t::x` | failed | assert 1 == 2 |", "boot_ms = 700 ms, outside max 600",
		"drift vs main +30.0% (fails the run)", "Baseline main (run r-base): 1 newly failing, 0 fixed, 1 drifted, 1 beyond a gated drift"} {
		if !strings.Contains(md, want) {
			t.Errorf("summary lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(md, "more") {
		t.Errorf("only the first line of a message: %s", md)
	}
	var nb *Baseline
	if nb.Headline() != "" || StepSummary(r, "", nil) == "" {
		t.Error("nil baseline")
	}
	if h := (&Baseline{Branch: "main", Message: "no earlier valid run"}).Headline(); h != "Baseline main: no earlier valid run" {
		t.Error(h)
	}
}
