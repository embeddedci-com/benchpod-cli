package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// A coded refusal is worded from the catalog once; a refusal without a code is shown as the pod
// wrote it, with no hint (the CLI never reads a refusal's text).
func TestWithRefusalHint(t *testing.T) {
	pe := &tcpclient.PodError{Reason: "The pod's LAN is locked: flash needs the cloud or USB", Code: "BP0040",
		Args: map[string]any{"cmd": "flash"}}
	err := withRefusalHint(fmt.Errorf("flash: %w", pe))
	want := "flash: BP0040: The pod's LAN is locked: flash needs the cloud or USB; run this over USB with --connection usb, or from the cloud (" +
		errcatalog.DocsURL("BP0040") + ")"
	if err.Error() != want {
		t.Fatalf("got %q\nwant %q", err, want)
	}
	if _, ok := tcpclient.AsPodError(err); !ok {
		t.Fatal("the hint hid the refusal")
	}
	once := refusedError("flash", pe)
	if got := withRefusalHint(once).Error(); got != once.Error() {
		t.Fatalf("got %q", got)
	}
	plainRefusal := &tcpclient.PodError{Reason: "locked: flash needs the cloud or USB"}
	if got := withRefusalHint(plainRefusal); got != error(plainRefusal) {
		t.Fatalf("uncoded: got %v", got)
	}
	if got := refusedError("generate", &tcpclient.PodError{Reason: "unknown waveform"}).Error(); got != "generate: the pod refused: unknown waveform" {
		t.Fatalf("uncoded refusedError: %q", got)
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
	reply := func(code string) string {
		b, _ := json.Marshal(map[string]any{"status": "error", "code": code, "args": map[string]any{"cmd": "ping"}})
		return string(b)
	}
	locked, _ := fakeLANPod(t, reply("BP0040"))
	leased, _ := fakeLANPod(t, reply("BP0025"))
	swd, _ := fakeLANPod(t, reply("BP0050"))
	forbidden, _ := fakeLANPod(t, reply("BP0045"))
	gateware, _ := fakeLANPod(t, reply("BP0017"))
	uncoded, _ := fakeLANPod(t, `{"status":"error","message":"busy: anything"}`)
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
		{[]string{"ping", "--connection", gateware}, exitUpdatePod},
		{[]string{"ping", "--connection", uncoded}, exitRefused}, // its text is never read
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
		{cloudCfgError("cloud ca set", caMissing, true, &tcpclient.PodError{Reason: "x", Code: "BP0040"}), exitRefused},
		{cloudCfgError("cloud proxy clear", proxyMissing, true, &tcpclient.PodError{Reason: "x", Code: "BP0042"}), exitRefused},
		{cloudCfgError("cloud ca set", caMissing, false, &tcpclient.PodError{Reason: "x", Code: "BP0051"}), exitBusy},
		{cloudPolicyError(lanPolicy, "off", &serverapi.APIError{Status: http.StatusConflict, Code: "BP0041", Message: "BP0041"}), exitRefused},
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
		tcpclient.FirmwareTooOld: exitUpdatePod, tcpclient.ClientTooOld: exitUpdateCLI,
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
			pe, _ := tcpclient.AsPodError(tcpclient.RefusalError("", code, args)) // no message: 4.0
			if got := exitCode(fmt.Errorf("x: %w", pe)); got != wantExit[kind] {
				t.Errorf("exit %d, want %d", got, wantExit[kind])
			}
			fix := errcatalog.Fill(e.Fix["cli"], args)
			if got := refusalHint(pe); got != fix {
				t.Errorf("hint %q, want %q", got, fix)
			}
			msg := refusedError("cmd", pe).Error()
			if !strings.Contains(msg, code+": "+errcatalog.Fill(e.Message, args)) || !strings.Contains(msg, fix) {
				t.Errorf("refusedError = %q", msg)
			}
			if msg := withRefusalHint(fmt.Errorf("cmd: %w", pe)).Error(); !strings.Contains(msg, fix) {
				t.Errorf("withRefusalHint = %q", msg)
			}
		})
	}
}

// "The firmware lacks this command" is BP0002 only.
func TestIsUnknownCommandHelper(t *testing.T) {
	if !isUnknownCommand(fmt.Errorf("x: %w", &tcpclient.PodError{Code: "BP0002"})) ||
		isUnknownCommand(&tcpclient.PodError{Reason: "unknown cmd"}) ||
		isUnknownCommand(&tcpclient.PodError{Code: "BP0006", Reason: "missing la"}) ||
		isUnknownCommand(errors.New("unknown cmd")) {
		t.Fatal("isUnknownCommand")
	}
}

// unknownCmdReplies are a pod's answers to a command its firmware lacks: BP0002, with no message.
var unknownCmdReplies = []string{
	`{"status":"error","code":"BP0002","args":{"cmd":"x"}}`,
}

// fakeLANPodByCmd is a LAN pod that answers each command by name ("" for any other).
func fakeLANPodByCmd(t *testing.T, replies map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			var req map[string]any
			_ = json.Unmarshal([]byte(line), &req)
			cmd, _ := req["cmd"].(string)
			reply, ok := replies[cmd]
			if !ok {
				reply = replies[""]
			}
			_, _ = io.WriteString(conn, reply+"\n")
			conn.Close()
		}
	}()
	return ln.Addr().String()
}

// A pod whose firmware announces a newer API than this CLI handles, or one older than firmware 4.0
// (api 1, or none), stops a command with its own exit code (6, 7); so does BP0030. The firmware
// commands (status and the update commands) still run against it.
func TestPodAPILevels(t *testing.T) {
	apiProbe = true
	defer func() { apiProbe = false; tcpclient.ResetAPIProbe() }()
	pod := func(status string) string {
		return fakeLANPodByCmd(t, map[string]string{"status": `{"status":"ok","data":` + status + `}`, "": `{"status":"ok","data":"pong"}`})
	}
	newer := pod(`{"version":"5.0.0","api":3,"min_client_api":3}`)
	same := pod(`{"version":"4.0.0","api":2,"min_client_api":2}`)
	old := pod(`{"version":"3.10.13"}`)
	dev := pod(`{"version":"3.10.14","api":1,"min_client_api":0}`)
	tooOld, _ := fakeLANPod(t, `{"status":"error","code":"BP0030","args":{"client_api":1,"min_client_api":2,"api":2},`+
		`"message":"Update the BenchPod SDK, MCP or CLI: this firmware needs client API 2 or newer (this client sent 1)"}`)
	var logged strings.Builder
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	for _, c := range []struct {
		args []string
		want int
		msg  string
	}{
		// status still shows the pod (it is how the user finds out), with the line as a warning.
		{[]string{"status", "--connection", newer}, exitOK,
			"Warning: this pod's firmware 5.0.0 needs a newer benchpod CLI (pod API 3, this CLI handles 2): brew upgrade --cask benchpod"},
		{[]string{"status", "--connection", old}, exitOK,
			"Warning: this pod's firmware 3.10.13 is too old for this benchpod CLI (pod API 1, this CLI needs 2): this pod needs firmware 4.0: benchpod flash-self"},
		{[]string{"ping", "--connection", newer}, exitUpdateCLI, "needs a newer benchpod CLI"},
		{[]string{"ping", "--connection", old}, exitUpdatePod, "this pod needs firmware 4.0: benchpod flash-self"},
		{[]string{"ping", "--connection", dev}, exitUpdatePod, "this pod needs firmware 4.0: benchpod flash-self"},
		{[]string{"ping", "--connection", same}, exitOK, ""},
		{[]string{"ping", "--connection", tooOld}, exitUpdateCLI,
			"this pod's firmware needs a newer benchpod CLI (pod API 2, this CLI handles 2): brew upgrade --cask benchpod"},
	} {
		logged.Reset()
		if got := run(c.args); got != c.want {
			t.Errorf("%v: exit %d, want %d (%s)", c.args, got, c.want, logged.String())
		}
		if !strings.Contains(logged.String(), c.msg) || (c.msg != "" && strings.Contains(logged.String(), "BP0030:")) {
			t.Errorf("%v: logged %q", c.args, logged.String())
		}
	}
	if got := exitCode(fmt.Errorf("x: %w", &tcpclient.UpdateRequiredError{PodAPI: 3})); got != exitUpdateCLI {
		t.Errorf("wrapped: exit %d", got)
	}
	if got := exitCode(fmt.Errorf("x: %w", &tcpclient.FirmwareTooOldError{PodAPI: 1})); got != exitUpdatePod {
		t.Errorf("wrapped: exit %d", got)
	}
	_ = context.Background
	_ = time.Second
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
