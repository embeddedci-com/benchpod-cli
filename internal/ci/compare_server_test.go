package ci

// The compare tests of embeddedci.com's run compare (embeddedci-server internal/testruns),
// ported so `benchpod ci diff` and the run page give the same diff for the same two runs.

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleRun = `{
  "schema_version": 1,
  "run_id": "local-20261118T140211Z-3f9a",
  "key": {"suite": "process-node-hil", "dut": "process-node", "definition_sha256": "abc"},
  "trigger": "manual",
  "started": "2026-11-18T14:02:11.123Z",
  "finished": "2026-11-18T14:09:03.010Z",
  "future_field": {"kept": true},
  "inputs": {
    "dut_firmware": {"file": "process-node.elf", "sha256": "f00d", "size": 123456, "build_id": "", "version": ""},
    "tests": {"repo": "embeddedci-com/examples", "ref": "main", "sha": "s1", "dirty": false},
    "pod": {"name": "pod-a", "board": "analog-v3r2", "firmware": "3.10.8", "gateware": 58, "features_rev": 12, "connection": "lan"},
    "wiring": {"name": "process-node", "sha256": "w1", "snapshot": {"uart_rx": 5}},
    "tools": {"cli": "0.9.2"},
    "github": {"run_id": "1"}
  },
  "health": {"valid": true, "reasons": [], "lost_samples": 0},
  "results": {
    "outcome": "failed",
    "duration_ms": 412000,
    "counts": {"passed": 0, "failed": 0, "error": 0, "skipped": 0, "xfailed": 0, "xpassed": 0},
    "cases": [
      {"id": "boot", "step": "boot", "kind": "native", "outcome": "passed", "duration_ms": 500,
       "measurements": [{"name": "boot_ms", "value": 500, "unit": "ms", "min": null, "max": 600, "drift": 0.2, "outcome": "passed", "source": "native"}],
       "calls": {"power_on": {"n": 1, "p50_ms": 3, "p95_ms": 3, "max_ms": 3, "errors": 0}}},
      {"id": "tests/test_can.py::test_ack", "step": "hil", "kind": "pytest", "outcome": "failed", "duration_ms": 220,
       "message": "assert 0x12 == 0x13", "attempt": 1, "trace": "trace/tests_test_can.py__test_ack.ndjson"},
      {"id": "tests/test_can.py::test_skip", "step": "hil", "kind": "pytest", "outcome": "skipped", "skip_reason": "no DAC"}
    ]
  },
  "keep": {"traces": "failed"}
}`

func mustParse(t *testing.T, s string) *Run {
	t.Helper()
	r, err := ParseRun([]byte(s))
	if err != nil {
		t.Fatalf("ParseRun: %v", err)
	}
	return r
}

// edit returns sampleRun parsed after f changed its generic form.
func edit(t *testing.T, f func(m map[string]any)) *Run {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(sampleRun), &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	b, _ := json.Marshal(m)
	return mustParse(t, string(b))
}

func cases(m map[string]any) []any { return m["results"].(map[string]any)["cases"].([]any) }

func TestCompareIncomparable(t *testing.T) {
	a := mustParse(t, sampleRun)
	b := edit(t, func(m map[string]any) {
		m["key"].(map[string]any)["dut"] = "other-board"
		m["inputs"].(map[string]any)["wiring"].(map[string]any)["sha256"] = "w2"
	})
	d := Compare(a, b)
	if d.Comparable || len(d.Reasons) != 2 {
		t.Fatalf("comparable=%v reasons=%v", d.Comparable, d.Reasons)
	}
	if len(d.Cases.NewlyFailing)+len(d.Measurements) != 0 {
		t.Fatal("incomparable runs list no case changes")
	}
	inv := edit(t, func(m map[string]any) { m["health"].(map[string]any)["valid"] = false })
	if d := Compare(a, inv); d.Comparable || !strings.Contains(strings.Join(d.Reasons, ";"), "invalid") {
		t.Fatalf("invalid run: %+v", d.Reasons)
	}
}

func TestCompareCases(t *testing.T) {
	a := mustParse(t, sampleRun)
	b := edit(t, func(m map[string]any) {
		cs := cases(m)
		boot := cs[0].(map[string]any)
		boot["outcome"] = "failed"                                      // newly failing
		boot["duration_ms"] = 900                                       // +80 %, +400 ms: slower
		boot["measurements"].([]any)[0].(map[string]any)["value"] = 700 // above max 600: limit
		boot["calls"] = map[string]any{"power_on": map[string]any{"n": 1, "p50_ms": 9, "p95_ms": 9, "max_ms": 9, "errors": 0}}
		cs[1].(map[string]any)["outcome"] = "passed"                                                // fixed
		cs = append(cs[:2], map[string]any{"id": "tests/test_new.py::test_x", "outcome": "passed"}) // skip removed, one new
		m["results"].(map[string]any)["cases"] = cs
		pod := m["inputs"].(map[string]any)["pod"].(map[string]any)
		pod["firmware"] = "3.10.11"
		pod["name"] = "pod-b"
	})
	d := Compare(a, b)
	if !d.Comparable {
		t.Fatalf("reasons %v", d.Reasons)
	}
	eq := func(name string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	eq("newly_failing", d.Cases.NewlyFailing, "boot")
	eq("fixed", d.Cases.Fixed, "tests/test_can.py::test_ack")
	eq("new", d.Cases.New, "tests/test_new.py::test_x")
	eq("removed", d.Cases.Removed, "tests/test_can.py::test_skip")
	eq("flags", d.Flags, "pod differs")
	if len(d.Slower) != 1 || d.Slower[0].Case != "boot" || d.Slower[0].BMS != 900 {
		t.Errorf("slower = %+v", d.Slower)
	}
	if len(d.Measurements) != 1 || d.Measurements[0].Status != "limit" || d.Measurements[0].Change == nil || *d.Measurements[0].Change != 0.4 {
		t.Errorf("measurements = %+v", d.Measurements)
	}
	if len(d.CallsSlower) != 1 || d.CallsSlower[0].Cmd != "power_on" {
		t.Errorf("calls_slower = %+v", d.CallsSlower)
	}
	fields := map[string]bool{}
	for _, c := range d.InputsChanged {
		fields[c.Field] = true
	}
	if !fields["pod.firmware"] || !fields["pod.name"] || fields["github.run_id"] || fields["wiring.snapshot.uart_rx"] {
		t.Errorf("inputs_changed = %+v", d.InputsChanged)
	}
}

func TestCompareDriftAndRepeats(t *testing.T) {
	a := mustParse(t, sampleRun)
	b := edit(t, func(m map[string]any) {
		cs := cases(m)
		boot := cs[0].(map[string]any)
		boot["measurements"].([]any)[0].(map[string]any)["value"] = 590 // +18 %: within 20 %, under max
		ack := cs[1].(map[string]any)
		ack["outcome"] = "passed"
		// a repeat of test_ack that failed: the case is still failing
		retry := map[string]any{"id": "tests/test_can.py::test_ack", "outcome": "failed", "attempt": 2}
		skip := cs[2].(map[string]any)
		m["results"].(map[string]any)["cases"] = []any{boot, ack, retry, skip}
	})
	d := Compare(a, b)
	if len(d.Measurements) != 1 || d.Measurements[0].Status != "within" {
		t.Fatalf("measurements = %+v", d.Measurements)
	}
	if strings.Join(d.Cases.StillFailing, ",") != "tests/test_can.py::test_ack" {
		t.Fatalf("still_failing = %v", d.Cases.StillFailing)
	}
	c := edit(t, func(m map[string]any) {
		boot := cases(m)[0].(map[string]any)
		boot["measurements"].([]any)[0].(map[string]any)["value"] = 400 // -20 %... and a bit
		boot["measurements"].([]any)[0].(map[string]any)["drift"] = 0.1
	})
	if d := Compare(a, c); len(d.Measurements) != 1 || d.Measurements[0].Status != "drift" {
		t.Fatalf("drift: %+v", d.Measurements)
	}
	sk := edit(t, func(m map[string]any) { cases(m)[0].(map[string]any)["outcome"] = "skipped" })
	if d := Compare(a, sk); strings.Join(d.Cases.NewlySkipped, ",") != "boot" {
		t.Fatalf("newly_skipped = %v", d.Cases.NewlySkipped)
	}
}

func TestCompareJSONShape(t *testing.T) {
	a := mustParse(t, sampleRun)
	d := Compare(a, a)
	data, _ := json.Marshal(d)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	for _, k := range []string{"found", "a", "b", "comparable", "reasons", "flags", "inputs_changed", "cases", "measurements", "slower", "calls_slower", "a_run"} {
		if _, ok := m[k]; !ok {
			t.Errorf("diff JSON lacks %q: %s", k, data)
		}
	}
	if m["found"] != true || m["a_run"].(map[string]any)["suite"] != "process-node-hil" {
		t.Errorf("found / a_run: %s", data)
	}
	if !strings.Contains(string(data), `"newly_failing":[]`) {
		t.Errorf("empty lists are []: %s", data)
	}
}

func TestCompareFailedThenSkippedIsNewlySkipped(t *testing.T) {
	a := mustParse(t, sampleRun) // test_ack failed in a
	b := edit(t, func(m map[string]any) { cases(m)[1].(map[string]any)["outcome"] = "skipped" })
	d := Compare(a, b)
	if strings.Join(d.Cases.NewlySkipped, ",") != "tests/test_can.py::test_ack" || len(d.Cases.Fixed) != 0 {
		t.Fatalf("newly_skipped %v fixed %v", d.Cases.NewlySkipped, d.Cases.Fixed)
	}
	inc := edit(t, func(m map[string]any) {
		m["key"].(map[string]any)["suite"] = "other"
		m["inputs"].(map[string]any)["pod"].(map[string]any)["firmware"] = "3.10.12"
	})
	d = Compare(a, inc)
	if d.Comparable || len(d.InputsChanged) != 1 || d.InputsChanged[0].Field != "pod.firmware" || len(d.Cases.Removed)+len(d.Slower) != 0 {
		t.Fatalf("incomparable: %+v", d)
	}
}
