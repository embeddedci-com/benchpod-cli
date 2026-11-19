package ci

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is run.json's schema_version.
const SchemaVersion = 1

// Case outcomes.
const (
	Passed  = "passed"
	Failed  = "failed"
	Errored = "error"
	Skipped = "skipped"
	XFailed = "xfailed"
	XPassed = "xpassed"
)

// Run outcomes: passed, failed and error as for cases, plus invalid.
const Invalid = "invalid"

// Caps from the spec.
const (
	MaxMessageBytes        = 16 << 10
	MaxCaseLogBytes        = 64 << 10
	MaxMeasurementsPerCase = 200
)

// Run is run.json (schema_version 1).
type Run struct {
	SchemaVersion int     `json:"schema_version"`
	RunID         string  `json:"run_id"`
	Key           Key     `json:"key"`
	Trigger       string  `json:"trigger"`
	Started       string  `json:"started"`
	Finished      string  `json:"finished"`
	Inputs        Inputs  `json:"inputs"`
	Health        Health  `json:"health"`
	Results       Results `json:"results"`
	Keep          Keep    `json:"keep"`
	// Uploaded is where the run is on embeddedci.com, once uploaded (run_id stays the local id).
	Uploaded *Uploaded `json:"uploaded,omitempty"`

	rawInputs json.RawMessage // the inputs object as read (ReadRun), for Compare
}

// Key is what runs are stored and compared under.
type Key struct {
	Suite            string `json:"suite"`
	DUT              string `json:"dut"`
	DefinitionSHA256 string `json:"definition_sha256"`
}

// Inputs is everything the run ran with.
type Inputs struct {
	DUTFirmware DUTFirmware `json:"dut_firmware"`
	Tests       Tests       `json:"tests"`
	Pod         Pod         `json:"pod"`
	Wiring      Wiring      `json:"wiring"`
	Tools       Tools       `json:"tools"`
	GitHub      GitHub      `json:"github"`
}

// DUTFirmware is the image flashed into the DUT.
type DUTFirmware struct {
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	BuildID string `json:"build_id"`
	Version string `json:"version"`
}

// Tests is the git state of the tests' working directory.
type Tests struct {
	Repo  string `json:"repo"`
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Dirty bool   `json:"dirty"`
}

// Pod is the pod the run used.
type Pod struct {
	Name              string `json:"name"`
	Board             string `json:"board"`
	Firmware          string `json:"firmware"`
	Gateware          int    `json:"gateware"`
	FeaturesRev       int    `json:"features_rev"`
	CalibrationSHA256 string `json:"calibration_sha256"`
	Connection        string `json:"connection"` // lan | usb | usb_net | cloud | serial
}

// IsZero says nothing is known about the pod.
func (p Pod) IsZero() bool { return p == Pod{} }

// Wiring is the stored wiring profile the run used.
type Wiring struct {
	Name     string         `json:"name"`
	SHA256   string         `json:"sha256"`
	Snapshot map[string]any `json:"snapshot"`
}

// Tools are the versions of what ran.
type Tools struct {
	CLI    string `json:"cli"`
	SDK    string `json:"sdk"`
	Python string `json:"python"`
	Pytest string `json:"pytest"`
	OS     string `json:"os"`
}

// Merge fills t's empty fields from o.
func (t *Tools) Merge(o Tools) {
	fill := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	fill(&t.CLI, o.CLI)
	fill(&t.SDK, o.SDK)
	fill(&t.Python, o.Python)
	fill(&t.Pytest, o.Pytest)
	fill(&t.OS, o.OS)
}

// GitHub is the Actions run, when the run is one.
type GitHub struct {
	RunID      string `json:"run_id"`
	RunAttempt string `json:"run_attempt"`
	RunURL     string `json:"run_url"`
	Workflow   string `json:"workflow"`
	Actor      string `json:"actor"`
}

// Health says whether the run can be trusted.
type Health struct {
	Valid               bool     `json:"valid"`
	Reasons             []string `json:"reasons"`
	LostSamples         int      `json:"lost_samples"`
	RateMisses          int      `json:"rate_misses"`
	BusyRetries         int      `json:"busy_retries"`
	Reconnects          int      `json:"reconnects"`
	LeaseLost           bool     `json:"lease_lost"`
	MeasurementsDropped int      `json:"measurements_dropped"`
	TraceTruncated      bool     `json:"trace_truncated"`
}

// Invalidate marks the run invalid for reason (once per reason).
func (h *Health) Invalidate(reason string) {
	h.Valid = false
	for _, r := range h.Reasons {
		if r == reason {
			return
		}
	}
	h.Reasons = append(h.Reasons, reason)
}

// Results are the cases and their totals.
type Results struct {
	Outcome    string `json:"outcome"`
	DurationMS int64  `json:"duration_ms"`
	Counts     Counts `json:"counts"`
	Cases      []Case `json:"cases"`
}

// Counts are cases per outcome.
type Counts struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Error   int `json:"error"`
	Skipped int `json:"skipped"`
	XFailed int `json:"xfailed"`
	XPassed int `json:"xpassed"`
}

// Total is the number of cases.
func (c Counts) Total() int { return c.Passed + c.Failed + c.Error + c.Skipped + c.XFailed + c.XPassed }

// Keep is the keep policy that was applied.
type Keep struct {
	Traces string `json:"traces"`
}

// Case is one test result.
type Case struct {
	ID           string               `json:"id"`
	Step         string               `json:"step"`
	Kind         string               `json:"kind"` // native | pytest | junit | gotest
	Outcome      string               `json:"outcome"`
	DurationMS   float64              `json:"duration_ms"`
	Message      string               `json:"message"`
	SkipReason   string               `json:"skip_reason"`
	Attempt      int                  `json:"attempt"`
	Trace        string               `json:"trace"`
	Log          string               `json:"log"`
	Measurements []Measurement        `json:"measurements"`
	Calls        map[string]CallStats `json:"calls"`
}

// Failing says the case failed or errored.
func (c *Case) Failing() bool { return c.Outcome == Failed || c.Outcome == Errored }

// Measurement is one named value of a case.
type Measurement struct {
	Name    string   `json:"name"`
	Value   float64  `json:"value"`
	Unit    string   `json:"unit"`
	Min     *float64 `json:"min"`
	Max     *float64 `json:"max"`
	Drift   *float64 `json:"drift"`
	Outcome string   `json:"outcome"` // passed | failed | recorded
	Source  string   `json:"source"`  // measure | native | record_property | junit | openhtf
}

// Evaluate sets the outcome from the limits: failed outside min/max, passed within, recorded
// without limits.
func (m *Measurement) Evaluate() {
	switch {
	case m.Min == nil && m.Max == nil:
		m.Outcome = "recorded"
	case (m.Min != nil && m.Value < *m.Min) || (m.Max != nil && m.Value > *m.Max):
		m.Outcome = Failed
	default:
		m.Outcome = Passed
	}
}

// LimitText is "840 ms not within max 600" for a failed measurement.
func (m *Measurement) LimitText() string {
	var lim []string
	if m.Min != nil {
		lim = append(lim, fmt.Sprintf("min %g", *m.Min))
	}
	if m.Max != nil {
		lim = append(lim, fmt.Sprintf("max %g", *m.Max))
	}
	unit := ""
	if m.Unit != "" {
		unit = " " + m.Unit
	}
	return fmt.Sprintf("%s = %g%s, outside %s", m.Name, m.Value, unit, strings.Join(lim, ", "))
}

// CallStats are one pod command's call counts and latencies within a case.
type CallStats struct {
	N      int     `json:"n"`
	P50MS  float64 `json:"p50_ms"`
	P95MS  float64 `json:"p95_ms"`
	MaxMS  float64 `json:"max_ms"`
	Errors int     `json:"errors"`
}

// ApplyExpect sets e's limits on m (from a YAML expect) and re-evaluates it.
func ApplyExpect(m *Measurement, e Expect) {
	if e.Min != nil {
		m.Min = e.Min
	}
	if e.Max != nil {
		m.Max = e.Max
	}
	if e.Drift != nil {
		m.Drift = e.Drift
	}
	m.Evaluate()
}

// FailOnLimits fails a passed case whose measurements are outside their limits, with the list in
// its message.
func FailOnLimits(c *Case) {
	var failed []string
	for i := range c.Measurements {
		if c.Measurements[i].Outcome == Failed {
			failed = append(failed, c.Measurements[i].LimitText())
		}
	}
	if len(failed) == 0 {
		return
	}
	if c.Outcome == Passed || c.Outcome == XPassed {
		c.Outcome = Failed
	}
	msg := "measurement limits: " + strings.Join(failed, "; ")
	if c.Message == "" {
		c.Message = msg
	} else if !strings.Contains(c.Message, msg) {
		c.Message += "\n" + msg
	}
	c.Message = Cap(c.Message, MaxMessageBytes)
}

// Normalize fills nil slices and maps (so run.json always has [] and {}), caps the message and
// log, and caps the measurements, counting what it drops.
func (c *Case) Normalize() (dropped int) {
	if c.Measurements == nil {
		c.Measurements = []Measurement{}
	}
	if len(c.Measurements) > MaxMeasurementsPerCase {
		dropped = len(c.Measurements) - MaxMeasurementsPerCase
		c.Measurements = c.Measurements[:MaxMeasurementsPerCase]
	}
	for i := range c.Measurements {
		m := &c.Measurements[i]
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
			m.Value = 0
		}
		if m.Outcome == "" {
			m.Evaluate()
		}
	}
	if c.Calls == nil {
		c.Calls = map[string]CallStats{}
	}
	if c.Attempt == 0 {
		c.Attempt = 1
	}
	c.Message = Cap(c.Message, MaxMessageBytes)
	c.Log = CapTail(c.Log, MaxCaseLogBytes)
	return dropped
}

// Cap keeps the first max bytes of s (on a rune boundary), marking the cut.
func Cap(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "\n[cut]"
	cut := max - len(mark)
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + mark
}

// CapTail keeps the last max bytes of s, marking the cut.
func CapTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "[cut]\n"
	start := len(s) - (max - len(mark))
	for start < len(s) && !utf8Start(s[start]) {
		start++
	}
	return mark + s[start:]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// Finish computes the counts and the outcome, and normalizes every case.
func (r *Run) Finish(requiredFailed bool) {
	r.SchemaVersion = SchemaVersion
	if r.Health.Reasons == nil {
		r.Health.Reasons = []string{}
	}
	if r.Results.Cases == nil {
		r.Results.Cases = []Case{}
	}
	if r.Inputs.Wiring.Snapshot == nil {
		r.Inputs.Wiring.Snapshot = map[string]any{}
	}
	var c Counts
	for i := range r.Results.Cases {
		cs := &r.Results.Cases[i]
		r.Health.MeasurementsDropped += cs.Normalize()
		switch cs.Outcome {
		case Passed:
			c.Passed++
		case Failed:
			c.Failed++
		case Errored:
			c.Error++
		case Skipped:
			c.Skipped++
		case XFailed:
			c.XFailed++
		case XPassed:
			c.XPassed++
		default:
			cs.Outcome = Errored
			c.Error++
		}
	}
	r.Results.Counts = c
	r.Results.Outcome = RunOutcome(r.Health.Valid, c, requiredFailed)
}

// RunOutcome: invalid when the health is not valid; else error when any case errored; else
// failed when any case failed or a required step failed; else passed.
func RunOutcome(valid bool, c Counts, requiredFailed bool) string {
	switch {
	case !valid:
		return Invalid
	case c.Error > 0:
		return Errored
	case c.Failed > 0 || requiredFailed:
		return Failed
	}
	return Passed
}

// Exit codes of `benchpod ci` (spec section 6).
const (
	ExitPassed     = 0
	ExitSetup      = 1 // file, usage or setup error
	ExitFailed     = 2 // a test failed (or errored)
	ExitRegression = 3 // drift beyond fail_on_drift against the baseline (later)
	ExitInvalid    = 4 // the run cannot be trusted: require not met, pod or lease lost, pod busy
)

// ExitCode is the exit code for a run outcome.
func ExitCode(outcome string) int {
	switch outcome {
	case Passed:
		return ExitPassed
	case Failed, Errored:
		return ExitFailed
	case Invalid:
		return ExitInvalid
	}
	return ExitSetup
}

// WriteJSON writes v as indented JSON to path atomically (a temporary file, then a rename).
func WriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'))
}

// WriteFileAtomic writes data to path through a temporary file in the same directory.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// ReadRun reads run.json from a run folder or the file itself.
func ReadRun(path string) (*Run, error) {
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "run.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := ParseRun(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// ParseRun reads a run.json document.
func ParseRun(data []byte) (*Run, error) {
	var r Run
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	var raw struct {
		Inputs json.RawMessage `json:"inputs"`
		Health struct {
			Valid *bool `json:"valid"`
		} `json:"health"`
	}
	if json.Unmarshal(data, &raw) == nil {
		r.rawInputs = raw.Inputs
		if raw.Health.Valid == nil { // a run without the field counts as valid, as on embeddedci.com
			r.Health.Valid = true
		}
	}
	if r.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("schema_version %d is not supported (this CLI reads %d)", r.SchemaVersion, SchemaVersion)
	}
	return &r, nil
}

// CaseSlug is the file name of a case's trace: the id with every character outside
// [A-Za-z0-9._-] replaced by "_", at most 120 characters, plus "-<8 hex of sha1(id)>" when it
// was cut or changed.
func CaseSlug(id string) string {
	var b strings.Builder
	changed := false
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
			changed = true
		}
	}
	s := b.String()
	if len(s) > 120 {
		s = s[:120]
		changed = true
	}
	if s == "" {
		changed = true
	}
	if changed {
		sum := sha1.Sum([]byte(id))
		s += "-" + hex.EncodeToString(sum[:])[:8]
	}
	return s
}

// NewRunID is "local-<UTC %Y%m%dT%H%M%SZ>-<4 hex>".
func NewRunID(t time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return "local-" + t.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// DefaultRunDir is ./embeddedci-runs/<started UTC>-<suite>.
func DefaultRunDir(t time.Time, suite string) string {
	name := Slug(suite)
	if name == "" {
		name = "run"
	}
	return filepath.Join("embeddedci-runs", t.UTC().Format("20060102T150405Z")+"-"+name)
}

// Timestamp is the run.json form of t: RFC 3339 UTC with milliseconds.
func Timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// SHA256Hex is the hex sha256 of data.
func SHA256Hex(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// CanonicalJSON is v as JSON with sorted keys and no spaces (encoding/json sorts map keys).
func CanonicalJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Round-trip through any so struct field order does not matter either.
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, err
	}
	return json.Marshal(generic)
}

// CanonicalSHA256 is the sha256 of CanonicalJSON(v), "" when v is nil or empty.
func CanonicalSHA256(v any) string {
	if v == nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return ""
	}
	data, err := CanonicalJSON(v)
	if err != nil {
		return ""
	}
	return SHA256Hex(data)
}

// FailedCases are the failing cases, in order.
func (r *Run) FailedCases() []Case {
	var out []Case
	for _, c := range r.Results.Cases {
		if c.Failing() {
			out = append(out, c)
		}
	}
	return out
}

// sortedKeys is the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
