package ci

import (
	"fmt"
	"io"
	"math"

	"strconv"
	"strings"
	"text/tabwriter"
)

// Diff is the comparison of run a (older) with run b (newer), spec section 8.
type Diff struct {
	A             string          `json:"a"`
	B             string          `json:"b"`
	Comparable    bool            `json:"comparable"`
	Reasons       []string        `json:"reasons"`
	Flags         []string        `json:"flags"`
	InputsChanged []InputChange   `json:"inputs_changed"`
	Cases         DiffCases       `json:"cases"`
	Measurements  []MeasureChange `json:"measurements"`
	Slower        []SlowerCase    `json:"slower"`
	CallsSlower   []SlowerCall    `json:"calls_slower"`
}

// InputChange is one input that differs.
type InputChange struct {
	Field string `json:"field"`
	A     any    `json:"a"`
	B     any    `json:"b"`
}

// DiffCases are case ids grouped by how they changed.
type DiffCases struct {
	NewlyFailing []string `json:"newly_failing"`
	Fixed        []string `json:"fixed"`
	StillFailing []string `json:"still_failing"`
	New          []string `json:"new"`
	Removed      []string `json:"removed"`
	NewlySkipped []string `json:"newly_skipped"`
}

// MeasureChange is one measurement in both runs.
type MeasureChange struct {
	Case   string   `json:"case"`
	Name   string   `json:"name"`
	A      float64  `json:"a"`
	B      float64  `json:"b"`
	Unit   string   `json:"unit"`
	Change *float64 `json:"change"` // (b-a)/|a|; null when a is 0
	Status string   `json:"status"` // within | drift | limit
}

// SlowerCase is a case that took 50 % and 200 ms longer.
type SlowerCase struct {
	Case string  `json:"case"`
	AMS  float64 `json:"a_ms"`
	BMS  float64 `json:"b_ms"`
}

// SlowerCall is a pod command whose median latency went up 50 % and 2 ms.
type SlowerCall struct {
	Cmd    string  `json:"cmd"`
	AP50MS float64 `json:"a_p50_ms"`
	BP50MS float64 `json:"b_p50_ms"`
}

// Compare diffs a (the older run) against b.
func Compare(a, b *Run) *Diff {
	d := &Diff{A: a.RunID, B: b.RunID, Comparable: true, Reasons: []string{}, Flags: []string{},
		InputsChanged: []InputChange{}, Measurements: []MeasureChange{}, Slower: []SlowerCase{}, CallsSlower: []SlowerCall{},
		Cases: DiffCases{NewlyFailing: []string{}, Fixed: []string{}, StillFailing: []string{}, New: []string{}, Removed: []string{}, NewlySkipped: []string{}}}
	if a.Key.Suite != b.Key.Suite {
		d.Reasons = append(d.Reasons, fmt.Sprintf("different suites (%s, %s)", a.Key.Suite, b.Key.Suite))
	}
	if a.Key.DUT != b.Key.DUT {
		d.Reasons = append(d.Reasons, fmt.Sprintf("different DUTs (%s, %s)", a.Key.DUT, b.Key.DUT))
	}
	if !a.Health.Valid {
		d.Reasons = append(d.Reasons, a.RunID+" is not valid: "+strings.Join(a.Health.Reasons, "; "))
	}
	if !b.Health.Valid {
		d.Reasons = append(d.Reasons, b.RunID+" is not valid: "+strings.Join(b.Health.Reasons, "; "))
	}
	if a.Inputs.Wiring.SHA256 != b.Inputs.Wiring.SHA256 {
		d.Reasons = append(d.Reasons, "the wiring differs")
	}
	if len(d.Reasons) > 0 {
		d.Comparable = false
		return d
	}
	pa, pb := a.Inputs.Pod, b.Inputs.Pod
	if pa.Name != pb.Name {
		d.Flags = append(d.Flags, "pod differs")
	}
	if pa.Board != pb.Board {
		d.Flags = append(d.Flags, "board differs")
	}
	if pa.Connection != pb.Connection {
		d.Flags = append(d.Flags, "connection differs")
	}
	d.InputsChanged = inputChanges(a.Inputs, b.Inputs)

	ca, cb := caseSummary(a.Results.Cases), caseSummary(b.Results.Cases)
	for _, id := range ca.order {
		x := ca.byID[id]
		y, ok := cb.byID[id]
		if !ok {
			d.Cases.Removed = append(d.Cases.Removed, id)
			continue
		}
		switch {
		case !x.failing && y.failing:
			d.Cases.NewlyFailing = append(d.Cases.NewlyFailing, id)
		case x.failing && y.failing:
			d.Cases.StillFailing = append(d.Cases.StillFailing, id)
		case x.failing && !y.failing && !y.skipped:
			d.Cases.Fixed = append(d.Cases.Fixed, id)
		}
		if !x.skipped && y.skipped {
			d.Cases.NewlySkipped = append(d.Cases.NewlySkipped, id)
		}
		if y.ms >= x.ms*1.5 && y.ms-x.ms >= 200 {
			d.Slower = append(d.Slower, SlowerCase{Case: id, AMS: x.ms, BMS: y.ms})
		}
		for _, name := range x.mOrder {
			ma := x.m[name]
			mb, ok := y.m[name]
			if !ok {
				continue
			}
			mc := MeasureChange{Case: id, Name: name, A: ma.Value, B: mb.Value, Unit: firstNonEmpty(mb.Unit, ma.Unit), Status: "within"}
			if ma.Value != 0 {
				ch := (mb.Value - ma.Value) / math.Abs(ma.Value)
				ch = math.Round(ch*1e6) / 1e6
				mc.Change = &ch
			}
			drift := mb.Drift
			if drift == nil {
				drift = ma.Drift
			}
			switch {
			case mb.Outcome == Failed:
				mc.Status = "limit"
			case drift != nil && (mc.Change == nil && mb.Value != ma.Value || mc.Change != nil && math.Abs(*mc.Change) > *drift):
				mc.Status = "drift"
			}
			d.Measurements = append(d.Measurements, mc)
		}
	}
	for _, id := range cb.order {
		if _, ok := ca.byID[id]; !ok {
			d.Cases.New = append(d.Cases.New, id)
		}
	}
	callsA, callsB := callMedians(a.Results.Cases), callMedians(b.Results.Cases)
	for _, cmd := range sortedKeys(callsA) {
		x := callsA[cmd]
		y, ok := callsB[cmd]
		if ok && y >= x*1.5 && y-x >= 2 {
			d.CallsSlower = append(d.CallsSlower, SlowerCall{Cmd: cmd, AP50MS: round3(x), BP50MS: round3(y)})
		}
	}
	return d
}

type caseInfo struct {
	failing, skipped bool
	ms               float64
	m                map[string]Measurement
	mOrder           []string
}

type caseSet struct {
	order []string
	byID  map[string]*caseInfo
}

// caseSummary folds repeated attempts into one entry per id: failing when any attempt failed,
// skipped when every attempt was, the longest duration, the last attempt's measurements.
func caseSummary(cases []Case) caseSet {
	s := caseSet{byID: map[string]*caseInfo{}}
	for _, c := range cases {
		ci := s.byID[c.ID]
		if ci == nil {
			ci = &caseInfo{skipped: true, m: map[string]Measurement{}}
			s.byID[c.ID] = ci
			s.order = append(s.order, c.ID)
		}
		ci.failing = ci.failing || c.Failing()
		ci.skipped = ci.skipped && c.Outcome == Skipped
		ci.ms = math.Max(ci.ms, c.DurationMS)
		for _, m := range c.Measurements {
			if _, seen := ci.m[m.Name]; !seen {
				ci.mOrder = append(ci.mOrder, m.Name)
			}
			ci.m[m.Name] = m
		}
	}
	return s
}

// callMedians is each command's median latency over the run: the per-case medians weighted by
// their call counts.
func callMedians(cases []Case) map[string]float64 {
	sum, n := map[string]float64{}, map[string]int{}
	for _, c := range cases {
		for cmd, st := range c.Calls {
			if st.N == 0 {
				continue
			}
			sum[cmd] += st.P50MS * float64(st.N)
			n[cmd] += st.N
		}
	}
	out := map[string]float64{}
	for cmd, total := range sum {
		out[cmd] = total / float64(n[cmd])
	}
	return out
}

func inputChanges(a, b Inputs) []InputChange {
	type field struct {
		name string
		a, b any
	}
	fields := []field{
		{"dut_firmware.sha256", a.DUTFirmware.SHA256, b.DUTFirmware.SHA256},
		{"dut_firmware.build_id", a.DUTFirmware.BuildID, b.DUTFirmware.BuildID},
		{"dut_firmware.version", a.DUTFirmware.Version, b.DUTFirmware.Version},
		{"tests.repo", a.Tests.Repo, b.Tests.Repo},
		{"tests.ref", a.Tests.Ref, b.Tests.Ref},
		{"tests.sha", a.Tests.SHA, b.Tests.SHA},
		{"tests.dirty", a.Tests.Dirty, b.Tests.Dirty},
		{"pod.name", a.Pod.Name, b.Pod.Name},
		{"pod.board", a.Pod.Board, b.Pod.Board},
		{"pod.firmware", a.Pod.Firmware, b.Pod.Firmware},
		{"pod.gateware", a.Pod.Gateware, b.Pod.Gateware},
		{"pod.features_rev", a.Pod.FeaturesRev, b.Pod.FeaturesRev},
		{"pod.calibration_sha256", a.Pod.CalibrationSHA256, b.Pod.CalibrationSHA256},
		{"pod.connection", a.Pod.Connection, b.Pod.Connection},
		{"tools.cli", a.Tools.CLI, b.Tools.CLI},
		{"tools.sdk", a.Tools.SDK, b.Tools.SDK},
		{"tools.python", a.Tools.Python, b.Tools.Python},
		{"tools.pytest", a.Tools.Pytest, b.Tools.Pytest},
		{"tools.os", a.Tools.OS, b.Tools.OS},
	}
	out := []InputChange{}
	for _, f := range fields {
		if f.a != f.b {
			out = append(out, InputChange{Field: f.name, A: f.a, B: f.b})
		}
	}
	return out
}

// Regressed says b is worse than a: a case newly failing or a measurement newly outside its limit.
func (d *Diff) Regressed() bool {
	if !d.Comparable {
		return false
	}
	if len(d.Cases.NewlyFailing) > 0 {
		return true
	}
	for _, m := range d.Measurements {
		if m.Status == "limit" {
			return true
		}
	}
	return false
}

// WriteText prints the diff as tables for a terminal.
func (d *Diff) WriteText(w io.Writer) {
	fmt.Fprintf(w, "a: %s\nb: %s\n", d.A, d.B)
	if !d.Comparable {
		fmt.Fprintln(w, "incomparable:")
		for _, r := range d.Reasons {
			fmt.Fprintf(w, "  - %s\n", r)
		}
		return
	}
	if len(d.Flags) > 0 {
		fmt.Fprintf(w, "note: %s\n", strings.Join(d.Flags, ", "))
	}
	if len(d.InputsChanged) > 0 {
		fmt.Fprintln(w, "\nInputs changed:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  FIELD\tA\tB")
		for _, c := range d.InputsChanged {
			fmt.Fprintf(tw, "  %s\t%v\t%v\n", c.Field, dash(c.A), dash(c.B))
		}
		tw.Flush()
	}
	groups := []struct {
		title string
		ids   []string
	}{
		{"Newly failing", d.Cases.NewlyFailing}, {"Fixed", d.Cases.Fixed}, {"Still failing", d.Cases.StillFailing},
		{"New", d.Cases.New}, {"Removed", d.Cases.Removed}, {"Newly skipped", d.Cases.NewlySkipped},
	}
	fmt.Fprintln(w, "\nCases:")
	any := false
	for _, g := range groups {
		if len(g.ids) == 0 {
			continue
		}
		any = true
		fmt.Fprintf(w, "  %s (%d):\n", g.title, len(g.ids))
		for _, id := range g.ids {
			fmt.Fprintf(w, "    %s\n", id)
		}
	}
	if !any {
		fmt.Fprintln(w, "  no changes")
	}
	if len(d.Measurements) > 0 {
		fmt.Fprintln(w, "\nMeasurements:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  CASE\tNAME\tA\tB\tCHANGE\tSTATUS")
		for _, m := range d.Measurements {
			ch := "-"
			if m.Change != nil {
				ch = fmt.Sprintf("%+.1f%%", *m.Change*100)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", m.Case, m.Name, num(m.A, m.Unit), num(m.B, m.Unit), ch, m.Status)
		}
		tw.Flush()
	}
	if len(d.Slower) > 0 {
		fmt.Fprintln(w, "\nSlower cases:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  CASE\tA\tB")
		for _, s := range d.Slower {
			fmt.Fprintf(tw, "  %s\t%s ms\t%s ms\n", s.Case, num(s.AMS, ""), num(s.BMS, ""))
		}
		tw.Flush()
	}
	if len(d.CallsSlower) > 0 {
		fmt.Fprintln(w, "\nSlower pod calls (median):")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  COMMAND\tA\tB")
		for _, s := range d.CallsSlower {
			fmt.Fprintf(tw, "  %s\t%s ms\t%s ms\n", s.Cmd, num(s.AP50MS, ""), num(s.BP50MS, ""))
		}
		tw.Flush()
	}
}

func dash(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return "-"
	}
	return v
}

func num(v float64, unit string) string {
	s := strconv.FormatFloat(v, 'g', 6, 64)
	if unit != "" {
		s += " " + unit
	}
	return s
}
