package ci

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ── reading JUnit XML ───────────────────────────────────────────────────────

type junitSuites struct {
	XMLName xml.Name
	Suites  []junitSuite `xml:"testsuite"`
	Cases   []junitCase  `xml:"testcase"` // <testsuite> as the root
}

type junitSuite struct {
	Name   string       `xml:"name,attr"`
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

type junitCase struct {
	ClassName  string          `xml:"classname,attr"`
	Name       string          `xml:"name,attr"`
	Time       string          `xml:"time,attr"`
	Failure    *junitResult    `xml:"failure"`
	Error      *junitResult    `xml:"error"`
	Skipped    *junitResult    `xml:"skipped"`
	SystemOut  string          `xml:"system-out"`
	SystemErr  string          `xml:"system-err"`
	Properties []junitProperty `xml:"properties>property"`
}

type junitResult struct {
	Type    string `xml:"type,attr"`
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

// ParseJUnit reads the cases of a JUnit XML file (<testsuites> or <testsuite> root). Each case's
// id is classname::name (just name without a classname); numeric <properties> become
// measurements with source "junit". kind is the case kind to record ("junit", or "pytest" for
// pytest's own --junitxml).
func ParseJUnit(data []byte, step, kind string) ([]Case, error) {
	var root junitSuites
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse JUnit XML: %w", err)
	}
	var raw []junitCase
	raw = append(raw, root.Cases...)
	var walk func(s []junitSuite)
	walk = func(s []junitSuite) {
		for _, su := range s {
			raw = append(raw, su.Cases...)
			walk(su.Suites)
		}
	}
	walk(root.Suites)
	out := make([]Case, 0, len(raw))
	for _, jc := range raw {
		c := Case{Step: step, Kind: kind, Outcome: Passed, Attempt: 1}
		switch {
		case jc.ClassName != "" && jc.Name != "":
			c.ID = jc.ClassName + "::" + jc.Name
		case jc.Name != "":
			c.ID = jc.Name
		default:
			c.ID = jc.ClassName
		}
		if kind == "pytest" {
			c.ID = pytestNodeID(jc.ClassName, jc.Name)
		}
		if t, err := strconv.ParseFloat(strings.TrimSpace(jc.Time), 64); err == nil {
			c.DurationMS = round3(t * 1000)
		}
		switch {
		case jc.Error != nil:
			c.Outcome = Errored
			c.Message = resultText(jc.Error)
		case jc.Failure != nil:
			c.Outcome = Failed
			c.Message = resultText(jc.Failure)
		case jc.Skipped != nil:
			c.Outcome = Skipped
			c.SkipReason = strings.TrimSpace(firstNonEmpty(jc.Skipped.Message, jc.Skipped.Body))
			// pytest writes an expected failure as a skip of type pytest.xfail.
			if jc.Skipped.Type == "pytest.xfail" {
				c.Outcome = XFailed
			}
		}
		if c.Failing() {
			c.Log = CapTail(strings.TrimSpace(jc.SystemOut+"\n"+jc.SystemErr), MaxCaseLogBytes)
		}
		for _, p := range jc.Properties {
			if v, err := strconv.ParseFloat(strings.TrimSpace(p.Value), 64); err == nil {
				c.Measurements = append(c.Measurements, Measurement{Name: p.Name, Value: v, Outcome: "recorded", Source: "junit"})
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// pytestNodeID turns pytest's JUnit classname ("tests.test_can.TestBus") and name back into a
// node id ("tests/test_can.py::TestBus::test_ack") as well as JUnit allows: the module path is
// taken up to the first part that starts with "Test" (a class). It is a best effort; the pytest
// plugin's parts file has the exact node ids.
func pytestNodeID(classname, name string) string {
	if classname == "" {
		return name
	}
	parts := strings.Split(classname, ".")
	mod := len(parts)
	for i, p := range parts {
		if strings.HasPrefix(p, "Test") {
			mod = i
			break
		}
	}
	id := strings.Join(parts[:mod], "/") + ".py"
	for _, cls := range parts[mod:] {
		id += "::" + cls
	}
	return id + "::" + name
}

func resultText(r *junitResult) string {
	msg := strings.TrimSpace(r.Message)
	body := strings.TrimSpace(r.Body)
	switch {
	case msg == "":
		return Cap(body, MaxMessageBytes)
	case body == "" || strings.Contains(body, msg):
		return Cap(firstNonEmpty(body, msg), MaxMessageBytes)
	}
	return Cap(msg+"\n"+body, MaxMessageBytes)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}

// ── writing junit.xml ───────────────────────────────────────────────────────

type outSuite struct {
	XMLName  xml.Name  `xml:"testsuite"`
	Name     string    `xml:"name,attr"`
	Tests    int       `xml:"tests,attr"`
	Failures int       `xml:"failures,attr"`
	Errors   int       `xml:"errors,attr"`
	Skipped  int       `xml:"skipped,attr"`
	Time     string    `xml:"time,attr"`
	Cases    []outCase `xml:"testcase"`
}

type outCase struct {
	ClassName  string        `xml:"classname,attr"`
	Name       string        `xml:"name,attr"`
	Time       string        `xml:"time,attr"`
	Properties *outProps     `xml:"properties,omitempty"`
	Failure    *outResult    `xml:"failure,omitempty"`
	Error      *outResult    `xml:"error,omitempty"`
	Skipped    *outResult    `xml:"skipped,omitempty"`
	SystemOut  *outCharacter `xml:"system-out,omitempty"`
}

type outProps struct {
	Props []junitProperty `xml:"property"`
}

type outResult struct {
	Message string `xml:"message,attr,omitempty"`
	Body    string `xml:",chardata"`
}

type outCharacter struct {
	Body string `xml:",chardata"`
}

// WriteJUnit writes the cases as one <testsuite name="<suite>">. classname is the step id, name
// the case id; an attempt past the first is named "<id> [attempt N]".
func WriteJUnit(w io.Writer, suite string, cases []Case, durationMS int64) error {
	s := outSuite{Name: suite, Time: secs(float64(durationMS))}
	for _, c := range cases {
		oc := outCase{ClassName: c.Step, Name: c.ID, Time: secs(c.DurationMS)}
		if c.Attempt > 1 {
			oc.Name = fmt.Sprintf("%s [attempt %d]", c.ID, c.Attempt)
		}
		for _, m := range c.Measurements {
			if oc.Properties == nil {
				oc.Properties = &outProps{}
			}
			oc.Properties.Props = append(oc.Properties.Props, junitProperty{Name: m.Name, Value: strconv.FormatFloat(m.Value, 'g', -1, 64)})
		}
		s.Tests++
		switch c.Outcome {
		case Failed:
			s.Failures++
			oc.Failure = &outResult{Message: firstLine(c.Message), Body: c.Message}
		case Errored:
			s.Errors++
			oc.Error = &outResult{Message: firstLine(c.Message), Body: c.Message}
		case Skipped:
			s.Skipped++
			oc.Skipped = &outResult{Message: c.SkipReason}
		case XFailed:
			s.Skipped++
			oc.Skipped = &outResult{Message: "xfail: " + firstNonEmpty(c.SkipReason, c.Message)}
		}
		if c.Log != "" {
			oc.SystemOut = &outCharacter{Body: c.Log}
		}
		s.Cases = append(s.Cases, oc)
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

func secs(ms float64) string { return strconv.FormatFloat(ms/1000, 'f', 3, 64) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
