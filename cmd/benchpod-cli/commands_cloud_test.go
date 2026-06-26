package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/authstore"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// TestDefaultDeviceNameFromAddr verifies the default device name is always URL-safe
// ([A-Za-z0-9._-], no colons) so the server's name validation accepts it.
func TestDefaultDeviceNameFromAddr(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"192.168.1.214:8080", "192.168.1.214"}, // the reported case: port stripped
		{"192.168.1.214:9000", "192.168.1.214"},
		{"192.168.1.214", "192.168.1.214"}, // no port
		{"benchpod.local:8080", "benchpod.local"},
		{"[fe80::1]:8080", "fe80--1"}, // IPv6: colons sanitized to dashes
		{":8080", "benchpod"},         // no host -> fallback
		{"", "benchpod"},
	}
	for _, c := range cases {
		got := defaultDeviceNameFromAddr(c.addr)
		if got != c.want {
			t.Errorf("defaultDeviceNameFromAddr(%q) = %q, want %q", c.addr, got, c.want)
		}
		// Defense: the result must never contain a colon or space.
		for _, r := range got {
			if r == ':' || r == ' ' {
				t.Errorf("defaultDeviceNameFromAddr(%q) = %q contains an invalid char", c.addr, got)
			}
		}
	}
}

// TestParseServerEndpoint covers the scheme→tls and the default-port mapping the
// firmware relies on to dial the server itself.
func TestParseServerEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantHost string
		wantPort int
		wantTLS  bool
		wantErr  bool
	}{
		{name: "https default port", raw: "https://www.embeddedci.com", wantHost: "www.embeddedci.com", wantPort: 443, wantTLS: true},
		{name: "wss default port", raw: "wss://www.embeddedci.com", wantHost: "www.embeddedci.com", wantPort: 443, wantTLS: true},
		{name: "http default port", raw: "http://localhost", wantHost: "localhost", wantPort: 80, wantTLS: false},
		{name: "ws default port", raw: "ws://localhost", wantHost: "localhost", wantPort: 80, wantTLS: false},
		{name: "explicit port over tls", raw: "https://example.com:8443", wantHost: "example.com", wantPort: 8443, wantTLS: true},
		{name: "explicit port plaintext", raw: "http://192.168.1.5:8080", wantHost: "192.168.1.5", wantPort: 8080, wantTLS: false},
		{name: "no host", raw: "https://", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://example.com", wantErr: true},
		{name: "bad port", raw: "http://example.com:notaport", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			host, port, tls, err := parseServerEndpoint(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseServerEndpoint(%q) = (%q,%d,%v,nil), want error", c.raw, host, port, tls)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseServerEndpoint(%q): unexpected error %v", c.raw, err)
			}
			if host != c.wantHost || port != c.wantPort || tls != c.wantTLS {
				t.Fatalf("parseServerEndpoint(%q) = (%q,%d,%v), want (%q,%d,%v)",
					c.raw, host, port, tls, c.wantHost, c.wantPort, c.wantTLS)
			}
		})
	}
}

// TestEnsureTokens exercises the four states of the cached-token lifecycle:
// a fresh access token (no network), an expired access token that refreshes
// against the server, an expired refresh token (must re-login, no network), and
// a missing token file (must log in, no network).
func TestEnsureTokens(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	t.Run("fresh access token returns cached without calling server", func(t *testing.T) {
		api, calls := refreshServer(t, http.StatusOK, nil)
		path := writeTokens(t, &authstore.Tokens{
			AccessToken: "acc", RefreshToken: "ref",
			AccessExpiresAt: future, RefreshExpiresAt: future,
			SessionID: "s1", UserID: "u1",
		})
		got, err := ensureTokens(context.Background(), api, path)
		if err != nil {
			t.Fatalf("ensureTokens: %v", err)
		}
		if got.AccessToken != "acc" {
			t.Fatalf("access token = %q, want acc", got.AccessToken)
		}
		if *calls != 0 {
			t.Fatalf("server was called %d times, want 0 for a fresh token", *calls)
		}
	})

	t.Run("expired access refreshes against server", func(t *testing.T) {
		api, calls := refreshServer(t, http.StatusOK, &serverapi.TokenResponse{
			AccessToken: "new-acc", RefreshToken: "new-ref",
			AccessExpiresIn: 3600, RefreshExpiresIn: 7200,
		})
		path := writeTokens(t, &authstore.Tokens{
			AccessToken: "old-acc", RefreshToken: "ref",
			AccessExpiresAt: past, RefreshExpiresAt: future,
			SessionID: "s1", UserID: "u1",
		})
		got, err := ensureTokens(context.Background(), api, path)
		if err != nil {
			t.Fatalf("ensureTokens: %v", err)
		}
		if got.AccessToken != "new-acc" || got.RefreshToken != "new-ref" {
			t.Fatalf("tokens = (%q,%q), want (new-acc,new-ref)", got.AccessToken, got.RefreshToken)
		}
		// The session/user carry over from the cached tokens when the refresh
		// response omits them.
		if got.SessionID != "s1" || got.UserID != "u1" {
			t.Fatalf("session/user = (%q,%q), want (s1,u1)", got.SessionID, got.UserID)
		}
		if *calls != 1 {
			t.Fatalf("server was called %d times, want 1", *calls)
		}
		// The refreshed tokens must be persisted for the next run.
		reloaded, err := authstore.Load(path)
		if err != nil {
			t.Fatalf("reload tokens: %v", err)
		}
		if reloaded.AccessToken != "new-acc" {
			t.Fatalf("persisted access token = %q, want new-acc", reloaded.AccessToken)
		}
	})

	t.Run("expired refresh requires login, no server call", func(t *testing.T) {
		api, calls := refreshServer(t, http.StatusOK, nil)
		path := writeTokens(t, &authstore.Tokens{
			AccessToken: "acc", RefreshToken: "ref",
			AccessExpiresAt: past, RefreshExpiresAt: past,
			SessionID: "s1", UserID: "u1",
		})
		if _, err := ensureTokens(context.Background(), api, path); err == nil {
			t.Fatal("ensureTokens: expected error for expired refresh token")
		}
		if *calls != 0 {
			t.Fatalf("server was called %d times, want 0 (stale refresh fenced off)", *calls)
		}
	})

	t.Run("missing token file requires login, no server call", func(t *testing.T) {
		api, calls := refreshServer(t, http.StatusOK, nil)
		path := filepath.Join(t.TempDir(), "does-not-exist.json")
		if _, err := ensureTokens(context.Background(), api, path); err == nil {
			t.Fatal("ensureTokens: expected error for missing token file")
		}
		if *calls != 0 {
			t.Fatalf("server was called %d times, want 0 for a missing token file", *calls)
		}
	})
}

// writeTokens persists t to a fresh temp token file and returns its path.
func writeTokens(t *testing.T, toks *authstore.Tokens) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token.json")
	if err := authstore.Save(path, toks); err != nil {
		t.Fatalf("save tokens: %v", err)
	}
	return path
}

// refreshServer returns a serverapi.Client pointed at an httptest server that
// answers /api/auth/guest/refresh with status and (when non-nil) resp, plus a
// counter of how many times it was hit.
func refreshServer(t *testing.T, status int, resp *serverapi.TokenResponse) (*serverapi.Client, *int) {
	t.Helper()
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(status)
		if resp != nil {
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	t.Cleanup(ts.Close)
	return serverapi.New(ts.URL), &calls
}
