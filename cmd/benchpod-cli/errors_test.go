package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// explainRefusal is the one place a policy refusal gets its hint; the texts come from the
// firmware (internal/fwrefusals).
func TestExplainRefusal(t *testing.T) {
	for _, c := range []struct {
		id      string
		overLAN bool
		want    string
	}{
		{"lan_locked", true, lockedUSBHint},
		{"lan_locked", false, ""},
		{"lease_busy", true, leaseBusyHint},
		{"lease_busy", false, ""},
		{"heavy_busy", true, heavyBusyHint},
		{"heavy_busy", false, heavyBusyHint},
		{"tunnel_forbidden", false, forbiddenHint},
		{"psram_bus_busy", true, ""}, // says what to do already
		{"swd_busy_spi_session", true, ""},
		{"unknown_cmd", true, ""},
	} {
		if got := explainRefusal(fwrefusals.Example(c.id), c.overLAN); got != c.want {
			t.Errorf("%s (LAN %v): got %q, want %q", c.id, c.overLAN, got, c.want)
		}
	}
}

func TestWithRefusalHintAddsTheHintOnce(t *testing.T) {
	msg := fwrefusals.Example("lan_locked")
	err := withRefusalHint(fmt.Errorf("la pullup: %w", &tcpclient.PodError{Reason: msg}))
	if got := err.Error(); got != "la pullup: "+msg+" ("+lockedUSBHint+")" {
		t.Fatalf("got %q", got)
	}
	if _, ok := tcpclient.AsPodError(err); !ok {
		t.Fatal("the hint hid the refusal")
	}
	// Already explained (refusedError): left alone.
	once := refusedError("cloud ca set", &tcpclient.PodError{Reason: msg}, true)
	if got := withRefusalHint(once).Error(); strings.Count(got, lockedUSBHint) != 1 {
		t.Fatalf("got %q", got)
	}
	// A console refusal did not come over the LAN, and other errors pass through.
	usb := &tcpclient.PodError{Cmd: "proxy-set", Reason: msg}
	if got := withRefusalHint(usb); got != error(usb) {
		t.Fatalf("got %v", got)
	}
	plain := errors.New("connect to bench pod at 10.0.0.1:8080: i/o timeout")
	if got := withRefusalHint(plain); got != plain {
		t.Fatalf("got %v", got)
	}
}

// The exit code tells a script what went wrong; run with real arguments against a fake pod.
func TestExitCodes(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	t.Setenv("BENCHPOD_TIMEOUT", "")
	reply := func(id string) string {
		b, _ := json.Marshal(map[string]string{"status": "error", "message": fwrefusals.Example(id)})
		return string(b)
	}
	locked, _ := fakeLANPod(t, reply("lan_locked"))
	leased, _ := fakeLANPod(t, reply("lease_busy"))
	swd, _ := fakeLANPod(t, reply("swd_busy"))
	forbidden, _ := fakeLANPod(t, reply("tunnel_forbidden"))
	ok, _ := fakeLANPod(t, `{"status":"ok","data":"pong"}`)
	// A port nothing listens on: grab one, then close it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	ln.Close()

	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"ping", "--connection", ok}, exitOK},
		{[]string{"ping", "--connection", ok, "--no-such-flag"}, exitUsage},
		{[]string{"ping", "--connection", ok, "extra"}, exitUsage},
		{[]string{"no-such-command"}, exitUsage},
		{[]string{"la", "pullup", "1"}, exitUsage},
		{[]string{"ping", "--connection", locked}, exitRefused},
		{[]string{"ping", "--connection", forbidden}, exitRefused},
		{[]string{"ping", "--connection", leased}, exitBusy},
		{[]string{"ping", "--connection", swd}, exitBusy},
		{[]string{"ping", "--connection", closed, "--timeout", "1s"}, exitUnreachable},
	} {
		if got := run(c.args); got != c.want {
			t.Errorf("%v: exit %d, want %d", c.args, got, c.want)
		}
	}
}

func TestExitCodeOfWrappedErrors(t *testing.T) {
	if got := exitCode(fmt.Errorf("x: %w", usageErrorf("--voltage: bad"))); got != exitUsage {
		t.Errorf("usage: %d", got)
	}
	if got := exitCode(fmt.Errorf("x: %w", &tcpclient.PodError{Cmd: "proxy-set", Reason: "w25q write failed"})); got != exitRefused {
		t.Errorf("console refusal: %d", got)
	}
	if got := exitCode(errors.New("something else")); got != exitError {
		t.Errorf("other: %d", got)
	}
}

// A refusal worded for the user still exits as a refusal.
func TestShownRefusalsKeepTheirExitCode(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{cloudCfgError("cloud ca set", caMissing, true, &tcpclient.PodError{Reason: fwrefusals.Example("lan_locked")}), exitRefused},
		{cloudCfgError("cloud proxy clear", proxyMissing, true, &tcpclient.PodError{Reason: fwrefusals.Example("cloud_proxy_from_lan")}), exitRefused},
		{cloudCfgError("cloud ca set", caMissing, false, &tcpclient.PodError{Cmd: "ca", Reason: fwrefusals.Example("ota_owner_busy")}), exitBusy},
		{cloudPolicyError(sigPolicy, "audit", &serverapi.APIError{Status: http.StatusConflict, Message: fwrefusals.Example("sig_policy_loosen")}), exitRefused},
		{cloudPolicyError(lanPolicy, "off", &serverapi.APIError{Status: http.StatusForbidden}), exitRefused},
	} {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("%v: exit %d, want %d", c.err, got, c.want)
		}
		if got := withRefusalHint(c.err); got.Error() != c.err.Error() {
			t.Errorf("hint added twice: %v", got)
		}
	}
}
