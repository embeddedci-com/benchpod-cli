package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type cloudFlashHit struct {
	path, auth, lease string
	body              map[string]any
}

// startCloudFlashServer is the Actions OIDC endpoint plus embeddedci.com's exchange and cloud
// flash route; status is the flash route's HTTP status.
func startCloudFlashServer(t *testing.T, status int) (*[]cloudFlashHit, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var hits []cloudFlashHit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/actions/token":
			_, _ = w.Write([]byte(`{"value":"oidc-jwt"}`))
		case "/api/github/oidc/token":
			_, _ = w.Write([]byte(`{"access_token":"gha-token","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
		case "/api/cloud/devices/flash":
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			mu.Lock()
			hits = append(hits, cloudFlashHit{r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("X-Benchpod-Lease"), m})
			mu.Unlock()
			if status != http.StatusOK {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"pod not allowed for this repository"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"output":"** Programming Finished **"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unexpected ` + r.URL.Path + `"}`))
		}
	}))
	t.Cleanup(srv.Close)
	noSignIn(t)
	t.Setenv("BENCHPOD_API_BASE", srv.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"/actions/token")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-secret")
	resetGitHubToken()
	t.Cleanup(resetGitHubToken)
	old := loadPodWiring
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring {
		clk, dio, nrst := 11, 12, true
		return &podWiring{SwdSwclk: &clk, SwdSwdio: &dio, SwdNreset: &nrst, SwdTarget: "target/stm32f4x.cfg"}
	}
	t.Cleanup(func() { loadPodWiring = old })
	return &hits, &mu
}

func TestFlashCloudRouteWithGitHubToken(t *testing.T) {
	hits, mu := startCloudFlashServer(t, http.StatusOK)
	cloudLeaseID = "L-run"
	t.Cleanup(func() { cloudLeaseID = "" })
	g := &globalFlags{connection: "embeddedci:benchpod-a1b2c3"}
	if err := runFlash(g, &flashFlags{repo: "acme/fw", ref: "main", build: "latest", targetPower: 1}); err != nil {
		t.Fatal(err)
	}
	if err := flashForCI(g, ciFlash{BuildID: "b-42", Timeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*hits) != 2 {
		t.Fatalf("hits %+v", *hits)
	}
	h := (*hits)[0]
	if h.path != "/api/cloud/devices/flash?device=benchpod-a1b2c3" || h.auth != "Bearer gha-token" || h.lease != "L-run" {
		t.Errorf("request %+v", h)
	}
	b := h.body
	if b["repo"] != "acme/fw" || b["ref"] != "main" || b["build"] != nil || b["target"] != "target/stm32f4x.cfg" ||
		b["swclk"].(float64) != 11 || b["swdio"].(float64) != 12 || b["nreset"] != true || b["efuse"].(float64) != 1 {
		t.Errorf("body %v", b)
	}
	if (*hits)[1].body["build"] != "b-42" {
		t.Errorf("ci flash body %v", (*hits)[1].body)
	}
}

func TestFlashCloudRouteRefusal(t *testing.T) {
	startCloudFlashServer(t, http.StatusForbidden)
	err := runFlash(&globalFlags{connection: "embeddedci:benchpod-a1b2c3"}, &flashFlags{build: "b-1"})
	if err == nil || !strings.Contains(err.Error(), "repository's list of pods") || !strings.Contains(err.Error(), "pod not allowed") {
		t.Fatalf("%v", err)
	}
}
