package ci

import (
	"fmt"
	"strings"
)

// ── baseline gating and the GitHub step summary ─────────────────────────────

// Baseline is the result of comparing a run with its baseline branch.
type Baseline struct {
	Branch  string
	Found   bool   // a baseline run exists
	Message string // why not compared ("" when compared)
	Diff    *Diff
	Gated   []MeasureChange // drifted measurements that fail the run
}

// GateBaseline picks the drifted measurements of d that cfg gates (fail_on_drift, or gate: drift).
func GateBaseline(cfg *Config, d *Diff) []MeasureChange {
	if d == nil || !d.Comparable {
		return nil
	}
	var out []MeasureChange
	for _, m := range d.Measurements {
		if m.Status == "drift" && cfg.GatesDrift(m.Case, m.Name) {
			out = append(out, m)
		}
	}
	return out
}

// Headline is the one line about the baseline compare.
func (b *Baseline) Headline() string {
	if b == nil {
		return ""
	}
	if !b.Found || b.Diff == nil {
		return fmt.Sprintf("Baseline %s: %s", b.Branch, firstNonEmpty(b.Message, "not compared"))
	}
	d := b.Diff
	if !d.Comparable {
		return fmt.Sprintf("Baseline %s (run %s): incomparable (%s)", b.Branch, d.A, strings.Join(d.Reasons, "; "))
	}
	drift, limit := 0, 0
	for _, m := range d.Measurements {
		switch m.Status {
		case "drift":
			drift++
		case "limit":
			limit++
		}
	}
	parts := []string{
		fmt.Sprintf("%d newly failing", len(d.Cases.NewlyFailing)),
		fmt.Sprintf("%d fixed", len(d.Cases.Fixed)),
		fmt.Sprintf("%d drifted", drift),
	}
	if limit > 0 {
		parts = append(parts, fmt.Sprintf("%d outside limits", limit))
	}
	if len(b.Gated) > 0 {
		parts = append(parts, fmt.Sprintf("%d beyond a gated drift", len(b.Gated)))
	}
	if len(d.Slower) > 0 {
		parts = append(parts, fmt.Sprintf("%d slower", len(d.Slower)))
	}
	return fmt.Sprintf("Baseline %s (run %s): %s", b.Branch, d.A, strings.Join(parts, ", "))
}

// StepSummary is the Markdown for $GITHUB_STEP_SUMMARY.
func StepSummary(r *Run, url string, b *Baseline) string {
	var w strings.Builder
	c := r.Results.Counts
	icon := map[string]string{Passed: "passed", Failed: "FAILED", Errored: "ERROR", Invalid: "INVALID"}[r.Results.Outcome]
	fmt.Fprintf(&w, "### benchpod ci: %s on %s: %s\n\n", mdText(r.Key.Suite), mdText(r.Key.DUT), firstNonEmpty(icon, r.Results.Outcome))
	fmt.Fprintf(&w, "%d passed, %d failed, %d error, %d skipped", c.Passed, c.Failed, c.Error, c.Skipped)
	if c.XFailed+c.XPassed > 0 {
		fmt.Fprintf(&w, ", %d xfailed, %d xpassed", c.XFailed, c.XPassed)
	}
	fmt.Fprintf(&w, " in %.1f s", float64(r.Results.DurationMS)/1000)
	if p := r.Inputs.Pod; p.Name != "" {
		fmt.Fprintf(&w, " on %s", mdText(p.Name))
	}
	w.WriteString("\n\n")
	if url != "" {
		fmt.Fprintf(&w, "Run: %s\n\n", url)
	}
	if !r.Health.Valid {
		fmt.Fprintf(&w, "**Not valid:** %s\n\n", mdText(strings.Join(r.Health.Reasons, "; ")))
	}
	if failed := r.FailedCases(); len(failed) > 0 {
		w.WriteString("| Failed case | Outcome | Message |\n|---|---|---|\n")
		for i, fc := range failed {
			if i == 50 {
				fmt.Fprintf(&w, "| ... and %d more | | |\n", len(failed)-50)
				break
			}
			fmt.Fprintf(&w, "| `%s` | %s | %s |\n", mdCode(fc.ID), fc.Outcome, mdText(firstLine(fc.Message)))
		}
		w.WriteString("\n")
	}
	var rows []string
	gated := map[string]bool{}
	if b != nil {
		for _, m := range b.Gated {
			gated[m.Case+"\x00"+m.Name] = true
		}
	}
	for _, cs := range r.Results.Cases {
		for _, m := range cs.Measurements {
			if m.Outcome == Failed {
				rows = append(rows, fmt.Sprintf("| `%s` | %s | %g %s | %s |", mdCode(cs.ID), mdText(m.Name), m.Value, mdText(m.Unit), mdText(m.LimitText())))
			}
		}
	}
	if b != nil && b.Diff != nil {
		for _, m := range b.Diff.Measurements {
			if m.Status != "drift" {
				continue
			}
			ch := ""
			if m.Change != nil {
				ch = fmt.Sprintf("%+.1f%%", *m.Change*100)
			}
			what := "drift vs " + b.Branch + " " + ch
			if gated[m.Case+"\x00"+m.Name] {
				what += " (fails the run)"
			}
			rows = append(rows, fmt.Sprintf("| `%s` | %s | %g %s | %s |", mdCode(m.Case), mdText(m.Name), m.B, mdText(m.Unit), what))
		}
	}
	if len(rows) > 0 {
		w.WriteString("| Case | Measurement | Value | Why |\n|---|---|---|---|\n")
		w.WriteString(strings.Join(rows, "\n"))
		w.WriteString("\n\n")
	}
	if h := b.Headline(); h != "" {
		w.WriteString(mdText(h) + "\n\n")
	}
	return w.String()
}

func mdText(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ", "<", "&lt;", ">", "&gt;").Replace(s)
}

func mdCode(s string) string { return strings.NewReplacer("`", "'", "|", `\|`, "\n", " ").Replace(s) }

// GatedList is Gated, nil-safe.
func (b *Baseline) GatedList() []MeasureChange {
	if b == nil {
		return nil
	}
	return b.Gated
}
