package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// fakeIdentityConsole stands in for the pod's USB console.
type fakeIdentityConsole struct {
	state   serialconsole.IdentityState
	newID   string
	wipeErr error
	calls   []string
}

func (f *fakeIdentityConsole) Identity(context.Context) (serialconsole.IdentityState, error) {
	f.calls = append(f.calls, "identity")
	return f.state, nil
}

func (f *fakeIdentityConsole) IdentityWipe(_ context.Context, token string) (string, error) {
	f.calls = append(f.calls, "identity-wipe "+token)
	if f.wipeErr != nil {
		return "", f.wipeErr
	}
	return f.newID, nil
}

func (f *fakeIdentityConsole) Close() error { return nil }

func withFakeIdentityConsole(t *testing.T, fc *fakeIdentityConsole, tty bool) {
	t.Helper()
	saved, savedTTY := openIdentityConsole, stdinIsTerminal
	openIdentityConsole = func(_ *globalFlags, device string, _ time.Duration) (identityConsole, string, context.Context, context.CancelFunc, error) {
		if device == "" {
			device = "/dev/cu.usbmodem1101"
		}
		return fc, device, context.Background(), func() {}, nil
	}
	stdinIsTerminal = func() bool { return tty }
	t.Cleanup(func() { openIdentityConsole, stdinIsTerminal = saved, savedTTY })
}

func TestIdentityWipeOnlyOverUSB(t *testing.T) {
	fc := &fakeIdentityConsole{}
	withFakeIdentityConsole(t, fc, true)
	for _, conn := range []string{"192.168.1.220", "192.168.1.220:8080", "benchpod-a1b2c3.local"} {
		err := runIdentityWipe(&globalFlags{connection: conn}, true, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "--connection usb") {
			t.Fatalf("connection %q: err = %v", conn, err)
		}
	}
	if len(fc.calls) != 0 {
		t.Fatalf("console used: %v", fc.calls)
	}
}

func TestIdentityWipeConfirms(t *testing.T) {
	fc := &fakeIdentityConsole{state: serialconsole.IdentityState{ShortID: "a1b2c3", PublicKey: "KEY"}, newID: "ed6313"}
	withFakeIdentityConsole(t, fc, true)
	g := &globalFlags{connection: "usb"}

	var out bytes.Buffer
	err := runIdentityWipe(g, false, strings.NewReader("n\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("declined: err = %v", err)
	}
	if !strings.Contains(out.String(), "benchpod-a1b2c3 (public key KEY)") {
		t.Fatalf("current identity not shown: %q", out.String())
	}
	if got := strings.Join(fc.calls, ","); got != "identity" {
		t.Fatalf("calls after decline = %s", got)
	}

	out.Reset()
	fc.calls = nil
	if err := runIdentityWipe(g, false, strings.NewReader("yes\n"), &out); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if got := strings.Join(fc.calls, ","); got != "identity,identity-wipe a1b2c3" {
		t.Fatalf("calls = %s", got)
	}
	for _, w := range []string{"now benchpod-ed6313", "benchpod deregister --device-name benchpod-a1b2c3",
		"benchpod register --connection <pod address> --device-name benchpod-a1b2c3", "ws_auth.v2", "wiring"} {
		if !strings.Contains(out.String(), w) {
			t.Fatalf("next steps miss %q:\n%s", w, out.String())
		}
	}
}

func TestIdentityWipeUnknownWithYes(t *testing.T) {
	fc := &fakeIdentityConsole{state: serialconsole.IdentityState{Problem: "unknown identity record (magic 0xc0ffee02 v2), not replaced"}, newID: "0f0f0f"}
	withFakeIdentityConsole(t, fc, false) // a script: no terminal, --yes given
	var out bytes.Buffer
	if err := runIdentityWipe(&globalFlags{connection: "/dev/cu.usbmodem9"}, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if got := strings.Join(fc.calls, ","); got != "identity,identity-wipe unknown" {
		t.Fatalf("calls = %s", got)
	}
	s := out.String()
	if !strings.Contains(s, "/dev/cu.usbmodem9: none: unknown identity record") ||
		!strings.Contains(s, "--device-name <old name>") || !strings.Contains(s, "BenchPod page") {
		t.Fatalf("output:\n%s", s)
	}
}

func TestIdentityWipeNeedsTerminalOrYes(t *testing.T) {
	fc := &fakeIdentityConsole{}
	withFakeIdentityConsole(t, fc, false)
	err := runIdentityWipe(&globalFlags{connection: "usb"}, false, strings.NewReader("y\n"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}
	if len(fc.calls) != 0 {
		t.Fatalf("console used: %v", fc.calls)
	}
}

func TestIdentityWipeRefusalAndOldFirmware(t *testing.T) {
	fc := &fakeIdentityConsole{state: serialconsole.IdentityState{ShortID: "a1b2c3"},
		wipeErr: &serialconsole.IdentityError{Cmd: "identity-wipe", Reason: "hardware RNG failed; nothing was erased"}}
	withFakeIdentityConsole(t, fc, true)
	err := runIdentityWipe(&globalFlags{connection: "usb"}, true, nil, &bytes.Buffer{})
	if err == nil || err.Error() != "identity wipe: the pod refused: hardware RNG failed; nothing was erased" {
		t.Fatalf("err = %v", err)
	}
	fc.wipeErr = serialconsole.ErrIdentityUnsupported
	err = runIdentityWipe(&globalFlags{connection: "usb"}, true, nil, &bytes.Buffer{})
	if err == nil || err.Error() != identityMissing {
		t.Fatalf("old firmware: err = %v", err)
	}
}

func TestIdentityShow(t *testing.T) {
	fc := &fakeIdentityConsole{state: serialconsole.IdentityState{Problem: "identity sector unreadable (flash ECC error)"}}
	withFakeIdentityConsole(t, fc, false)
	var out bytes.Buffer
	if err := runIdentityShow(&globalFlags{connection: "usb"}, &out); err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out.String(), "none: identity sector unreadable") || !strings.Contains(out.String(), "benchpod identity wipe --connection usb") {
		t.Fatalf("show = %q", out.String())
	}
	if err := runIdentityShow(&globalFlags{connection: "10.0.0.5"}, &out); err == nil {
		t.Fatal("show over the LAN was allowed")
	}
}
