package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

func TestStatusSummaryIsReadable(t *testing.T) {
	var st podStatus
	raw := `{"device":"benchpod","version":"3.8.0","board":"stm32h563","board_rev":"v3","net":"ready",
		"ip":"192.168.1.221","rssi_dbm":null,"wifi":"off","cloud":"connected","la_vccio_mv":0,
		"gateware":48,"gateware_embedded":48,"lease":{"held":true,"holder":"ci-run-42","left_s":37}}`
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	p := targetPower{}
	p.Efuse1.Enabled = 1
	var out bytes.Buffer
	printStatusSummary(&out, statusSummary{
		addr: "192.168.1.221:8080", st: st, power: &p, latest: "v3.9.0",
		cloud:   &cloudState{known: true, Configured: true, State: "connected", DeviceID: "dev-1"},
		account: map[string]string{"dev-1": "bench-01"},
	})
	got := out.String()
	for _, want := range []string{
		"3.8.0 (v3.9.0 is available", "Gateware      48", "I/O voltage   not set (run `benchpod la voltage 3.3V`",
		"Target power  internal 5 V on", "192.168.1.221 (ready), Wi-Fi off",
		"registered, connected, on your account as bench-01", "held by cloud job ci-run-42 (37 s left)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "—") {
		t.Errorf("em-dash in output:\n%s", got)
	}
}

func TestStatusCloudLine(t *testing.T) {
	st := podStatus{Cloud: "disabled"}
	if got := describeStatusCloud(st, &cloudState{known: true}, nil); !strings.Contains(got, "not registered") {
		t.Errorf("got %q", got)
	}
	c := &cloudState{known: true, Configured: true, State: "Connected", DeviceID: "x"}
	if got := describeStatusCloud(st, c, nil); got != "registered, connected" {
		t.Errorf("got %q", got)
	}
	if got := describeStatusCloud(st, c, map[string]string{}); !strings.Contains(got, "different account") {
		t.Errorf("got %q", got)
	}
	if got := describeStatusCloud(st, nil, nil); got != "disabled" {
		t.Errorf("got %q", got)
	}
}

func TestVersionLess(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"3.8.0", "v3.9.0", true}, {"v3.8.0", "3.8.0", false}, {"3.10.0", "3.9.9", false},
		{"3.8.0-rc1", "3.8.1", true}, {"dev", "3.8.0", false}, {"3.8", "3.8.1", true},
	} {
		if got := versionLess(tc.a, tc.b); got != tc.want {
			t.Errorf("versionLess(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}

func TestLoginKeepsTheEmail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	tok, err := saveTokensFromResponse(path, &serverapi.TokenResponse{AccessToken: "a", RefreshToken: "r",
		AccessExpiresIn: 60, RefreshExpiresIn: 600, UserID: "u-1", Email: "you@example.com"}, nil)
	if err != nil || tok.Who() != "you@example.com" {
		t.Fatalf("got %v %+v", err, tok)
	}
	// A refresh from an older server sends no email: keep the one we had.
	tok, err = saveTokensFromResponse(path, &serverapi.TokenResponse{AccessToken: "b", RefreshToken: "r2",
		AccessExpiresIn: 60, RefreshExpiresIn: 600}, tok)
	if err != nil || tok.Email != "you@example.com" || tok.UserID != "u-1" {
		t.Fatalf("got %v %+v", err, tok)
	}
	who, _ := currentSession("", path)
	if who != "you@example.com" {
		t.Fatalf("currentSession = %q", who)
	}
	// No email at all (old token file): the user id.
	tok, _ = saveTokensFromResponse(path, &serverapi.TokenResponse{AccessToken: "c", RefreshToken: "r3",
		AccessExpiresIn: 60, RefreshExpiresIn: 600, UserID: "u-2"}, nil)
	if tok.Who() != "u-2" {
		t.Fatalf("got %q", tok.Who())
	}
}
