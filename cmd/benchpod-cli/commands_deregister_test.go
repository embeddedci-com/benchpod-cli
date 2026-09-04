package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/authstore"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// TestFindDeviceToDeregister covers device selection, the step that decides which physical pod
// gets detached: the public key wins when the pod could be asked for one, the name is the
// fallback, and neither is ever guessed at.
func TestFindDeviceToDeregister(t *testing.T) {
	devices := []serverapi.DeviceResponse{
		{ID: "id-a", Name: "bench-a", PublicKey: "key-a"},
		{ID: "id-b", Name: "bench-b", PublicKey: "key-b"},
	}

	t.Run("matches the attached pod by public key, not by name", func(t *testing.T) {
		// The pod was renamed in the UI, so only the key can identify it.
		got, err := findDeviceToDeregister(devices, "key-b", "")
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if got.ID != "id-b" {
			t.Fatalf("device = %q, want id-b", got.ID)
		}
	})

	t.Run("unknown public key is an error, never a guess", func(t *testing.T) {
		_, err := findDeviceToDeregister(devices, "key-z", "")
		if err == nil {
			t.Fatal("expected an error for a pod that is not registered to this account")
		}
	})

	t.Run("falls back to the name when there is no pod to ask", func(t *testing.T) {
		got, err := findDeviceToDeregister(devices, "", "bench-a")
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		if got.ID != "id-a" {
			t.Fatalf("device = %q, want id-a", got.ID)
		}
	})

	t.Run("unknown name is an error", func(t *testing.T) {
		if _, err := findDeviceToDeregister(devices, "", "bench-z"); err == nil {
			t.Fatal("expected an error for an unknown device name")
		}
	})
}

// TestRunDeregisterByName drives the whole --device-name path (no pod attached) against a fake
// server: the CLI must resolve the name through the device list and POST to the deregister
// endpoint of that device — and only that device.
func TestRunDeregisterByName(t *testing.T) {
	var deregistered []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/benchpod/devices":
			if got := r.Header.Get("Authorization"); got != "Bearer acc" {
				t.Errorf("Authorization = %q, want Bearer acc", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []serverapi.DeviceResponse{
				{ID: "id-a", Name: "bench-a", PublicKey: "key-a"},
				{ID: "id-b", Name: "bench-b", PublicKey: "key-b"},
			}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deregister"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/benchpod/devices/"), "/deregister")
			deregistered = append(deregistered, id)
			now := time.Now()
			_ = json.NewEncoder(w).Encode(serverapi.DeviceResponse{ID: id, Name: "bench-b", DisabledAt: &now})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	future := time.Now().Add(time.Hour)
	tokenPath := writeTokens(t, &authstore.Tokens{
		AccessToken: "acc", RefreshToken: "ref",
		AccessExpiresAt: future, RefreshExpiresAt: future,
		SessionID: "s1", UserID: "u1",
	})

	// No --connection is resolved on this path: an unreachable pod must not block deregistration.
	if err := runDeregister(&globalFlags{}, ts.URL, tokenPath, "bench-b", "", false); err != nil {
		t.Fatalf("runDeregister: %v", err)
	}
	if len(deregistered) != 1 || deregistered[0] != "id-b" {
		t.Fatalf("deregistered = %v, want [id-b]", deregistered)
	}
}

// TestRunDeregisterRejectsBothSelectors guards the ambiguous invocation rather than silently
// preferring one selector over the other.
func TestRunDeregisterRejectsBothSelectors(t *testing.T) {
	err := runDeregister(&globalFlags{}, "https://example.invalid", "", "bench-a", "id-b", false)
	if err == nil || !strings.Contains(err.Error(), "only one of") {
		t.Fatalf("err = %v, want a complaint about passing both selectors", err)
	}
}
