package serverapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCloudLeaseShapes(t *testing.T) {
	type hit struct {
		path, query, auth, lease string
		body                     map[string]any
	}
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		hits = append(hits, hit{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("X-Benchpod-Lease"), body})
		switch r.URL.Path {
		case "/api/auth/token":
			_, _ = w.Write([]byte(`{"token":"sess","expires_in":3600}`))
		case "/api/cloud/devices/lease/renew":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"busy":true,"holder":"web: alice","expires_at":"2026-11-18T14:00:00Z","code":"lease_held"}`))
		default:
			_, _ = w.Write([]byte(`{"lease_id":"L-1"}`))
		}
	}))
	defer srv.Close()
	api := New(srv.URL)
	api.LeaseID = "L-1"
	ctx := context.Background()
	s, err := api.MintCloudSession(ctx, "ApiKey eci_x", 3600)
	if err != nil || s.Token != "sess" {
		t.Fatalf("%+v %v", s, err)
	}
	busy, err := api.CloudLease(ctx, s.Token, "", "benchpod-a1b2c3", LeaseRequest{LeaseID: "L-1", RunLabel: "benchpod ci: me", TTLSeconds: 300})
	if err != nil || busy != nil {
		t.Fatalf("acquire %v %v", busy, err)
	}
	busy, err = api.CloudLease(ctx, s.Token, "renew", "benchpod-a1b2c3", LeaseRequest{LeaseID: "L-1", TTLSeconds: 300})
	if err != nil || busy == nil || busy.Holder != "web: alice" {
		t.Fatalf("renew %+v %v", busy, err)
	}
	if hits[0].auth != "ApiKey eci_x" || hits[0].body["ttl_seconds"].(float64) != 3600 {
		t.Errorf("token exchange %+v", hits[0])
	}
	acq := hits[1]
	if acq.path != "/api/cloud/devices/lease" || acq.query != "device=benchpod-a1b2c3" || acq.auth != "Bearer sess" ||
		acq.body["lease_id"] != "L-1" || acq.body["run_label"] != "benchpod ci: me" || acq.body["ttl_seconds"].(float64) != 300 || acq.lease != "L-1" {
		t.Errorf("acquire request %+v", acq)
	}
}
