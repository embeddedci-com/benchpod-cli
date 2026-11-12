// Package sensors reads BenchPod sensor definitions and stimuli (the sensor library's format,
// docs/format.md), resolves the sensor library version, and builds the pod's sensor_* commands.
package sensors

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// Format is the definition format this CLI writes and checks.
	Format = 1
	// MaxDocumentBytes is the largest definition or stimulus JSON a pod accepts.
	MaxDocumentBytes = 32768
	// TooOld is the error for a pod without the "sensors" capability.
	TooOld = "this pod's firmware is too old for sensor emulation; update the firmware"
)

// Kinds are the sensor kinds a pod plays.
var Kinds = []string{"i2c", "spi", "uart", "analog"}

// NameRE is the pattern of definition and instance names.
var NameRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// Document is a definition or stimulus as the pod receives it: a JSON object in document order.
type Document struct{ *Map }

func (d Document) str(key string) string { v, _ := d.Get(key); s, _ := v.(string); return s }

// Name is the document's name ("" when it has none).
func (d Document) Name() string { return d.str("name") }

// Kind is the document's kind ("" when it has none).
func (d Document) Kind() string { return d.str("kind") }

// JSON is the compact JSON the pod receives.
func (d Document) JSON() ([]byte, error) { return d.Map.MarshalJSON() }

// Params is the definition's params (key, unit, min, max, default).
func (d Document) Params() []*Map {
	v, _ := d.Get("params")
	list, _ := v.([]any)
	var out []*Map
	for _, p := range list {
		if m, ok := p.(*Map); ok {
			out = append(out, m)
		}
	}
	return out
}

// ParseText reads a definition or stimulus from YAML or JSON text. JSON (text starting with "{")
// is read as JSON, keeping its numbers as written.
func ParseText(text string) (Document, error) {
	var v any
	var err error
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		if v, err = decodeOrderedJSON(text); err != nil {
			return Document{}, fmt.Errorf("invalid JSON: %w", err)
		}
	} else if v, err = ParseYAML(text); err != nil {
		return Document{}, err
	}
	m, ok := v.(*Map)
	if !ok {
		return Document{}, fmt.Errorf("a definition is a mapping (format, kind, name, ...)")
	}
	if err := checkFinite(m); err != nil {
		return Document{}, err
	}
	return Document{m}, nil
}

func checkFinite(v any) error {
	switch t := v.(type) {
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return fmt.Errorf("the pod takes finite numbers only (got %v)", t)
		}
	case *Map:
		for _, k := range t.keys {
			if err := checkFinite(t.values[k]); err != nil {
				return err
			}
		}
	case []any:
		for _, c := range t {
			if err := checkFinite(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadFile reads a .yaml/.yml/.json definition or stimulus; a stimulus's data segments with
// file: (CSV t,value, relative to the file) become points.
func ReadFile(path string) (Document, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	doc, err := ParseText(string(raw))
	if err != nil {
		return Document{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := InlineDataFiles(doc, filepath.Dir(path)); err != nil {
		return Document{}, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func commonErrors(d Document) []string {
	var errs []string
	format, set := d.Get("format")
	switch f, ok := number(format); {
	case !set || format == nil:
		errs = append(errs, "format: missing (format: 1)")
	case !ok || f != Format:
		errs = append(errs, fmt.Sprintf("format: %v is not supported (this CLI reads format %d)", format, Format))
	}
	if !NameRE.MatchString(d.Name()) {
		name, _ := d.Get("name")
		errs = append(errs, fmt.Sprintf("name: %q must match [a-z0-9_]{1,32}", fmt.Sprint(name)))
	}
	return errs
}

func sizeErrors(d Document) []string {
	b, err := d.JSON()
	if err != nil {
		return []string{err.Error()}
	}
	if len(b) > MaxDocumentBytes {
		return []string{fmt.Sprintf("%d bytes of JSON, more than the pod's %d", len(b), MaxDocumentBytes)}
	}
	return nil
}

// DefinitionErrors lists what is wrong with a definition's header and size (the pod checks the
// rest); nil when nothing.
func DefinitionErrors(d Document) []string {
	errs := commonErrors(d)
	switch k := d.Kind(); {
	case k == "stimulus":
		errs = append(errs, "kind: this is a stimulus, not a sensor definition")
	case !contains(Kinds, k):
		kind, _ := d.Get("kind")
		errs = append(errs, fmt.Sprintf("kind: %q must be one of %s", fmt.Sprint(kind), strings.Join(Kinds, ", ")))
	}
	if l, ok := d.Get("label"); ok && l != nil {
		if _, isStr := l.(string); !isStr {
			errs = append(errs, "label: must be text")
		}
	}
	if p, ok := d.Get("params"); ok && p != nil {
		list, isList := p.([]any)
		if !isList {
			errs = append(errs, "params: must be a list")
		}
		for i, item := range list {
			m, _ := item.(*Map)
			k, _ := m.Get("key")
			if ks, _ := k.(string); ks == "" {
				errs = append(errs, fmt.Sprintf("params[%d]: needs a key", i))
			}
		}
	}
	return append(errs, sizeErrors(d)...)
}

// StimulusErrors lists what is wrong with a stimulus's header and size; nil when nothing.
func StimulusErrors(d Document) []string {
	errs := commonErrors(d)
	if d.Kind() != "stimulus" {
		kind, _ := d.Get("kind")
		errs = append(errs, fmt.Sprintf("kind: %q must be stimulus", fmt.Sprint(kind)))
	}
	if s, ok := d.Get("signals"); ok && s != nil {
		if _, isMap := s.(*Map); !isMap {
			errs = append(errs, "signals: must be a mapping of param to segments")
		}
	}
	if f, ok := d.Get("faults"); ok && f != nil {
		if _, isList := f.([]any); !isList {
			errs = append(errs, "faults: must be a list")
		}
	}
	return append(errs, sizeErrors(d)...)
}

// CheckDefinition returns an error listing every header problem of a definition.
func CheckDefinition(d Document) error {
	if errs := DefinitionErrors(d); len(errs) > 0 {
		return fmt.Errorf("not a valid sensor definition: %s", strings.Join(errs, "; "))
	}
	return nil
}

// CheckStimulus returns an error listing every header problem of a stimulus.
func CheckStimulus(d Document) error {
	if errs := StimulusErrors(d); len(errs) > 0 {
		return fmt.Errorf("not a valid stimulus: %s", strings.Join(errs, "; "))
	}
	return nil
}

// InlineDataFiles replaces each stimulus data segment's file: (CSV t,value, relative to baseDir)
// with points: the pod receives one JSON document. Other documents are left as they are.
func InlineDataFiles(d Document, baseDir string) error {
	return InlineDataFilesFrom(d, func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(baseDir, filepath.FromSlash(name)))
	})
}

// InlineDataFilesFrom is InlineDataFiles with the CSV files read by read.
func InlineDataFilesFrom(d Document, read func(name string) ([]byte, error)) error {
	if d.Kind() != "stimulus" {
		return nil
	}
	sv, _ := d.Get("signals")
	signals, _ := sv.(*Map)
	var walk func(segs any) error
	walk = func(segs any) error {
		list, _ := segs.([]any)
		for _, s := range list {
			seg, ok := s.(*Map)
			if !ok {
				continue
			}
			shape, _ := seg.Get("shape")
			if shape == "repeat" {
				inner, _ := seg.Get("segments")
				if err := walk(inner); err != nil {
					return err
				}
			}
			fv, _ := seg.Get("file")
			if file, ok := fv.(string); ok && shape == "data" {
				raw, err := read(file)
				if err != nil {
					return fmt.Errorf("data file: %w", err)
				}
				pts, err := csvPoints(file, raw)
				if err != nil {
					return err
				}
				seg.Delete("file")
				seg.Set("points", pts)
			}
		}
		return nil
	}
	for _, k := range signals.Keys() {
		v, _ := signals.Get(k)
		if err := walk(v); err != nil {
			return err
		}
	}
	return nil
}

func csvPoints(name string, raw []byte) ([]any, error) {
	points := []any{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cells := strings.Split(line, ",")
		if len(cells) < 2 {
			return nil, fmt.Errorf("%s: expected 't,value', got %q", name, line)
		}
		t, err1 := strconv.ParseFloat(strings.TrimSpace(cells[0]), 64)
		v, err2 := strconv.ParseFloat(strings.TrimSpace(cells[1]), 64)
		if err1 != nil || err2 != nil {
			if len(points) > 0 {
				return nil, fmt.Errorf("%s: not a number: %q", name, line)
			}
			continue // a header line
		}
		points = append(points, []any{t, v})
	}
	return points, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
