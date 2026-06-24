package openocd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func TestCopyOpenOCDToPod(t *testing.T) {
	// OpenOCD request frame: sig "DAP", len=3, type=request(0x01), rsv=0, payload.
	in := []byte{0x44, 0x41, 0x50, 0x00, 0x03, 0x00, 0x01, 0x00, 0xAA, 0xBB, 0xCC}
	var out bytes.Buffer
	if err := copyOpenOCDToPod(&out, bytes.NewReader(in)); !errors.Is(err, io.EOF) {
		t.Fatalf("copyOpenOCDToPod err = %v, want io.EOF at clean end", err)
	}
	// Pod frame: 2-byte len + payload (header stripped).
	want := []byte{0x03, 0x00, 0xAA, 0xBB, 0xCC}
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("pod frame = % x, want % x", out.Bytes(), want)
	}
}

func TestCopyOpenOCDToPodTwoFrames(t *testing.T) {
	in := []byte{
		0x44, 0x41, 0x50, 0x00, 0x01, 0x00, 0x01, 0x00, 0x11,
		0x44, 0x41, 0x50, 0x00, 0x02, 0x00, 0x01, 0x00, 0x22, 0x33,
	}
	var out bytes.Buffer
	if err := copyOpenOCDToPod(&out, bytes.NewReader(in)); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	want := []byte{0x01, 0x00, 0x11, 0x02, 0x00, 0x22, 0x33}
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("pod frames = % x, want % x", out.Bytes(), want)
	}
}

func TestCopyOpenOCDToPodBadSignature(t *testing.T) {
	in := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x00, 0x01, 0x00, 0x11}
	var out bytes.Buffer
	err := copyOpenOCDToPod(&out, bytes.NewReader(in))
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("bad packet signature")) {
		t.Fatalf("err = %v, want a bad-signature error", err)
	}
}

func TestCopyPodToOpenOCD(t *testing.T) {
	// Pod response frame: 2-byte len + payload.
	in := []byte{0x03, 0x00, 0xAA, 0xBB, 0xCC}
	var out bytes.Buffer
	if err := copyPodToOpenOCD(&out, bytes.NewReader(in)); !errors.Is(err, io.EOF) {
		t.Fatalf("copyPodToOpenOCD err = %v, want io.EOF at clean end", err)
	}
	// OpenOCD response frame: sig "DAP", len=3, type=response(0x02), rsv=0, payload.
	want := []byte{0x44, 0x41, 0x50, 0x00, 0x03, 0x00, 0x02, 0x00, 0xAA, 0xBB, 0xCC}
	if !bytes.Equal(out.Bytes(), want) {
		t.Errorf("openocd frame = % x, want % x", out.Bytes(), want)
	}
}

// TestDAPFramingRoundTrip pushes a request through copyOpenOCDToPod and the
// resulting pod frame straight back through copyPodToOpenOCD, proving the payload
// survives the header swap in both directions.
func TestDAPFramingRoundTrip(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	ocReq := append([]byte{0x44, 0x41, 0x50, 0x00, byte(len(payload)), 0x00, 0x01, 0x00}, payload...)

	var podFrame bytes.Buffer
	if err := copyOpenOCDToPod(&podFrame, bytes.NewReader(ocReq)); !errors.Is(err, io.EOF) {
		t.Fatalf("to pod: %v", err)
	}
	var ocResp bytes.Buffer
	if err := copyPodToOpenOCD(&ocResp, bytes.NewReader(podFrame.Bytes())); !errors.Is(err, io.EOF) {
		t.Fatalf("to openocd: %v", err)
	}
	got := ocResp.Bytes()
	if !bytes.Equal(got[dapTCPHeaderLen:], payload) {
		t.Errorf("round-trip payload = % x, want % x", got[dapTCPHeaderLen:], payload)
	}
}

func TestSupportsCMSISDAPTCP(t *testing.T) {
	orig := commandContext
	t.Cleanup(func() { commandContext = orig })

	// Exit 0: the probe's `shutdown` returns cleanly -> backend supported.
	commandContext = fakeExec("0")
	if ok, err := SupportsCMSISDAPTCP(context.Background(), "openocd"); err != nil || !ok {
		t.Errorf("SupportsCMSISDAPTCP supported = (%v, %v), want (true, nil)", ok, err)
	}

	// Exit 1: old OpenOCD rejects `cmsis-dap backend tcp` -> not supported, no error.
	commandContext = fakeExec("1")
	if ok, err := SupportsCMSISDAPTCP(context.Background(), "openocd"); err != nil || ok {
		t.Errorf("SupportsCMSISDAPTCP unsupported = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestBridgeDAP(t *testing.T) {
	tests := []struct {
		name     string
		exitCode string
		wantErr  bool
	}{
		{"success", "0", false},
		{"failure", "2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := commandContext
			t.Cleanup(func() { commandContext = orig })
			commandContext = fakeExec(tt.exitCode)

			podConn, testPod := net.Pipe()

			errCh := make(chan error, 1)
			go func() {
				errCh <- BridgeDAP(context.Background(), "openocd", nil, podConn, os.Stderr, os.Stderr)
			}()

			servePodDAPFrame(t, testPod)

			select {
			case err := <-errCh:
				if tt.wantErr && err == nil {
					t.Error("BridgeDAP: expected error from non-zero exit, got nil")
				}
				if !tt.wantErr && err != nil {
					t.Errorf("BridgeDAP: unexpected error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("BridgeDAP did not return in time")
			}
			testPod.Close()
		})
	}
}

// servePodDAPFrame plays the pod side of one BridgeDAP exchange: it reads the
// translated request frame (2-byte len + "DAPREQ") the bridge forwards from the
// fake OpenOCD, then writes one response frame (2-byte len + "DAPRESP").
func servePodDAPFrame(t *testing.T, testPod net.Conn) {
	t.Helper()
	_ = testPod.SetDeadline(time.Now().Add(5 * time.Second))
	lenHdr := make([]byte, 2)
	if _, err := readFull(testPod, lenHdr); err != nil {
		t.Fatalf("read pod request length: %v", err)
	}
	n := int(lenHdr[0]) | int(lenHdr[1])<<8
	payload := make([]byte, n)
	if _, err := readFull(testPod, payload); err != nil {
		t.Fatalf("read pod request payload: %v", err)
	}
	if string(payload) != "DAPREQ" {
		t.Errorf("pod received %q, want DAPREQ", payload)
	}
	resp := append([]byte{byte(len("DAPRESP")), 0x00}, "DAPRESP"...)
	if _, err := testPod.Write(resp); err != nil {
		t.Fatalf("write pod response: %v", err)
	}
}

// TestBridgeDAPTargetUnreachable checks that a no-target OpenOCD failure (the
// stderr markers) is surfaced as ErrTargetUnreachable, while an unrelated
// failure is not. The detection lives in runBridge, shared with the wifi/serial
// transports.
func TestBridgeDAPTargetUnreachable(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		wantUnreach bool
	}{
		{"cannot read IDR", "Error: cannot read IDR", true},
		{"connecting DP", "Error: Error connecting DP: not found", true},
		{"generic failure", "Error: some unrelated openocd failure", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := commandContext
			t.Cleanup(func() { commandContext = orig })
			commandContext = fakeExecStderr("1", tt.stderr)

			podConn, testPod := net.Pipe()
			errCh := make(chan error, 1)
			go func() {
				errCh <- BridgeDAP(context.Background(), "openocd", nil, podConn, io.Discard, io.Discard)
			}()

			servePodDAPFrame(t, testPod)

			select {
			case err := <-errCh:
				if err == nil {
					t.Fatal("BridgeDAP: expected error from non-zero exit, got nil")
				}
				if got := errors.Is(err, ErrTargetUnreachable); got != tt.wantUnreach {
					t.Errorf("errors.Is(err, ErrTargetUnreachable) = %v, want %v (err=%v)", got, tt.wantUnreach, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("BridgeDAP did not return in time")
			}
			testPod.Close()
		})
	}
}

// TestBridgeDAPNonNetConn proves BridgeDAP works over a plain io.ReadWriteCloser
// (the serial transport's stream), not just a net.Conn.
func TestBridgeDAPNonNetConn(t *testing.T) {
	orig := commandContext
	t.Cleanup(func() { commandContext = orig })
	commandContext = fakeExec("0")

	podConn, testPod := net.Pipe()
	wrapped := rwc{r: podConn, w: podConn, c: podConn}

	errCh := make(chan error, 1)
	go func() {
		errCh <- BridgeDAP(context.Background(), "openocd", nil, wrapped, os.Stderr, os.Stderr)
	}()

	servePodDAPFrame(t, testPod)

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("BridgeDAP over io.ReadWriteCloser: unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("BridgeDAP did not return in time")
	}
	testPod.Close()
}
