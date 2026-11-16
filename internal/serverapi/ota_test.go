package serverapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartOTAUpload(t *testing.T) {
	var got struct {
		path, query, contentType, sig, org string
		body                               []byte
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.query = r.URL.Path, r.URL.RawQuery
		got.contentType, got.sig, got.org = r.Header.Get("Content-Type"), r.Header.Get(OTASignatureHeader), r.Header.Get(OrgHeader)
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"job_id": "j1", "phase": "streaming", "live": true, "commit_after": false})
	}))
	defer ts.Close()
	c := New(ts.URL)
	c.Org = orgLab
	job, err := c.StartOTA(context.Background(), "tok", "dev 1", OTARequest{
		Image: []byte("IMAGE"), Signature: []byte{1, 2, 3}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if job.JobID != "j1" || job.Terminal() {
		t.Errorf("job %+v", job)
	}
	if got.path != "/api/benchpod/devices/dev 1/ota" || got.query != "commit=false" {
		t.Errorf("path %q query %q", got.path, got.query)
	}
	if string(got.body) != "IMAGE" || got.contentType != "application/octet-stream" {
		t.Errorf("body %q type %q", got.body, got.contentType)
	}
	if got.sig != base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}) || got.org != orgLab {
		t.Errorf("sig %q org %q", got.sig, got.org)
	}

	// A release goes by tag, with no body and no signature header.
	if _, err := c.StartOTA(context.Background(), "tok", "d", OTARequest{Release: "v3.10.1"}); err != nil {
		t.Fatal(err)
	}
	if got.query != "release=v3.10.1" || len(got.body) != 0 || got.sig != "" {
		t.Errorf("release: query %q body %q sig %q", got.query, got.body, got.sig)
	}
	if _, err := c.StartOTA(context.Background(), "tok", "d", OTARequest{}); err == nil {
		t.Error("an empty request was sent")
	}
}

func TestStartOTARefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"installing firmware needs an organization owner or admin","code":"forbidden_tier"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).StartOTA(context.Background(), "tok", "d", OTARequest{Image: []byte{1}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "forbidden_tier" || apiErr.Status != http.StatusForbidden {
		t.Fatalf("err = %#v", err)
	}
}

func TestGetAndAbortOTA(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"phase":"verified","percent":100,"version_before":"3.10.1"}`))
	}))
	defer ts.Close()
	c := New(ts.URL)
	job, err := c.GetOTA(context.Background(), "tok", "d")
	if err != nil || job.Phase != OTAPhaseVerified || !job.Terminal() || job.VersionBefore != "3.10.1" {
		t.Fatalf("job %+v %v", job, err)
	}
	if err := c.AbortOTA(context.Background(), "tok", "d"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "GET /api/benchpod/devices/d/ota" || paths[1] != "POST /api/benchpod/devices/d/ota/abort" {
		t.Errorf("paths %q", paths)
	}
}
