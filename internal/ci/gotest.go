package ci

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// goTestEvent is one line of `go test -json` (test2json).
type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
	Output  string  `json:"Output"`
}

// ParseGoTestJSON reads `go test -json` output into cases: one per test (id Package/Test,
// subtests included), plus one per package that failed without a failing test (a build error or
// a panic outside a test), whose message is the package output. Lines that are not JSON are
// ignored.
func ParseGoTestJSON(data []byte, step string) []Case {
	type acc struct {
		c      Case
		out    strings.Builder
		closed bool
	}
	var order []string
	tests := map[string]*acc{}
	pkgOut := map[string]*strings.Builder{}
	pkgFailed := map[string]bool{}
	pkgHasFail := map[string]bool{}
	var pkgOrder []string

	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev goTestEvent
		if json.Unmarshal(line, &ev) != nil || ev.Action == "" {
			continue
		}
		if ev.Test == "" {
			if _, ok := pkgOut[ev.Package]; !ok {
				pkgOut[ev.Package] = &strings.Builder{}
				pkgOrder = append(pkgOrder, ev.Package)
			}
			switch ev.Action {
			case "output":
				if pkgOut[ev.Package].Len() < 1<<20 {
					pkgOut[ev.Package].WriteString(ev.Output)
				}
			case "fail":
				pkgFailed[ev.Package] = true
			}
			continue
		}
		id := ev.Package + "/" + ev.Test
		if ev.Package == "" {
			id = ev.Test
		}
		a := tests[id]
		if a == nil {
			a = &acc{c: Case{ID: id, Step: step, Kind: "gotest", Outcome: Passed, Attempt: 1}}
			tests[id] = a
			order = append(order, id)
		}
		switch ev.Action {
		case "output":
			if a.out.Len() < 1<<20 {
				a.out.WriteString(ev.Output)
			}
		case "pass":
			a.c.Outcome, a.closed = Passed, true
			a.c.DurationMS = round3(ev.Elapsed * 1000)
		case "fail":
			a.c.Outcome, a.closed = Failed, true
			a.c.DurationMS = round3(ev.Elapsed * 1000)
			pkgHasFail[ev.Package] = true
		case "skip":
			a.c.Outcome, a.closed = Skipped, true
			a.c.DurationMS = round3(ev.Elapsed * 1000)
		}
	}
	out := make([]Case, 0, len(order))
	for _, id := range order {
		a := tests[id]
		text := a.out.String()
		switch {
		case !a.closed:
			// Started but never ended: the binary died (panic, timeout) while it ran.
			a.c.Outcome = Errored
			a.c.Message = Cap(goFailureText(text), MaxMessageBytes)
			a.c.Log = CapTail(text, MaxCaseLogBytes)
		case a.c.Outcome == Failed:
			a.c.Message = Cap(goFailureText(text), MaxMessageBytes)
			a.c.Log = CapTail(text, MaxCaseLogBytes)
		case a.c.Outcome == Skipped:
			a.c.SkipReason = goSkipReason(text)
		}
		out = append(out, a.c)
	}
	for _, pkg := range pkgOrder {
		if pkgFailed[pkg] && !pkgHasFail[pkg] {
			text := pkgOut[pkg].String()
			out = append(out, Case{ID: pkg, Step: step, Kind: "gotest", Outcome: Errored, Attempt: 1,
				Message: Cap(strings.TrimSpace(text), MaxMessageBytes), Log: CapTail(text, MaxCaseLogBytes)})
		}
	}
	return out
}

// goFailureText is a test's output without the === RUN / --- FAIL framing lines.
func goFailureText(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "=== ") || strings.HasPrefix(t, "--- PASS") || strings.HasPrefix(t, "--- FAIL") {
			continue
		}
		keep = append(keep, strings.TrimRight(l, " \r"))
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// goSkipReason is the text a skipped test logged (t.Skip's arguments).
func goSkipReason(out string) string {
	for _, l := range strings.Split(out, "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "=== ") || strings.HasPrefix(t, "--- SKIP") {
			continue
		}
		// "    hw_test.go:12: needs a pod" -> "needs a pod"
		if i := strings.Index(t, ": "); i > 0 && strings.Contains(t[:i], ".go:") {
			t = t[i+2:]
		}
		return t
	}
	return ""
}
