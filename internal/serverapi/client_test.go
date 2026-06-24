package serverapi

import (
	"context"
	"encoding/json"
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
