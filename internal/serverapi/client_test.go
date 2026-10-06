package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterDeviceSendsPublicKey(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "dev-123", "name": "board-1", "owner_user_id": "u1",
			"public_key": gotBody["public_key"],
		})
	}))
	defer ts.Close()

	c := New(ts.URL)
	dev, err := c.RegisterDevice(context.Background(), "tok-abc", "board-1", "PUBKEY43", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	if dev.ID != "dev-123" {
		t.Fatalf("dev.ID = %q", dev.ID)
	}
	if gotPath != "/api/benchpod/devices" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-abc" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotBody["public_key"] != "PUBKEY43" {
		t.Fatalf("public_key in body = %v", gotBody["public_key"])
	}
	if gotBody["name"] != "board-1" {
		t.Fatalf("name in body = %v", gotBody["name"])
	}
}

func TestPodPolicyGetAndSet(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = nil
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		if r.Method == http.MethodPut && gotBody["policy"] == "audit" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sig_policy: only the USB console can loosen the policy (now required)"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"policy": "required", "supported": true})
	}))
	defer ts.Close()
	c := New(ts.URL)

	p, err := c.GetPodPolicy(context.Background(), "tok", "dev 1", "sig-policy")
	if err != nil || p.Policy != "required" || !p.Supported {
		t.Fatalf("get = %+v, %v", p, err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/benchpod/devices/dev 1/sig-policy" {
		t.Fatalf("request = %s %s", gotMethod, gotPath)
	}

	if _, err := c.SetPodPolicy(context.Background(), "tok", "dev1", "sig-policy", "required"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if gotMethod != http.MethodPut || gotBody["policy"] != "required" {
		t.Fatalf("request = %s %v", gotMethod, gotBody)
	}

	_, err = c.SetPodPolicy(context.Background(), "tok", "dev1", "sig-policy", "audit")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict ||
		apiErr.Message != "sig_policy: only the USB console can loosen the policy (now required)" {
		t.Fatalf("err = %#v", err)
	}

	if _, err := c.GetPodPolicy(context.Background(), "tok", "dev1", "wifi-policy"); err == nil {
		t.Fatal("expected an error for an unknown policy")
	}
}
