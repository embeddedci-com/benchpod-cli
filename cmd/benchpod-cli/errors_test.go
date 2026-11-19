package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
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
	plain := errors.New("connect to pod at 10.0.0.1:8080: i/o timeout")
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

// Every code in the catalog: the exit code follows its kind, and the refusal shows the catalog's
// message and fix.cli with the args filled in, whatever the pod's text.
func TestEveryCodedRefusal(t *testing.T) {
	wantExit := map[tcpclient.RefusalKind]int{
		tcpclient.Refused: exitRefused, tcpclient.Locked: exitRefused,
		tcpclient.Forbidden: exitRefused, tcpclient.Busy: exitBusy,
	}
	args := map[string]any{"cmd": "x", "arg": "la", "name": "k66", "rev": 3, "max": 8, "bytes": 9000,
		"expected": 0, "feature": "i2c", "min": 55, "la": 4, "rx": 1, "tx": 2, "have": "up", "want": "down",
		"holder": "benchpod ci: alice", "purpose": "smoke", "left_s": 30, "by": "bob", "id": "L-1", "mv": 12,
		"client_api": 1, "min_client_api": 2, "api": 2}
	for _, code := range errcatalog.Codes() {
		t.Run(code, func(t *testing.T) {
			if code == tcpclient.CodeClientTooOld {
				// Not a *PodError: the update-required error (TestNewerPodFirmwareNeedsANewerCLI).
				if got := exitCode(tcpclient.RefusalError("", code, args)); got != exitUpdateCLI {
					t.Errorf("exit %d, want %d", got, exitUpdateCLI)
				}
				return
			}
			e, _ := errcatalog.Lookup(code)
			kind, ok := tcpclient.CodeKind(code)
			if !ok {
				t.Fatal("no kind")
			}
			pe := &tcpclient.PodError{Reason: "pod returned an error", Code: code, Args: args}
			if got := exitCode(fmt.Errorf("x: %w", pe)); got != wantExit[kind] {
				t.Errorf("exit %d, want %d", got, wantExit[kind])
			}
			fix := errcatalog.Fill(e.Fix["cli"], args)
			if got := refusalHint(pe, true); got != fix {
				t.Errorf("hint %q, want %q", got, fix)
			}
			msg := refusedError("cmd", pe, true).Error()
			if !strings.Contains(msg, code+": "+errcatalog.Fill(e.Message, args)) || !strings.Contains(msg, fix) {
				t.Errorf("refusedError = %q", msg)
			}
			if msg := withRefusalHint(fmt.Errorf("cmd: %w", pe)).Error(); !strings.Contains(msg, fix) {
				t.Errorf("withRefusalHint = %q", msg)
			}
		})
	}
}

// The text fallbacks still classify a refusal without a code as before (older firmware).
func TestUncodedRefusalsKeepTheirExitCodes(t *testing.T) {
	for id, want := range map[string]int{
		"lan_locked": exitRefused, "lan_locked_v39": exitRefused, "tunnel_forbidden": exitRefused,
		"lease_busy": exitBusy, "heavy_busy": exitBusy, "psram_bus_busy": exitBusy, "swd_busy": exitBusy,
		"uart_busy": exitBusy, "spi_busy": exitBusy, "swd_or_spi_busy": exitBusy, "unknown_cmd": exitRefused,
		"la_voltage_unset": exitRefused, "pin_conflict": exitRefused,
	} {
		if got := exitCode(&tcpclient.PodError{Reason: fwrefusals.Example(id)}); got != want {
			t.Errorf("%s: exit %d, want %d", id, got, want)
		}
	}
}

// One helper says "the firmware lacks this command": BP0002, or the old text.
func TestIsUnknownCommandHelper(t *testing.T) {
	if !isUnknownCommand(fmt.Errorf("x: %w", &tcpclient.PodError{Code: "BP0002", Reason: "pod returned an error"})) ||
		!isUnknownCommand(&tcpclient.PodError{Reason: fwrefusals.Example("unknown_cmd")}) ||
		isUnknownCommand(&tcpclient.PodError{Code: "BP0006", Reason: "missing la"}) ||
		isUnknownCommand(errors.New("unknown cmd")) {
		t.Fatal("isUnknownCommand")
	}
}

// unknownCmdReplies are a pod's answers to a command its firmware lacks: the text of firmware
// without codes, and BP0002 (with no text: a build without texts).
var unknownCmdReplies = []string{
	`{"status":"error","message":"unknown cmd"}`,
	`{"status":"error","code":"BP0002","args":{"cmd":"x"}}`,
}

// A pod whose firmware announces a newer API than this CLI handles stops a command that reads
// its status, with its own exit code; so does BP0030. A pod with api 1 or none works.
func TestNewerPodFirmwareNeedsANewerCLI(t *testing.T) {
	newer, _ := fakeLANPod(t, `{"status":"ok","data":{"version":"3.11.0","api":2,"min_client_api":0}}`)
	same, _ := fakeLANPod(t, `{"status":"ok","data":{"version":"3.10.14","api":1,"min_client_api":0}}`)
	old, _ := fakeLANPod(t, `{"status":"ok","data":{"version":"3.10.13"}}`)
	tooOld, _ := fakeLANPod(t, `{"status":"error","code":"BP0030","args":{"client_api":1,"min_client_api":2,"api":2},`+
		`"message":"update the BenchPod SDK, MCP or CLI: this firmware needs client API 2 or newer (this client sent 1)"}`)
	var logged strings.Builder
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	for _, c := range []struct {
		args []string
		want int
		msg  string
	}{
		{[]string{"status", "--connection", newer}, exitUpdateCLI,
			"this pod's firmware 3.11.0 needs a newer benchpod CLI (pod API 2, this CLI handles 1): brew upgrade --cask benchpod"},
		{[]string{"ping", "--connection", tooOld}, exitUpdateCLI,
			"this pod's firmware needs a newer benchpod CLI (pod API 2, this CLI handles 1): brew upgrade --cask benchpod"},
		{[]string{"ping", "--connection", newer}, exitOK, ""}, // ping reads no status
	} {
		logged.Reset()
		if got := run(c.args); got != c.want {
			t.Errorf("%v: exit %d, want %d (%s)", c.args, got, c.want, logged.String())
		}
		if !strings.Contains(logged.String(), c.msg) || (c.msg != "" && strings.Contains(logged.String(), "BP0030:")) {
			t.Errorf("%v: logged %q", c.args, logged.String())
		}
	}
	for _, addr := range []string{same, old} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := (&tcpclient.Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "status", "caps": false})
		cancel()
		if err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	if got := exitCode(fmt.Errorf("x: %w", &tcpclient.UpdateRequiredError{PodAPI: 2})); got != exitUpdateCLI {
		t.Errorf("wrapped: exit %d", got)
	}
}

// Every JSON command the CLI sends carries client_api and client ("cli <version>"), on the LAN and
// through embeddedci.com.
func TestEveryRequestCarriesTheClientFields(t *testing.T) {
	tcpclient.SetClientFields(true)
	defer tcpclient.SetClientFields(false)
	addr, got := fakeLANPod(t, `{"status":"ok","data":"pong"}`)
	if code := run([]string{"ping", "--connection", addr}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var req map[string]any
	_ = json.Unmarshal([]byte(<-got), &req)
	if req["client_api"] != float64(tcpclient.ClientAPI) || req["client"] != "cli "+strings.TrimPrefix(version, "v") {
		t.Fatalf("LAN request %v", req)
	}

	f := &fakeCloud{reply: podReplies(map[string]string{"ping": `"pong"`})}
	f.start(t)
	if code := run([]string{"ping", "--connection", "embeddedci:bench-1"}); code != exitOK {
		t.Fatalf("cloud exit %d", code)
	}
	if len(f.commands) == 0 || f.commands[0]["client_api"] != float64(tcpclient.ClientAPI) || f.commands[0]["client"] == nil {
		t.Fatalf("cloud request %v", f.commands)
	}
}
