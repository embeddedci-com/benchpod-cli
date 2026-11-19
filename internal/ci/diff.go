package ci

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"

	"strconv"
	"strings"
	"text/tabwriter"
)

// Diff is the comparison of run a (older) with run b (newer), spec section 8.
type Diff struct {
	Found         bool            `json:"found"` // always true locally (the server says false when it has no run to compare with)
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
	ARun          *RunSummary     `json:"a_run"`
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

// Compare diffs a (the older run) against b, with the same rules as embeddedci.com's run compare
// (so a diff reads the same in the terminal and on the run page):
//
//   - Comparable: same key.suite and key.dut, both health.valid, same inputs.wiring.sha256.
//     Otherwise comparable is false, reasons say why, and only inputs_changed is filled.
//   - A different pod, board or connection is compared but flagged.
//   - Cases are matched by id. With repeats a case counts as failing when any attempt failed; its
//     duration and measurements are the last attempt's. Failed before and skipped now is
//     newly_skipped, not fixed.
//   - A measurement is matched by case id + name: "limit" when b is outside its limits, "drift"
//     when the change (b-a)/|a| is beyond the allowed drift (b's, else a's), else "within".
//   - slower: 50 % and 200 ms longer. calls_slower: a command whose p50 over the run (the
//     n-weighted mean of the cases' p50) is 50 % and 2 ms higher.
//   - inputs_changed: every input field that differs, except github.* and wiring.snapshot.
func Compare(a, b *Run) *Diff {
	d := &Diff{Found: true, A: a.RunID, B: b.RunID, Comparable: true, Reasons: []string{}, Flags: []string{},
		InputsChanged: []InputChange{}, Measurements: []MeasureChange{}, Slower: []SlowerCase{}, CallsSlower: []SlowerCall{},
		Cases: DiffCases{NewlyFailing: []string{}, Fixed: []string{}, StillFailing: []string{}, New: []string{}, Removed: []string{}, NewlySkipped: []string{}}}
	sum := summarize(a)
	d.ARun = &sum
	if a.Key.Suite != b.Key.Suite {
		d.Reasons = append(d.Reasons, "different suite ("+a.Key.Suite+" vs "+b.Key.Suite+")")
	}
	if a.Key.DUT != b.Key.DUT {
		d.Reasons = append(d.Reasons, "different DUT ("+a.Key.DUT+" vs "+b.Key.DUT+")")
	}
	if !a.Health.Valid {
		d.Reasons = append(d.Reasons, "run "+a.RunID+" is invalid")
	}
	if !b.Health.Valid {
		d.Reasons = append(d.Reasons, "run "+b.RunID+" is invalid")
	}
	if a.Inputs.Wiring.SHA256 != b.Inputs.Wiring.SHA256 {
		d.Reasons = append(d.Reasons, "different wiring")
	}
	d.Comparable = len(d.Reasons) == 0
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
	d.InputsChanged = inputsChanged(a.inputsJSON(), b.inputsJSON())
	if !d.Comparable {
		return d
	}

	ac, aOrder := caseIndex(a)
	bc, bOrder := caseIndex(b)
	for _, id := range bOrder {
		cb := bc[id]
		ca, ok := ac[id]
		if !ok {
			d.Cases.New = append(d.Cases.New, id)
			continue
		}
		switch {
		case cb.failing && !ca.failing:
			d.Cases.NewlyFailing = append(d.Cases.NewlyFailing, id)
		case cb.failing && ca.failing:
			d.Cases.StillFailing = append(d.Cases.StillFailing, id)
		case ca.failing && cb.skipped:
			d.Cases.NewlySkipped = append(d.Cases.NewlySkipped, id)
		case ca.failing:
			d.Cases.Fixed = append(d.Cases.Fixed, id)
		case cb.skipped && !ca.skipped:
			d.Cases.NewlySkipped = append(d.Cases.NewlySkipped, id)
		}
		da, db := ca.last.DurationMS, cb.last.DurationMS
		if db >= da*slowerFactor && db-da >= slowerCaseMS {
			d.Slower = append(d.Slower, SlowerCase{Case: id, AMS: da, BMS: db})
		}
		d.Measurements = append(d.Measurements, measurementChanges(id, ca.last, cb.last)...)
	}
	for _, id := range aOrder {
		if _, ok := bc[id]; !ok {
			d.Cases.Removed = append(d.Cases.Removed, id)
		}
	}
	sort.SliceStable(d.Measurements, func(i, j int) bool {
		return statusRank(d.Measurements[i].Status) < statusRank(d.Measurements[j].Status)
	})
	d.CallsSlower = callsSlower(a, b)
	return d
}

// Thresholds of "slower".
const (
	slowerFactor = 1.5
	slowerCaseMS = 200
	slowerCallMS = 2
)

func statusRank(s string) int {
	switch s {
	case "limit":
		return 0
	case "drift":
		return 1
	}
	return 2
}

type caseSummary struct {
	failing, skipped bool
	last             *Case
}

// caseIndex groups a run's cases by id, in first-seen order; last is the last attempt.
func caseIndex(r *Run) (map[string]*caseSummary, []string) {
	m := map[string]*caseSummary{}
	var order []string
	for i := range r.Results.Cases {
		c := &r.Results.Cases[i]
		s, ok := m[c.ID]
		if !ok {
			s = &caseSummary{skipped: true}
			m[c.ID] = s
			order = append(order, c.ID)
		}
		if c.Outcome == Failed || c.Outcome == Errored {
			s.failing = true
		}
		if c.Outcome != Skipped {
			s.skipped = false
		}
		if s.last == nil || c.Attempt >= s.last.Attempt {
			s.last = c
		}
	}
	for _, s := range m {
		if s.failing {
			s.skipped = false
		}
	}
	return m, order
}

func measurementChanges(caseID string, a, b *Case) []MeasureChange {
	am := map[string]Measurement{}
	for _, m := range a.Measurements {
		am[m.Name] = m // the last value of a name
	}
	last := map[string]Measurement{}
	for _, m := range b.Measurements {
		last[m.Name] = m
	}
	seen := map[string]bool{}
	var out []MeasureChange
	for _, m := range b.Measurements { // in the order names first appear in b
		if seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		ma, ok := am[m.Name]
		if !ok {
			continue
		}
		mb := last[m.Name]
		mc := MeasureChange{Case: caseID, Name: m.Name, A: ma.Value, B: mb.Value, Unit: mb.Unit, Status: "within"}
		if ma.Value != 0 {
			ch := (mb.Value - ma.Value) / math.Abs(ma.Value)
			mc.Change = &ch
		}
		drift := mb.Drift
		if drift == nil {
			drift = ma.Drift
		}
		switch {
		case mb.Outcome == Failed || outsideLimits(mb):
			mc.Status = "limit"
		case drift != nil && mc.Change != nil && math.Abs(*mc.Change) > *drift:
			mc.Status = "drift"
		case drift != nil && mc.Change == nil && mb.Value != ma.Value:
			mc.Status = "drift"
		}
		out = append(out, mc)
	}
	return out
}

func outsideLimits(m Measurement) bool {
	return (m.Min != nil && m.Value < *m.Min) || (m.Max != nil && m.Value > *m.Max)
}

// callP50 is the n-weighted mean of the cases' p50 per command.
func callP50(r *Run) map[string]float64 {
	sum, n := map[string]float64{}, map[string]int{}
	for _, c := range r.Results.Cases {
		for cmd, st := range c.Calls {
			if st.N <= 0 {
				continue
			}
			sum[cmd] += st.P50MS * float64(st.N)
			n[cmd] += st.N
		}
	}
	out := make(map[string]float64, len(sum))
	for cmd, s := range sum {
		out[cmd] = s / float64(n[cmd])
	}
	return out
}

func callsSlower(a, b *Run) []SlowerCall {
	pa, pb := callP50(a), callP50(b)
	out := []SlowerCall{}
	for cmd, vb := range pb {
		va, ok := pa[cmd]
		if !ok {
			continue
		}
		if vb >= va*slowerFactor && vb-va >= slowerCallMS {
			out = append(out, SlowerCall{Cmd: cmd, AP50MS: roundMS(va), BP50MS: roundMS(vb)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cmd < out[j].Cmd })
	return out
}

func roundMS(v float64) float64 { return math.Round(v*1000) / 1000 }

// ignoredInputs change on every run and say nothing about the result.
var ignoredInputs = map[string]bool{"github": true, "wiring.snapshot": true}

// inputsJSON is the run's inputs object as read (every field, also ones this CLI does not know),
// else as this CLI has them.
func (r *Run) inputsJSON() json.RawMessage {
	if len(r.rawInputs) > 0 {
		return r.rawInputs
	}
	data, _ := json.Marshal(r.Inputs)
	return data
}

// inputsChanged flattens both inputs objects to dotted fields and lists the ones that differ, in
// field order.
func inputsChanged(a, b json.RawMessage) []InputChange {
	fa, fb := flattenJSON(a), flattenJSON(b)
	keys := map[string]bool{}
	for k := range fa {
		keys[k] = true
	}
	for k := range fb {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	out := []InputChange{}
	for _, k := range sorted {
		va, oka := fa[k]
		vb, okb := fb[k]
		if oka && okb && jsonEqual(va, vb) {
			continue
		}
		if isEmptyValue(va) && isEmptyValue(vb) {
			continue
		}
		out = append(out, InputChange{Field: k, A: va, B: vb})
	}
	return out
}

func flattenJSON(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return out
	}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if ignoredInputs[prefix] {
			return
		}
		if m, ok := v.(map[string]any); ok {
			for k, child := range m {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, child)
			}
			return
		}
		if prefix != "" {
			out[prefix] = v
		}
	}
	walk("", v)
	return out
}

func isEmptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	}
	return false
}

func jsonEqual(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// RunSummary is a_run: the run compared with, as embeddedci.com summarizes it (the fields a local
// run has; build_id is the uploaded build it flashed, device_name the pod).
type RunSummary struct {
	ID           string `json:"id"`
	Suite        string `json:"suite"`
	DUT          string `json:"dut"`
	Outcome      string `json:"outcome"`
	Valid        bool   `json:"valid"`
	Trigger      string `json:"trigger"`
	Started      string `json:"started"`
	Finished     string `json:"finished,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	Counts       Counts `json:"counts"`
	BuildID      string `json:"build_id"`
	DeviceID     string `json:"device_id"`
	DeviceName   string `json:"device_name"`
	UploadedBy   string `json:"uploaded_by"`
	TestsRepo    string `json:"tests_repo"`
	TestsRef     string `json:"tests_ref"`
	TestsSHA     string `json:"tests_sha"`
	Firmware     string `json:"firmware"`
	GithubRunURL string `json:"github_run_url"`
}

func summarize(r *Run) RunSummary {
	return RunSummary{ID: r.RunID, Suite: r.Key.Suite, DUT: r.Key.DUT, Outcome: r.Results.Outcome, Valid: r.Health.Valid,
		Trigger: r.Trigger, Started: r.Started, Finished: r.Finished, DurationMS: r.Results.DurationMS, Counts: r.Results.Counts,
		BuildID: r.Inputs.DUTFirmware.BuildID, DeviceName: r.Inputs.Pod.Name, TestsRepo: r.Inputs.Tests.Repo,
		TestsRef: r.Inputs.Tests.Ref, TestsSHA: r.Inputs.Tests.SHA, Firmware: r.Inputs.DUTFirmware.File, GithubRunURL: r.Inputs.GitHub.RunURL}
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
	if !d.Comparable {
		return
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
