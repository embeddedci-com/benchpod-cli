package openocd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestFind(t *testing.T) {
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })

	lookPath = func(string) (string, error) { return "/usr/bin/openocd", nil }
	if bin, err := Find(); err != nil || bin != "/usr/bin/openocd" {
		t.Errorf("Find() = (%q, %v), want (/usr/bin/openocd, nil)", bin, err)
	}

	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := Find(); err == nil {
		t.Error("Find() with missing binary: expected error, got nil")
	} else if !strings.Contains(err.Error(), "install openocd") {
		t.Errorf("Find() error = %q, want it to mention installing openocd", err.Error())
	}
}

func TestValidate(t *testing.T) {
	orig := commandContext
	t.Cleanup(func() { commandContext = orig })

	commandContext = fakeExec("0")
	if err := Validate(context.Background(), "openocd"); err != nil {
		t.Errorf("Validate() with working binary: %v", err)
	}

	commandContext = fakeExec("1")
	if err := Validate(context.Background(), "openocd"); err == nil {
		t.Error("Validate() with failing binary: expected error, got nil")
	}
}

// rwc wraps a net.Conn but exposes ONLY io.ReadWriteCloser, proving the bridge
// does not depend on any net.Conn-specific methods (the serial transport hands
// it a non-net.Conn stream). Used by TestBridgeDAPNonNetConn.
type rwc struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

func (x rwc) Read(p []byte) (int, error)  { return x.r.Read(p) }
func (x rwc) Write(p []byte) (int, error) { return x.w.Write(p) }
func (x rwc) Close() error                { return x.c.Close() }

func readFull(c net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := c.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// fakeExec returns a commandContext seam that re-invokes the test binary in
// helper-process mode (TestHelperProcess), forwarding the requested exit code.
func fakeExec(exitCode string) func(context.Context, string, ...string) *exec.Cmd {
	return fakeExecStderr(exitCode, "")
}

// fakeExecStderr is fakeExec plus a line the fake OpenOCD writes to its stderr
// before exiting, so tests can exercise stderr-marker detection.
func fakeExecStderr(exitCode, stderrMsg string) func(context.Context, string, ...string) *exec.Cmd {
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1", "BRIDGE_EXIT="+exitCode, "BRIDGE_STDERR="+stderrMsg)
		return cmd
	}
}

// TestHelperProcess is not a real test: it is the fake OpenOCD binary invoked by
// fakeExec. For `--version`/`shutdown` it just exits with BRIDGE_EXIT; otherwise
// it acts as OpenOCD's cmsis_dap_tcp client — connecting to the bridge port
// parsed from its args, exchanging one framed packet, then exiting with
// BRIDGE_EXIT.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	code := 0
	if os.Getenv("BRIDGE_EXIT") == "1" {
		code = 1
	} else if os.Getenv("BRIDGE_EXIT") == "2" {
		code = 2
	}

	// No-socket paths (Validate's --version, SupportsCMSISDAPTCP's config-stage
	// probe ending in `shutdown`): just exit with the requested code.
	for _, a := range args {
		if a == "--version" || a == "shutdown" {
			os.Exit(code)
		}
	}

	// CMSIS-DAP TCP path (BridgeDAP): act as OpenOCD's cmsis_dap_tcp client —
	// connect and exchange one framed packet, then exit with code.
	if port := portArg(args, "cmsis-dap tcp port "); port != "" {
		dapHelperClient(port, code)
	}
	os.Exit(10) // a bridge test must always select the cmsis-dap TCP backend
}

// portArg returns the value following the "-c <prefix><value>" config arg, or "".
func portArg(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix)
		}
	}
	return ""
}

// dapHelperClient is the fake OpenOCD cmsis_dap_tcp client used by the BridgeDAP
// tests. It connects to the bridge, sends one request frame (8-byte DAP header +
// "DAPREQ"), expects one response frame carrying "DAPRESP", optionally emits a
// stderr marker, then exits with code. The header framing here must match
// copyOpenOCDToPod / copyPodToOpenOCD.
func dapHelperClient(port string, code int) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 5*time.Second)
	if err != nil {
		os.Exit(11)
	}
	defer conn.Close()

	const reqPayload = "DAPREQ"
	req := []byte{0x44, 0x41, 0x50, 0x00, byte(len(reqPayload)), 0x00, 0x01, 0x00}
	req = append(req, reqPayload...)
	if _, err := conn.Write(req); err != nil {
		os.Exit(12)
	}

	hdr := make([]byte, 8)
	if _, err := readFull(conn, hdr); err != nil {
		os.Exit(13)
	}
	// signature "DAP", packet_type response (0x02).
	if hdr[0] != 0x44 || hdr[1] != 0x41 || hdr[2] != 0x50 || hdr[3] != 0x00 || hdr[6] != 0x02 {
		os.Exit(14)
	}
	n := int(hdr[4]) | int(hdr[5])<<8
	payload := make([]byte, n)
	if _, err := readFull(conn, payload); err != nil || string(payload) != "DAPRESP" {
		os.Exit(15)
	}
	if msg := os.Getenv("BRIDGE_STDERR"); msg != "" {
		_, _ = os.Stderr.WriteString(msg + "\n")
	}
	os.Exit(code)
}
