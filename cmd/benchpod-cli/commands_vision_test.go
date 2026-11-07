package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestVisionDefaultName(t *testing.T) {
	if got := visionDefaultName("AI-Vision-2-5dcec6.local", "x"); got != "ai-vision-2-5dcec6" {
		t.Fatalf("host name: %q", got)
	}
	// "AQID" is bytes 01 02 03
	if got := visionDefaultName("", "AQIDBA"); got != "ai-vision-010203" {
		t.Fatalf("from the key: %q", got)
	}
}

func TestVisionPairServer(t *testing.T) {
	for _, c := range []struct {
		host     string
		port     int
		tls, exp bool
		want     string
	}{
		{"api.embeddedci.com", 443, true, false, "api.embeddedci.com"},
		{"127.0.0.1", 18291, false, false, "127.0.0.1:18291"},
		{"my.host", 8443, true, true, "my.host"},
	} {
		if got := visionPairServer(c.host, c.port, c.tls, c.exp); got != c.want {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

func TestNewBoardClient(t *testing.T) {
	b, err := newBoardClient("10.1.0.165", "")
	if err != nil || b.base != "http://10.1.0.165" {
		t.Fatalf("%+v %v", b, err)
	}
	if b, err = newBoardClient("https://vision.example:8443/x", "tk"); err != nil || b.base != "https://vision.example:8443" || b.token != "tk" {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := newBoardClient("embeddedci:bench", ""); err == nil {
		t.Fatal("a cloud name was taken as an address")
	}
}

// register reads the board's key, registers it with the API key, and pairs the board.
func TestVisionRegisterRegistersAndPairs(t *testing.T) {
	var mu sync.Mutex
	var regBody map[string]string
	var regAuth string
	var pairBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/aivision/devices" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		regAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&regBody)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"dev-1","name":"ai-vision-2-5dcec6","connection":"embeddedci:ai-vision-2-5dcec6"}`)
	}))
	defer server.Close()
	board := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/status":
			_, _ = io.WriteString(w, `{"hostname":"ai-vision-2-5dcec6","public_key":"AQIDBAUGBwgJCgsMDQ4PEBESExQVFhcYGRobHB0eHyA"}`)
		case "PUT /api/v1/cloud":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&pairBody)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"paired":true}`)
		case "GET /api/v1/cloud":
			_, _ = io.WriteString(w, `{"connected":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer board.Close()
	t.Setenv("BENCHPOD_API_KEY", "eci_test")

	o := visionRegisterOptions{board: board.URL, wait: 2e9}
	o.serverURL = server.URL
	if err := runVisionRegister(o); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if regAuth != "ApiKey eci_test" || regBody["name"] != "ai-vision-2-5dcec6" || !strings.HasPrefix(regBody["public_key"], "AQID") {
		t.Fatalf("registration: %q %+v", regAuth, regBody)
	}
	wantServer := strings.TrimPrefix(server.URL, "http://")
	if pairBody["device_id"] != "dev-1" || pairBody["server"] != wantServer {
		t.Fatalf("pairing: %+v, want server %s", pairBody, wantServer)
	}
}
