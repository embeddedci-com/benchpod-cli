package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestLogLevelShow(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"level":"info"}}`)
	var out bytes.Buffer
	if err := runLogLevel(&globalFlags{connection: addr}, "", &out); err != nil || !strings.Contains(out.String(), ": info") {
		t.Fatalf("show: %v %q", err, out.String())
	}
	if req := <-got; req != `{"cmd":"log_level"}` {
		t.Fatalf("request %s", req)
	}
}

func TestLogLevelSet(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"level":"debug"}}`)
	var out bytes.Buffer
	if err := runLogLevel(&globalFlags{connection: addr}, "debug", &out); err != nil || !strings.Contains(out.String(), "now debug") {
		t.Fatalf("set: %v %q", err, out.String())
	}
	if req := <-got; !strings.Contains(req, `"cmd":"log_level"`) || !strings.Contains(req, `"set":"debug"`) {
		t.Fatalf("request %s", req)
	}
}

func TestLogLevelOnOlderFirmware(t *testing.T) {
	for _, reply := range unknownCmdReplies {
		addr, _ := fakeLANPod(t, reply)
		err := runLogLevel(&globalFlags{connection: addr}, "", &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "newer than 3.10.12") {
			t.Fatalf("got %v", err)
		}
	}
}

func TestLogLevelRefusesOtherNames(t *testing.T) {
	cmd := newLogLevelCmd(&globalFlags{connection: "127.0.0.1:1"})
	cmd.SetArgs([]string{"verbose"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "error, warn, info or debug") {
		t.Fatalf("got %v", err)
	}
}
