package serverapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ── test runs (benchpod ci) ─────────────────────────────────────────────────
//
// POST /api/runs stores a run.json; POST /api/runs/{id}/files?name= one file of its run folder;
// GET /api/runs lists runs, GET /api/runs/{id} returns one, GET /api/runs/{id}/compare diffs it.

// RunCreated is the answer to an upload.
type RunCreated struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	BuildID string `json:"build_id"`
}

// CreateRun uploads a run.json document (sent as is).
func (c *Client) CreateRun(ctx context.Context, credential string, runJSON []byte) (*RunCreated, error) {
	var out RunCreated
	err := c.doAuthedJSON(ctx, http.MethodPost, "/api/runs", credential, rawBody{data: runJSON, contentType: "application/json"}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UploadRunFile uploads one file of the run folder (name relative to it: junit.xml, trace/...,
// att/..., logs/...).
func (c *Client) UploadRunFile(ctx context.Context, credential, runID, name string, data []byte) error {
	path := "/api/runs/" + url.PathEscape(runID) + "/files?name=" + url.QueryEscape(name)
	return c.doAuthedJSON(ctx, http.MethodPost, path, credential, rawBody{data: data, contentType: "application/octet-stream"}, nil)
}

// RunFilter selects runs for ListRuns ("" / 0 = no filter).
type RunFilter struct {
	Build, DUT, Suite, Device, Ref, Before string
	Limit                                  int
}

// RunSummary is one run in a list.
type RunSummary struct {
	ID           string         `json:"id"`
	URL          string         `json:"url"`
	Suite        string         `json:"suite"`
	DUT          string         `json:"dut"`
	Outcome      string         `json:"outcome"`
	Valid        bool           `json:"valid"`
	Trigger      string         `json:"trigger"`
	Started      string         `json:"started"`
	Finished     string         `json:"finished"`
	DurationMS   int64          `json:"duration_ms"`
	Counts       map[string]int `json:"counts"`
	BuildID      string         `json:"build_id"`
	DeviceID     string         `json:"device_id"`
	DeviceName   string         `json:"device_name"`
	UploadedBy   string         `json:"uploaded_by"`
	TestsRepo    string         `json:"tests_repo"`
	TestsRef     string         `json:"tests_ref"`
	TestsSHA     string         `json:"tests_sha"`
	Firmware     string         `json:"firmware"`
	GithubRunURL string         `json:"github_run_url"`
}

// RunList is a page of runs, newest first.
type RunList struct {
	Runs       []RunSummary `json:"runs"`
	NextBefore string       `json:"next_before"`
	OrgPods    int          `json:"org_pods"`
}

// ListRuns lists the organization's runs.
func (c *Client) ListRuns(ctx context.Context, credential string, f RunFilter) (*RunList, error) {
	q := url.Values{}
	for k, v := range map[string]string{"build": f.Build, "dut": f.DUT, "suite": f.Suite, "device": f.Device, "ref": f.Ref, "before": f.Before} {
		if strings.TrimSpace(v) != "" {
			q.Set(k, strings.TrimSpace(v))
		}
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	path := "/api/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out RunList
	if err := c.doAuthedJSON(ctx, http.MethodGet, path, credential, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRun returns a stored run.json, with the server's fields (id, url, build_id, files, ...).
func (c *Client) GetRun(ctx context.Context, credential, id string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.doAuthedJSON(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(id), credential, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CompareRun diffs run id (b) with with: "previous", another run's id, or "baseline:<branch>".
func (c *Client) CompareRun(ctx context.Context, credential, id, with string) (json.RawMessage, error) {
	var out json.RawMessage
	path := "/api/runs/" + url.PathEscape(id) + "/compare?with=" + url.QueryEscape(with)
	if err := c.doAuthedJSON(ctx, http.MethodGet, path, credential, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
