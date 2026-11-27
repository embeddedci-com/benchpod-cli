// Package ci is the file format and the record of `benchpod ci`: the benchpod-ci.yaml suite
// definition (this file), the run folder and run.json it writes (run.go), the cases it reads from
// JUnit XML, `go test -json` and the pytest plugin's parts files (junit.go, gotest.go, parts.go),
// the pod-call trace (trace.go) and the comparison of two runs (diff.go).
//
// Nothing here talks to a pod: the runner in cmd/benchpod-cli drives the pod and uses these types.
package ci

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultFile is the suite definition `benchpod ci` reads when -f is not given.
const DefaultFile = "benchpod-ci.yaml"

// DefaultRunTimeout bounds a whole run when the file sets no `timeout`.
const DefaultRunTimeout = 60 * time.Minute

// Step kinds: exactly one of these keys is set on a step.
const (
	KindFlash        = "flash"
	KindPowerOn      = "power_on"
	KindPowerOff     = "power_off"
	KindPowerCycle   = "power_cycle"
	KindUARTExpect   = "uart_expect"
	KindMeasurePower = "measure_power"
	KindCapture      = "capture"
	KindWait         = "wait"
	KindPytest       = "pytest"
	KindRun          = "run"
)

var stepKinds = []string{KindFlash, KindPowerOn, KindPowerOff, KindPowerCycle, KindUARTExpect,
	KindMeasurePower, KindCapture, KindWait, KindPytest, KindRun}

// Cleanup actions (`cleanup:` as a list); `cleanup: default` is all of them.
const (
	CleanupDACStop    = "dac_stop"
	CleanupPowerOff   = "power_off"
	CleanupSensorsOff = "sensors_off"
	CleanupPullsOff   = "pulls_off"
)

// DefaultCleanup is what `cleanup: default` (and an absent `cleanup`) runs, in this order.
var DefaultCleanup = []string{CleanupDACStop, CleanupPowerOff, CleanupSensorsOff, CleanupPullsOff}

// Native metrics each step kind produces (names fixed by the spec).
var nativeMetrics = map[string][]string{
	KindPowerCycle:   {"boot_ms"},
	KindUARTExpect:   {"match_ms"},
	KindMeasurePower: {"mean_ma", "peak_ma", "min_ma", "mean_v"},
}

// MetricUnit is the unit of a native metric.
func MetricUnit(name string) string {
	switch {
	case strings.HasSuffix(name, "_ms"):
		return "ms"
	case strings.HasSuffix(name, "_ma"):
		return "mA"
	case strings.HasSuffix(name, "_v"):
		return "V"
	}
	return ""
}

// Config is a parsed and validated benchpod-ci.yaml (version 1).
type Config struct {
	Path   string // the file it was read from ("" when parsed from bytes)
	Dir    string // the file's directory: commands and relative paths run from here
	SHA256 string // sha256 of the file bytes (key.definition_sha256)

	Version  int
	Suite    string
	DUT      string
	Wiring   string
	Require  Require
	Firmware *Firmware
	Env      map[string]string
	Baseline string
	Gate     string // "" (each metric's fail_on_drift decides) | drift (every drift allowance gates) | none
	Timeout  time.Duration
	Keep     string // all | failed | none
	Cleanup  []string
	Upload   string // auto | true | false
	Steps    []*Step
}

// Require is the `require` block.
type Require struct {
	PodFirmware string // a semver range, "" = any
	Range       VersionRange
}

// Firmware is the `firmware` block. `build` is a shell command that produces `file`; an uploaded
// build is named with `build_id`, or `repo` (+ `ref`, `artifact`).
type Firmware struct {
	Build    string
	File     string // relative to Config.Dir unless absolute
	BuildID  string
	Repo     string
	Ref      string
	Artifact string
	Flash    FlashOverrides
	Line     int
}

// Uploaded says the firmware is a build stored on embeddedci.com, not a local file.
func (f *Firmware) Uploaded() bool {
	return f != nil && (f.BuildID != "" || f.Repo != "" || f.Ref != "")
}

// FlashOverrides are `firmware.flash`: settings that override the wiring profile's for the flash.
type FlashOverrides struct {
	Target       string
	SWCLK, SWDIO string
	NReset       *bool
	LoadAddress  string
	TargetPower  string // a supply: 5v, external, 3v3 or 1v8 ("" leaves it to the wiring profile)
	// TargetPowerLegacy says target_power was given as 1, 2 or internal (deprecated).
	TargetPowerLegacy bool
	Verify            *bool
	Reset             *bool
	ConnectUnderReset *bool
	OpenOCD           string
}

// Step is one entry of `steps`.
type Step struct {
	ID       string
	Name     string
	Line     int
	Required bool
	Timeout  time.Duration // 0 = the kind's default
	Kind     string
	Implicit bool // the flash step added because `firmware` is set

	PowerCycle   *PowerCycle
	UARTExpect   *UARTExpect
	MeasurePower *MeasurePower
	Wait         time.Duration
	Pytest       *Pytest
	Run          string
	Results      string // "", "junit:<path>", "gotest-json"
	Expect       []Expect
}

// Display is the step's name for messages: its name, else its id.
func (s *Step) Display() string {
	if s.Name != "" {
		return s.Name
	}
	return s.ID
}

// PowerCycle is `power_cycle`: power off, wait off_ms, power on and (with until) read the UART
// until the regex matches.
type PowerCycle struct {
	Until   string
	Re      *regexp.Regexp
	Timeout time.Duration
	OffMS   int
	UART    UARTPort
}

// UARTPort says which serial port a UART step reads. The default is the soft UART on the
// profile's LA pins; Port "usb" is the serial port of the board the pod hosts over USB
// (firmware 4.0.3+, feature uart_usb), and then DTR, RTS and Baud apply.
type UARTPort struct {
	Port string // "" (the profile's LA pins) or "usb"
	DTR  *bool  // usb: the DTR line (the pod's default is true)
	RTS  *bool  // usb: the RTS line (the pod's default is true)
	Baud int    // usb: overrides the profile's uart_baud (0 = the profile's)
}

// IsUSB says the step reads the board's USB serial port.
func (u UARTPort) IsUSB() bool { return u.Port == "usb" }

// UARTExpect is `uart_expect`: optionally send a string, then read until the regex matches.
type UARTExpect struct {
	Until   string
	Re      *regexp.Regexp
	Timeout time.Duration
	Send    string
	UART    UARTPort
}

// MeasurePower is `measure_power`.
type MeasurePower struct {
	Duration time.Duration
}

// Pytest is a `pytest` step.
type Pytest struct {
	Path   string
	Args   []string
	Python string // the interpreter command, "python3" when empty; may be several words ("uv run python")
}

// Expect is one `expect` entry: a metric (native) or "<case id>/<name>" (pytest, run) with limits.
type Expect struct {
	Name  string
	Min   *float64
	Max   *float64
	Drift *float64
	// FailOnDrift: a change beyond Drift against the baseline fails the run (exit 11); nil = the
	// file's gate decides.
	FailOnDrift *bool
	Line        int
}

// Error is a problem in the file, with the line it is on when known.
type Error struct {
	File string
	Line int
	Msg  string
}

func (e *Error) Error() string {
	where := e.File
	if where == "" {
		where = DefaultFile
	}
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", where, e.Line, e.Msg)
	}
	return where + ": " + e.Msg
}

// Errors is every problem found in one file.
type Errors []*Error

func (es Errors) Error() string {
	lines := make([]string, len(es))
	for i, e := range es {
		lines[i] = e.Error()
	}
	return strings.Join(lines, "\n")
}

// Load reads and parses the file at path. env looks up variables for ${VAR} (os.LookupEnv when nil).
func Load(path string, env func(string) (string, bool)) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return Parse(data, abs, env)
}

// Parse parses and validates a suite definition. path is used in messages and for Config.Dir.
func Parse(data []byte, path string, env func(string) (string, bool)) (*Config, error) {
	if env == nil {
		env = os.LookupEnv
	}
	p := &parser{file: path}
	cfg := &Config{Path: path, SHA256: SHA256Hex(data), Keep: "failed", Upload: "auto",
		Timeout: DefaultRunTimeout, Cleanup: append([]string(nil), DefaultCleanup...), Env: map[string]string{}}
	if path != "" {
		cfg.Dir = filepath.Dir(path)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, &Error{File: path, Msg: strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	if len(doc.Content) == 0 {
		return nil, &Error{File: path, Msg: "the file is empty"}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, &Error{File: path, Line: root.Line, Msg: "the file must be a mapping (version: 1, suite: ..., steps: ...)"}
	}

	// `env` first: its values may use the process environment, and every other string may use both.
	if envNode := mapValue(root, "env"); envNode != nil {
		if envNode.Kind != yaml.MappingNode {
			p.errf(envNode.Line, "env must be a mapping of NAME: value")
		} else {
			for i := 0; i+1 < len(envNode.Content); i += 2 {
				k, v := envNode.Content[i], envNode.Content[i+1]
				if v.Kind != yaml.ScalarNode {
					p.errf(v.Line, "env.%s must be a string", k.Value)
					continue
				}
				val, err := Expand(v.Value, env)
				if err != nil {
					p.errf(v.Line, "env.%s: %v", k.Value, err)
				}
				cfg.Env[k.Value] = val
				v.Value = val
			}
		}
	}
	lookup := func(name string) (string, bool) {
		if v, ok := cfg.Env[name]; ok {
			return v, true
		}
		return env(name)
	}
	p.expandTree(root, lookup, true)

	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		seen[k.Value] = true
		switch k.Value {
		case "version":
			n, ok := p.intValue(v, "version")
			if ok && n != 1 {
				p.errf(v.Line, "version %d is not supported: this CLI reads version 1", n)
			}
			cfg.Version = n
		case "suite":
			cfg.Suite = p.str(v, "suite")
		case "dut":
			cfg.DUT = p.str(v, "dut")
		case "wiring":
			cfg.Wiring = p.str(v, "wiring")
		case "require":
			p.parseRequire(v, &cfg.Require)
		case "firmware":
			cfg.Firmware = p.parseFirmware(v)
		case "env":
			// done above
		case "baseline":
			cfg.Baseline = p.str(v, "baseline")
		case "gate":
			cfg.Gate = p.str(v, "gate")
			if cfg.Gate != "drift" && cfg.Gate != "none" {
				p.errf(v.Line, "gate must be drift (a drift beyond its allowance against the baseline fails the run with exit 11) or none, got %q", cfg.Gate)
			}
		case "timeout":
			if d, ok := p.duration(v, "timeout"); ok {
				cfg.Timeout = d
			}
		case "keep":
			p.parseKeep(v, cfg)
		case "cleanup":
			cfg.Cleanup = p.parseCleanup(v)
		case "upload":
			cfg.Upload = p.parseUpload(v)
		case "steps":
			cfg.Steps = p.parseSteps(v)
		default:
			p.errf(k.Line, "unknown key %q (known: version, suite, dut, wiring, require, firmware, env, baseline, gate, timeout, keep, cleanup, upload, steps)", k.Value)
		}
	}
	if !seen["version"] {
		p.errf(root.Line, "version is required (version: 1)")
	}
	if strings.TrimSpace(cfg.Suite) == "" {
		p.errf(root.Line, "suite is required: the name every run of this file is stored and compared under")
	}
	if strings.TrimSpace(cfg.DUT) == "" {
		p.errf(root.Line, `dut is required: the board wired to the pod ("none" for a suite that tests the pod only)`)
	}
	if len(cfg.Steps) == 0 && seen["steps"] {
		p.errf(mapValue(root, "steps").Line, "steps is empty")
	}
	if !seen["steps"] && cfg.Firmware == nil {
		p.errf(root.Line, "steps is required")
	}
	p.finishSteps(cfg)
	if len(p.errs) > 0 {
		return nil, p.errs
	}
	return cfg, nil
}

// finishSteps adds the implicit flash step, defaults ids, and checks ids are unique.
func (p *parser) finishSteps(cfg *Config) {
	hasFlash := false
	for _, s := range cfg.Steps {
		if s.Kind == KindFlash {
			hasFlash = true
			if cfg.Firmware == nil {
				p.errf(s.Line, "a flash step needs a firmware block (firmware: {file: ...})")
			}
		}
	}
	if cfg.Firmware != nil && !hasFlash {
		id := "flash"
		for _, s := range cfg.Steps {
			if s.ID == id {
				id = "flash-firmware"
			}
		}
		implicit := &Step{ID: id, Kind: KindFlash, Implicit: true, Required: true, Line: cfg.Firmware.Line}
		cfg.Steps = append([]*Step{implicit}, cfg.Steps...)
	}
	ids := map[string]int{}
	for _, s := range cfg.Steps {
		if s.ID == "" {
			s.ID = Slug(s.Name)
			if s.ID == "" {
				s.ID = strings.ReplaceAll(s.Kind, "_", "-")
			}
			// Unnamed steps of the same kind: power-on, power-on-2, ...
			if _, dup := ids[s.ID]; dup && s.Name == "" {
				base := s.ID
				for n := 2; ; n++ {
					s.ID = fmt.Sprintf("%s-%d", base, n)
					if _, dup := ids[s.ID]; !dup {
						break
					}
				}
			}
		}
		if line, dup := ids[s.ID]; dup {
			p.errf(s.Line, "step id %q is used twice (also on line %d): ids must be unique", s.ID, line)
			continue
		}
		ids[s.ID] = s.Line
	}
}

var stepIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Slug turns a step name into an id: lowercase, runs of other characters become one "-".
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 64 {
		s = strings.TrimRight(s[:64], "-")
	}
	return s
}

// ── parser helpers ──────────────────────────────────────────────────────────

type parser struct {
	file string
	errs Errors
}

func (p *parser) errf(line int, format string, args ...any) {
	p.errs = append(p.errs, &Error{File: p.file, Line: line, Msg: fmt.Sprintf(format, args...)})
}

// mapValue is the value node of key in a mapping node, nil when absent.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// expandTree expands ${VAR} in every string value (not keys) below n, except the env block's
// values, which were expanded already.
func (p *parser) expandTree(n *yaml.Node, lookup func(string) (string, bool), top bool) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if top && n.Content[i].Value == "env" {
				continue
			}
			p.expandTree(n.Content[i+1], lookup, false)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			p.expandTree(c, lookup, false)
		}
	case yaml.ScalarNode:
		if n.Tag != "!!str" && n.Tag != "" {
			return
		}
		v, err := Expand(n.Value, lookup)
		if err != nil {
			p.errf(n.Line, "%v", err)
			return
		}
		n.Value = v
	}
}

// parseUARTPort reads the optional port, dtr, rts and baud keys of a UART step. dtr, rts and baud
// belong to the USB serial port.
func (p *parser) parseUARTPort(v *yaml.Node, what string) UARTPort {
	var u UARTPort
	if n := mapValue(v, "port"); n != nil {
		u.Port = strings.ToLower(strings.TrimSpace(p.str(n, what+".port")))
		if u.Port != "" && u.Port != "usb" && u.Port != "pins" {
			p.errf(n.Line, "%s.port must be usb (the board's USB serial port) or pins (the profile's LA pins, the default), got %q", what, u.Port)
		}
		if u.Port == "pins" {
			u.Port = ""
		}
	}
	for _, k := range []string{"dtr", "rts"} {
		n := mapValue(v, k)
		if n == nil {
			continue
		}
		if !u.IsUSB() {
			p.errf(n.Line, "%s.%s only applies with port: usb", what, k)
			continue
		}
		if b, ok := p.boolValue(n, what+"."+k); ok {
			if k == "dtr" {
				u.DTR = &b
			} else {
				u.RTS = &b
			}
		}
	}
	if n := mapValue(v, "baud"); n != nil {
		if !u.IsUSB() {
			p.errf(n.Line, "%s.baud only applies with port: usb (the pins use the profile's uart_baud)", what)
		} else if b, ok := p.intValue(n, what+".baud"); ok {
			if b < 300 || b > 3000000 {
				p.errf(n.Line, "%s.baud must be 300 to 3000000", what)
			}
			u.Baud = b
		}
	}
	return u
}

func (p *parser) str(n *yaml.Node, what string) string {
	if n.Kind != yaml.ScalarNode {
		p.errf(n.Line, "%s must be a string", what)
		return ""
	}
	return n.Value
}

func (p *parser) intValue(n *yaml.Node, what string) (int, bool) {
	if n.Kind != yaml.ScalarNode {
		p.errf(n.Line, "%s must be a number", what)
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(n.Value))
	if err != nil {
		p.errf(n.Line, "%s must be a whole number, got %q", what, n.Value)
		return 0, false
	}
	return v, true
}

func (p *parser) boolValue(n *yaml.Node, what string) (bool, bool) {
	if n.Kind != yaml.ScalarNode {
		p.errf(n.Line, "%s must be true or false", what)
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(n.Value)) {
	case "true", "yes", "on":
		return true, true
	case "false", "no", "off":
		return false, true
	}
	p.errf(n.Line, "%s must be true or false, got %q", what, n.Value)
	return false, false
}

func (p *parser) number(n *yaml.Node, what string) (float64, bool) {
	if n.Kind != yaml.ScalarNode {
		p.errf(n.Line, "%s must be a number", what)
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(n.Value), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		p.errf(n.Line, "%s must be a number, got %q", what, n.Value)
		return 0, false
	}
	return v, true
}

func (p *parser) duration(n *yaml.Node, what string) (time.Duration, bool) {
	if n.Kind != yaml.ScalarNode {
		p.errf(n.Line, "%s must be a duration like 10s, 2m or 500ms", what)
		return 0, false
	}
	d, err := ParseDuration(n.Value)
	if err != nil {
		p.errf(n.Line, "%s: %v", what, err)
		return 0, false
	}
	return d, true
}

// ParseDuration reads 10s, 2m, 1h30m, 500ms; a bare number is seconds.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		if f < 0 {
			return 0, fmt.Errorf("duration %q is negative", s)
		}
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (use 10s, 2m, 500ms)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q is negative", s)
	}
	return d, nil
}

// ParseDrift reads "20%" or 0.2 as the fraction 0.2.
func ParseDrift(s string) (float64, error) {
	s = strings.TrimSpace(s)
	pct := strings.HasSuffix(s, "%")
	v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("drift %q must be a percentage (20%%) or a fraction (0.2)", s)
	}
	if pct {
		v /= 100
	}
	if v < 0 {
		return 0, fmt.Errorf("drift %q is negative", s)
	}
	return v, nil
}

// checkKeys reports every key of mapping n that is not in allowed.
func (p *parser) checkKeys(n *yaml.Node, what string, allowed ...string) {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if k := n.Content[i]; !ok[k.Value] {
			p.errf(k.Line, "%s: unknown key %q (known: %s)", what, k.Value, strings.Join(allowed, ", "))
		}
	}
}

func (p *parser) parseRequire(n *yaml.Node, r *Require) {
	if n.Kind != yaml.MappingNode {
		p.errf(n.Line, `require must be a mapping, e.g. require: {pod_firmware: ">=3.10.12"}`)
		return
	}
	p.checkKeys(n, "require", "pod_firmware")
	if v := mapValue(n, "pod_firmware"); v != nil {
		r.PodFirmware = p.str(v, "require.pod_firmware")
		rng, err := ParseVersionRange(r.PodFirmware)
		if err != nil {
			p.errf(v.Line, "require.pod_firmware: %v", err)
		}
		r.Range = rng
	}
}

func (p *parser) parseFirmware(n *yaml.Node) *Firmware {
	f := &Firmware{Line: n.Line}
	if n.Kind == yaml.ScalarNode { // firmware: path/to/file.elf
		f.File = n.Value
		return f
	}
	if n.Kind != yaml.MappingNode {
		p.errf(n.Line, "firmware must be a mapping (file: ..., build: ...)")
		return f
	}
	p.checkKeys(n, "firmware", "build", "file", "build_id", "repo", "ref", "artifact", "flash")
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		switch k.Value {
		case "build":
			f.Build = p.str(v, "firmware.build")
		case "file":
			f.File = p.str(v, "firmware.file")
		case "build_id":
			f.BuildID = p.str(v, "firmware.build_id")
		case "repo":
			f.Repo = p.str(v, "firmware.repo")
		case "ref":
			f.Ref = p.str(v, "firmware.ref")
		case "artifact":
			f.Artifact = p.str(v, "firmware.artifact")
		case "flash":
			p.parseFlash(v, &f.Flash)
		}
	}
	if f.File != "" && f.Uploaded() {
		p.errf(n.Line, "firmware: give either file or an uploaded build (build_id, or repo/ref), not both")
	}
	if f.File == "" && !f.Uploaded() {
		p.errf(n.Line, "firmware needs file (a local image) or an uploaded build (build_id, or repo with ref)")
	}
	if f.Build != "" && f.File == "" {
		p.errf(n.Line, "firmware.build is the command that produces firmware.file: set file too")
	}
	if f.Artifact != "" && !f.Uploaded() {
		p.errf(n.Line, "firmware.artifact only applies to an uploaded build (build_id, or repo/ref)")
	}
	return f
}

func (p *parser) parseFlash(n *yaml.Node, o *FlashOverrides) {
	if n.Kind != yaml.MappingNode {
		p.errf(n.Line, "firmware.flash must be a mapping (target: ..., swclk: ...)")
		return
	}
	p.checkKeys(n, "firmware.flash", "target", "swclk", "swdio", "nreset", "load_address", "target_power",
		"verify", "reset", "connect_under_reset", "openocd")
	optBool := func(v *yaml.Node, what string) *bool {
		b, ok := p.boolValue(v, what)
		if !ok {
			return nil
		}
		return &b
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		what := "firmware.flash." + k.Value
		switch k.Value {
		case "target":
			o.Target = p.str(v, what)
		case "swclk":
			o.SWCLK = p.str(v, what)
		case "swdio":
			o.SWDIO = p.str(v, what)
		case "nreset":
			o.NReset = optBool(v, what)
		case "load_address":
			o.LoadAddress = p.str(v, what)
		case "target_power":
			if v.Kind != yaml.ScalarNode {
				p.errf(v.Line, "%s must be one of %s", what, SupplyValues)
				break
			}
			sup, legacy, ok := NormalizeSupply(v.Value)
			if !ok {
				p.errf(v.Line, "%s must be one of %s, got %q", what, SupplyValues, v.Value)
				break
			}
			o.TargetPower, o.TargetPowerLegacy = sup, legacy
		case "verify":
			o.Verify = optBool(v, what)
		case "reset":
			o.Reset = optBool(v, what)
		case "connect_under_reset":
			o.ConnectUnderReset = optBool(v, what)
		case "openocd":
			o.OpenOCD = p.str(v, what)
		}
	}
}

func (p *parser) parseKeep(n *yaml.Node, cfg *Config) {
	if n.Kind != yaml.MappingNode {
		p.errf(n.Line, "keep must be a mapping, e.g. keep: {traces: failed}")
		return
	}
	p.checkKeys(n, "keep", "traces")
	if v := mapValue(n, "traces"); v != nil {
		t := p.str(v, "keep.traces")
		switch t {
		case "all", "failed", "none":
			cfg.Keep = t
		default:
			p.errf(v.Line, "keep.traces must be all, failed or none, got %q", t)
		}
	}
}

func (p *parser) parseCleanup(n *yaml.Node) []string {
	if n.Kind == yaml.ScalarNode {
		switch n.Value {
		case "default":
			return append([]string(nil), DefaultCleanup...)
		case "none":
			return []string{}
		}
		p.errf(n.Line, "cleanup must be default, none or a list of %s", strings.Join(DefaultCleanup, ", "))
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		p.errf(n.Line, "cleanup must be default, none or a list of %s", strings.Join(DefaultCleanup, ", "))
		return nil
	}
	out := []string{}
	for _, c := range n.Content {
		a := p.str(c, "cleanup entry")
		if !contains(DefaultCleanup, a) {
			p.errf(c.Line, "cleanup: unknown action %q (known: %s)", a, strings.Join(DefaultCleanup, ", "))
			continue
		}
		out = append(out, a)
	}
	return out
}

func (p *parser) parseUpload(n *yaml.Node) string {
	v := strings.ToLower(p.str(n, "upload"))
	switch v {
	case "auto", "true", "false":
		return v
	case "yes", "on":
		return "true"
	case "no", "off":
		return "false"
	}
	p.errf(n.Line, "upload must be auto, true or false, got %q", n.Value)
	return "auto"
}

func (p *parser) parseSteps(n *yaml.Node) []*Step {
	if n.Kind != yaml.SequenceNode {
		p.errf(n.Line, "steps must be a list")
		return nil
	}
	var out []*Step
	for i, sn := range n.Content {
		if s := p.parseStep(sn, i+1); s != nil {
			out = append(out, s)
		}
	}
	return out
}

var stepCommonKeys = []string{"id", "name", "required", "timeout", "expect", "results"}

func (p *parser) parseStep(n *yaml.Node, index int) *Step {
	if n.Kind != yaml.MappingNode {
		p.errf(n.Line, "step %d must be a mapping (id: ..., power_cycle: {...})", index)
		return nil
	}
	s := &Step{Line: n.Line}
	label := fmt.Sprintf("step %d", index)
	if v := mapValue(n, "id"); v != nil {
		s.ID = p.str(v, label+" id")
		if s.ID != "" && !stepIDRE.MatchString(s.ID) {
			p.errf(v.Line, "%s: id %q must be 1-64 characters of letters, digits, '.', '_' or '-', starting with a letter or digit", label, s.ID)
		}
		label = "step " + s.ID
	}
	var kinds []string
	var expectNode *yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		switch k.Value {
		case "id":
		case "name":
			s.Name = p.str(v, label+" name")
		case "required":
			s.Required, _ = p.boolValue(v, label+" required")
		case "timeout":
			s.Timeout, _ = p.duration(v, label+" timeout")
		case "expect":
			expectNode = v
		case "results":
			s.Results = p.str(v, label+" results")
		default:
			if !contains(stepKinds, k.Value) {
				p.errf(k.Line, "%s: unknown key %q (a step has one of %s, plus %s)", label, k.Value,
					strings.Join(stepKinds, ", "), strings.Join(stepCommonKeys, ", "))
				continue
			}
			kinds = append(kinds, k.Value)
			s.Kind = k.Value
			p.parseStepBody(s, v, label)
		}
	}
	switch len(kinds) {
	case 0:
		p.errf(n.Line, "%s needs one of %s", label, strings.Join(stepKinds, ", "))
		return nil
	case 1:
	default:
		p.errf(n.Line, "%s has %s: a step does exactly one thing", label, strings.Join(kinds, " and "))
		return nil
	}
	if s.Results != "" {
		if s.Kind != KindRun {
			p.errf(n.Line, "%s: results only applies to run steps", label)
		} else if s.Results != "gotest-json" && !strings.HasPrefix(s.Results, "junit:") {
			p.errf(n.Line, "%s: results must be junit:<path> or gotest-json, got %q", label, s.Results)
		} else if s.Results == "junit:" {
			p.errf(n.Line, "%s: results junit: needs the path of the JUnit XML file (junit:build/results.xml)", label)
		}
	}
	if expectNode != nil {
		s.Expect = p.parseExpect(expectNode, s, label)
	}
	return s
}

func (p *parser) parseStepBody(s *Step, v *yaml.Node, label string) {
	what := label + " " + s.Kind
	emptyOK := func() {
		if v.Kind == yaml.MappingNode && len(v.Content) > 0 {
			p.errf(v.Line, "%s takes no settings ({}); flash settings go in firmware.flash", what)
		} else if v.Kind == yaml.ScalarNode && v.Tag != "!!null" && v.Value != "" {
			p.errf(v.Line, "%s takes no value ({})", what)
		}
	}
	switch s.Kind {
	case KindFlash, KindPowerOn, KindPowerOff:
		emptyOK()
	case KindPowerCycle:
		pc := &PowerCycle{Timeout: 10 * time.Second, OffMS: 500}
		s.PowerCycle = pc
		if v.Kind == yaml.ScalarNode && (v.Tag == "!!null" || v.Value == "") {
			return
		}
		if v.Kind != yaml.MappingNode {
			p.errf(v.Line, "%s must be a mapping {until: <regex>, timeout: 10s, off_ms: 500}", what)
			return
		}
		p.checkKeys(v, what, "until", "timeout", "off_ms", "port", "dtr", "rts", "baud")
		pc.UART = p.parseUARTPort(v, what)
		if pc.UART.Port != "" && mapValue(v, "until") == nil {
			p.errf(v.Line, "%s: port, dtr, rts and baud need until (they say which serial port to read)", what)
		}
		if u := mapValue(v, "until"); u != nil {
			pc.Until = p.str(u, what+".until")
			pc.Re = p.regex(u, pc.Until, what+".until")
		}
		if t := mapValue(v, "timeout"); t != nil {
			if d, ok := p.duration(t, what+".timeout"); ok {
				pc.Timeout = d
			}
		}
		if o := mapValue(v, "off_ms"); o != nil {
			if n, ok := p.intValue(o, what+".off_ms"); ok {
				if n < 0 || n > 60000 {
					p.errf(o.Line, "%s.off_ms must be 0..60000", what)
				}
				pc.OffMS = n
			}
		}
	case KindUARTExpect:
		u := &UARTExpect{Timeout: 5 * time.Second}
		s.UARTExpect = u
		if v.Kind != yaml.MappingNode {
			p.errf(v.Line, "%s must be a mapping {until: <regex>, timeout: 5s, send: <string>}", what)
			return
		}
		p.checkKeys(v, what, "until", "timeout", "send", "port", "dtr", "rts", "baud")
		u.UART = p.parseUARTPort(v, what)
		if un := mapValue(v, "until"); un != nil {
			u.Until = p.str(un, what+".until")
			u.Re = p.regex(un, u.Until, what+".until")
		} else {
			p.errf(v.Line, "%s needs until: <regex>", what)
		}
		if t := mapValue(v, "timeout"); t != nil {
			if d, ok := p.duration(t, what+".timeout"); ok {
				u.Timeout = d
			}
		}
		if sn := mapValue(v, "send"); sn != nil {
			u.Send = p.str(sn, what+".send")
		}
	case KindMeasurePower:
		m := &MeasurePower{Duration: 2 * time.Second}
		s.MeasurePower = m
		if v.Kind == yaml.ScalarNode && (v.Tag == "!!null" || v.Value == "") {
			return
		}
		if v.Kind != yaml.MappingNode {
			p.errf(v.Line, "%s must be a mapping {duration: 2s}", what)
			return
		}
		p.checkKeys(v, what, "duration")
		if d := mapValue(v, "duration"); d != nil {
			if dd, ok := p.duration(d, what+".duration"); ok {
				if dd <= 0 || dd > 10*time.Minute {
					p.errf(d.Line, "%s.duration must be more than 0 and at most 10m", what)
				}
				m.Duration = dd
			}
		}
	case KindCapture:
		p.errf(v.Line, "%s: capture steps are not supported yet by this CLI; capture from a pytest step for now", what)
	case KindWait:
		if d, ok := p.duration(v, what); ok {
			s.Wait = d
		}
	case KindPytest:
		pt := &Pytest{}
		s.Pytest = pt
		if v.Kind == yaml.ScalarNode && (v.Tag == "!!null" || v.Value == "") {
			return
		}
		if v.Kind == yaml.ScalarNode { // pytest: tests
			pt.Path = v.Value
			return
		}
		if v.Kind != yaml.MappingNode {
			p.errf(v.Line, "%s must be a mapping {path: tests, args: [], python: python3}", what)
			return
		}
		p.checkKeys(v, what, "path", "args", "python")
		if pn := mapValue(v, "path"); pn != nil {
			pt.Path = p.str(pn, what+".path")
		}
		if an := mapValue(v, "args"); an != nil {
			if an.Kind != yaml.SequenceNode {
				p.errf(an.Line, "%s.args must be a list of strings, e.g. [-m, hardware]", what)
			} else {
				for _, a := range an.Content {
					pt.Args = append(pt.Args, p.str(a, what+".args entry"))
				}
			}
		}
		if py := mapValue(v, "python"); py != nil {
			pt.Python = p.str(py, what+".python")
		}
	case KindRun:
		s.Run = p.str(v, what)
		if strings.TrimSpace(s.Run) == "" {
			p.errf(v.Line, "%s: the command is empty", what)
		}
	}
}

func (p *parser) regex(n *yaml.Node, expr, what string) *regexp.Regexp {
	if expr == "" {
		p.errf(n.Line, "%s is empty", what)
		return nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		p.errf(n.Line, "%s: %v", what, err)
		return nil
	}
	return re
}

func (p *parser) parseExpect(n *yaml.Node, s *Step, label string) []Expect {
	if n.Kind != yaml.MappingNode {
		p.errf(n.Line, "%s expect must be a mapping {<metric>: {min: , max: , drift: 20%%}}", label)
		return nil
	}
	var out []Expect
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		e := Expect{Name: k.Value, Line: k.Line}
		switch s.Kind {
		case KindPytest, KindRun:
			if !strings.Contains(e.Name, "/") {
				p.errf(k.Line, "%s expect: %q must be \"<case id>/<measurement>\" on a %s step", label, e.Name, s.Kind)
			}
		default:
			allowed := nativeMetrics[s.Kind]
			if s.Kind == KindPowerCycle && s.PowerCycle != nil && s.PowerCycle.Until == "" {
				allowed = nil // boot_ms needs until
			}
			if !contains(allowed, e.Name) {
				hint := "this step produces no metrics"
				if len(allowed) > 0 {
					hint = "this step produces " + strings.Join(allowed, ", ")
				} else if s.Kind == KindPowerCycle {
					hint = "boot_ms needs power_cycle.until"
				}
				p.errf(k.Line, "%s expect: unknown metric %q (%s)", label, e.Name, hint)
				continue
			}
		}
		if v.Kind == yaml.ScalarNode && (v.Tag == "!!null" || v.Value == "") {
			out = append(out, e) // {} or empty: record only
			continue
		}
		if v.Kind != yaml.MappingNode {
			p.errf(v.Line, "%s expect.%s must be a mapping {min: , max: , drift: } ({} records it)", label, e.Name)
			continue
		}
		p.checkKeys(v, label+" expect."+e.Name, "min", "max", "drift", "fail_on_drift")
		for j := 0; j+1 < len(v.Content); j += 2 {
			kk, vv := v.Content[j], v.Content[j+1]
			if vv.Tag == "!!null" {
				continue
			}
			switch kk.Value {
			case "min":
				if f, ok := p.number(vv, label+" expect."+e.Name+".min"); ok {
					e.Min = &f
				}
			case "max":
				if f, ok := p.number(vv, label+" expect."+e.Name+".max"); ok {
					e.Max = &f
				}
			case "drift":
				d, err := ParseDrift(vv.Value)
				if err != nil {
					p.errf(vv.Line, "%s expect.%s: %v", label, e.Name, err)
					continue
				}
				e.Drift = &d
			case "fail_on_drift":
				if b, ok := p.boolValue(vv, label+" expect."+e.Name+".fail_on_drift"); ok {
					e.FailOnDrift = &b
				}
			}
		}
		if e.FailOnDrift != nil && *e.FailOnDrift && e.Drift == nil {
			p.errf(k.Line, "%s expect.%s: fail_on_drift needs a drift allowance (drift: 20%%)", label, e.Name)
		}
		if e.Min != nil && e.Max != nil && *e.Min > *e.Max {
			p.errf(k.Line, "%s expect.%s: min %g is above max %g", label, e.Name, *e.Min, *e.Max)
		}
		out = append(out, e)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ── ${VAR} expansion ────────────────────────────────────────────────────────

// Expand replaces ${VAR} and ${VAR:-default} in s. "$${" is a literal "${". A bare $VAR is left
// alone (shell commands keep their own variables). An unset variable without a default is an error.
func Expand(s string, lookup func(string) (string, bool)) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var b strings.Builder
	var missing []string
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "$${") {
			b.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(s[i:], "${") {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", fmt.Errorf("unclosed ${ in %q", s)
		}
		expr := s[i+2 : i+2+end]
		name, def, hasDef := strings.Cut(expr, ":-")
		if !envNameRE.MatchString(name) {
			return "", fmt.Errorf("${%s}: %q is not a variable name", expr, name)
		}
		if v, ok := lookup(name); ok && (v != "" || !hasDef) {
			b.WriteString(v)
		} else if hasDef {
			b.WriteString(def)
		} else {
			missing = append(missing, name)
		}
		i += 2 + end + 1
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("${%s} is not set (set it, or give a default: ${%s:-value})", missing[0], missing[0])
	}
	return b.String(), nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ── helpers shared with the runner ──────────────────────────────────────────

// Select keeps the steps whose ids are in ids (all when ids is empty). It errors on an unknown id.
func (c *Config) Select(ids []string) ([]*Step, error) {
	if len(ids) == 0 {
		return c.Steps, nil
	}
	want := map[string]bool{}
	for _, id := range ids {
		for _, part := range strings.Split(id, ",") {
			if part = strings.TrimSpace(part); part != "" {
				want[part] = true
			}
		}
	}
	var known []string
	for _, s := range c.Steps {
		known = append(known, s.ID)
	}
	for id := range want {
		if !contains(known, id) {
			return nil, fmt.Errorf("--step %s: no such step (steps: %s)", id, strings.Join(known, ", "))
		}
	}
	var out []*Step
	for _, s := range c.Steps {
		if want[s.ID] {
			out = append(out, s)
		}
	}
	return out, nil
}

// Resolve makes path absolute against the file's directory.
func (c *Config) Resolve(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(c.Dir, path)
}

// String summarizes the file for `ci validate`.
func (c *Config) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "suite %s, dut %s", c.Suite, c.DUT)
	if c.Wiring != "" {
		fmt.Fprintf(&b, ", wiring %s", c.Wiring)
	}
	fmt.Fprintf(&b, ": %d steps", len(c.Steps))
	return b.String()
}

// GatesDrift says whether a drift of measurement name of case caseID beyond its allowance fails
// the run: its expect's fail_on_drift (on the step whose id is the case id, or as
// "<case id>/<name>" on a pytest or run step), else the file's gate.
func (c *Config) GatesDrift(caseID, name string) bool {
	for _, s := range c.Steps {
		for _, e := range s.Expect {
			if (s.ID == caseID && e.Name == name) || e.Name == caseID+"/"+name {
				if e.FailOnDrift != nil {
					return *e.FailOnDrift
				}
			}
		}
	}
	return c.Gate == "drift"
}
