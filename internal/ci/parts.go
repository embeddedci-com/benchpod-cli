package ci

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Part is parts/<step>.json: what a child process (the pytest plugin) wrote for one step.
type Part struct {
	SchemaVersion int        `json:"schema_version"`
	Step          string     `json:"step"`
	Kind          string     `json:"kind"`
	Cases         []Case     `json:"cases"`
	Health        PartHealth `json:"health"`
	Tools         Tools      `json:"tools"`
	Pod           *Pod       `json:"pod"`
}

// PartHealth are the health counters a part adds to the run's.
type PartHealth struct {
	LostSamples         int      `json:"lost_samples"`
	RateMisses          int      `json:"rate_misses"`
	BusyRetries         int      `json:"busy_retries"`
	Reconnects          int      `json:"reconnects"`
	MeasurementsDropped int      `json:"measurements_dropped"`
	TraceTruncated      bool     `json:"trace_truncated"`
	Reasons             []string `json:"reasons"`
	// LeaseLost and Valid are not in the spec's part shape; a plugin that adds them is honored.
	LeaseLost bool  `json:"lease_lost"`
	Valid     *bool `json:"valid"`
}

// PartPath is where the part of step lives in runDir.
func PartPath(runDir, step string) string {
	return filepath.Join(runDir, "parts", step+".json")
}

// ReadPart reads the part of step; ok is false when there is none.
func ReadPart(runDir, step string) (*Part, bool, error) {
	data, err := os.ReadFile(PartPath(runDir, step))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var p Part
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, false, fmt.Errorf("parts/%s.json: %w", step, err)
	}
	if p.SchemaVersion != 0 && p.SchemaVersion != SchemaVersion {
		return nil, false, fmt.Errorf("parts/%s.json: schema_version %d is not supported", step, p.SchemaVersion)
	}
	return &p, true, nil
}

// MergePart adds a part to the run: its cases appended (each with the step and attempt set when
// the part left them out), health counters summed, reasons concatenated, tools merged and the pod
// block taken when the run has none.
func (r *Run) MergePart(p *Part, step string, attempt int) {
	for _, c := range p.Cases {
		if c.Step == "" {
			c.Step = step
		}
		if c.Kind == "" {
			c.Kind = firstNonEmpty(p.Kind, "pytest")
		}
		if c.Attempt == 0 {
			c.Attempt = attempt
		}
		r.Results.Cases = append(r.Results.Cases, c)
	}
	h := &r.Health
	h.LostSamples += p.Health.LostSamples
	h.RateMisses += p.Health.RateMisses
	h.BusyRetries += p.Health.BusyRetries
	h.Reconnects += p.Health.Reconnects
	h.MeasurementsDropped += p.Health.MeasurementsDropped
	h.TraceTruncated = h.TraceTruncated || p.Health.TraceTruncated
	h.Reasons = append(h.Reasons, p.Health.Reasons...)
	if p.Health.LostSamples > 0 {
		h.Invalidate(fmt.Sprintf("step %s: a capture lost %d samples", step, p.Health.LostSamples))
	}
	if p.Health.LeaseLost {
		h.LeaseLost = true
		h.Invalidate("lease lost (reported by step " + step + ")")
	}
	if p.Health.Valid != nil && !*p.Health.Valid {
		h.Valid = false
	}
	r.Inputs.Tools.Merge(p.Tools)
	if p.Pod != nil && r.Inputs.Pod.IsZero() {
		r.Inputs.Pod = *p.Pod
	}
}
