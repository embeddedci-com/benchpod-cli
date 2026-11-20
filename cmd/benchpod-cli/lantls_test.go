package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
)

// lan-tls scpi and https-only send the lan_tls switches; the LAN refusal (BP0042) says to use USB
// or the cloud.
func TestLanTLSSwitches(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"port":8443,"https_only":true,"scpi":true,"sessions":0,`+
		`"origin":"generated","key":"EC P-256","subject":"CN=benchpod-a1b2c3","sha256":"ab"}}`)
	out := t.TempDir() + "/out.txt"
	for _, c := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"lan-tls", "scpi", "on"}, map[string]any{"cmd": "lan_tls", "scpi": true}},
		{[]string{"lan-tls", "https-only", "off"}, map[string]any{"cmd": "lan_tls", "https_only": false}},
		{[]string{"lan-tls"}, map[string]any{"cmd": "lan_tls"}},
	} {
		if code := run(append(c.args, "--connection", addr, "--out", out)); code != exitOK {
			t.Fatalf("%v: exit %d", c.args, code)
		}
		var req map[string]any
		_ = json.Unmarshal([]byte(<-got), &req)
		if len(req) != len(c.want) {
			t.Fatalf("%v: sent %v", c.args, req)
		}
		for k, v := range c.want {
			if req[k] != v {
				t.Fatalf("%v: sent %v", c.args, req)
			}
		}
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "  scpi        on\n") || !strings.Contains(string(b), "  https-only  on\n") {
		t.Fatalf("show:\n%s", b)
	}
	if code := run([]string{"lan-tls", "scpi", "maybe", "--connection", addr}); code != exitUsage {
		t.Fatalf("bad value: exit %d", code)
	}

	refused, _ := fakeLANPod(t, `{"status":"error","code":"BP0042","args":{"setting":"lan_tls"}}`)
	var logged strings.Builder
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	if code := run([]string{"lan-tls", "scpi", "on", "--connection", refused}); code != exitRefused {
		t.Fatalf("LAN: exit %d", code)
	}
	if !strings.Contains(logged.String(), "BP0042: lan_tls cannot be changed over the LAN") || !strings.Contains(logged.String(), "--connection usb") {
		t.Fatalf("LAN refusal: %s", logged.String())
	}
}
