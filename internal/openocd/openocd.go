// Package openocd locates and validates the host's OpenOCD binary and bridges
// it to a bench pod that is already in CMSIS-DAP (PROTO_DAP) probe mode.
//
// OpenOCD's cmsis-dap TCP backend opens a TCP socket and exchanges whole
// CMSIS-DAP packets — it cannot perform the pod's dap_start handshake itself. So
// the caller first sends dap_start over the pod connection (see
// tcpclient.DAPStart / serialconsole.DAPStart) and hands the resulting raw
// connection here; BridgeDAP then stands up a loopback listener, points OpenOCD
// at it, and translates the per-packet framing both ways. OpenOCD owns the flash
// verdict: a clean exit (code 0) is success.
package openocd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
)

// ErrTargetUnreachable means OpenOCD's CMSIS-DAP probe worked but the target MCU
// never answered on SWD: the debug port IDCODE could not be read. This is
// distinct from a flash that started and then failed — it almost always means
// the target is unpowered, mis-wired, or held in reset. BridgeDAP returns it
// (wrapped) so callers can print transport-specific guidance instead of leaving
// the user with OpenOCD's terse "cannot read IDR".
var ErrTargetUnreachable = errors.New("SWD target did not respond (could not read the debug port IDCODE)")

// swdConnectMarkers are the OpenOCD stderr substrings that identify a failed DAP
// connect (no target on the wire). OpenOCD runs the JTAG/SWD switch sequences
// fine over a working probe, then logs one of these when the target never acks.
var swdConnectMarkers = [][]byte{
	[]byte("cannot read IDR"),
	[]byte("Error connecting DP"),
}

// failureScanner tees OpenOCD's stderr to the user's writer while watching for
// the swdConnectMarkers. It keeps a small rolling tail so a marker split across
// two writes is still caught. Bridge consults matched only after cmd.Wait
// returns, at which point exec guarantees the stderr copier has finished — so a
// plain atomic is enough.
type failureScanner struct {
	w       io.Writer
	tail    []byte
	matched atomic.Bool
}

// maxMarkerTail is the longest marker length minus one: the most we must carry
// over between writes to catch a marker straddling a write boundary.
const maxMarkerTail = 32

func (s *failureScanner) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	scan := append(s.tail, p...)
	for _, m := range swdConnectMarkers {
		if bytes.Contains(scan, m) {
			s.matched.Store(true)
			break
		}
	}
	if len(scan) > maxMarkerTail {
		scan = scan[len(scan)-maxMarkerTail:]
	}
	s.tail = append(s.tail[:0], scan...)
	return n, err
}

// lookPath is a test seam over exec.LookPath.
var lookPath = exec.LookPath

// commandContext is a test seam over exec.CommandContext, letting tests
// substitute a fake OpenOCD process.
var commandContext = exec.CommandContext

// Find resolves the OpenOCD binary on PATH, returning an actionable error when
// it is missing so the caller can tell the user to install it.
func Find() (string, error) {
	bin, err := lookPath("openocd")
	if err != nil {
		return "", fmt.Errorf("openocd not found in PATH; install openocd first (e.g. `brew install open-ocd` on macOS, `apt install openocd` on Debian/Ubuntu)")
	}
	return bin, nil
}

// Validate runs `openocd --version` and errors if it cannot be executed or
// exits non-zero, proving the located binary actually works before we commit to
// a flash session.
func Validate(ctx context.Context, bin string) error {
	cmd := commandContext(ctx, bin, "--version")
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := string(out)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("openocd does not run (`%s --version` failed): %s", bin, msg)
	}
	return nil
}

// bridgeStrategy parametrises runBridge for a particular host adapter: the
// OpenOCD adapter-driver config to prepend (pointing the backend at our loopback
// port) and the per-direction byte pumps between OpenOCD and the pod (a framing
// translation for CMSIS-DAP; see dap.go).
//
// The pod's DAP mode is left by closing the connection: the firmware disarms the
// SWD engine and returns the conn to JSON on disconnect
// (command_handler_conn_closed) — so the bridge never has to write an in-band
// "leave" frame (which could block on a stalled pod).
type bridgeStrategy struct {
	adapterArgs func(port int) []string
	copyToPod   func(dst io.Writer, src io.Reader) error // openocd -> pod
	copyFromPod func(dst io.Writer, src io.Reader) error // pod -> openocd
}

// runBridge is the shared bridge machinery behind BridgeDAP (CMSIS-DAP over TCP):
// it stands up a loopback listener, launches OpenOCD against the strategy's
// adapter config, accepts OpenOCD's single connection, and pumps bytes both ways
// (per the strategy) until OpenOCD exits. The accept race, pod-drop detection,
// the no-target stderr scan, and the exit-code verdict all live here.
func runBridge(ctx context.Context, bin string, strat bridgeStrategy, args []string, podConn io.ReadWriteCloser, stdout, stderr io.Writer) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for openocd bridge: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// Prepend the adapter config so OpenOCD connects back to our bridge; the
	// caller's args (target cfg, program command, passthrough) follow.
	fullArgs := append(strat.adapterArgs(port), args...)

	// runCtx lets us kill OpenOCD proactively if the pod link drops mid-flash;
	// otherwise OpenOCD spins on the dead socket emitting repeated errors.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	cmd := commandContext(runCtx, bin, fullArgs...)
	cmd.Stdout = stdout
	scanner := &failureScanner{w: stderr}
	cmd.Stderr = scanner
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start openocd: %w", err)
	}

	// Accept OpenOCD's connection. ln.Close on a failed/early exit unblocks
	// Accept, so wait for it in the foreground rather than risk hanging.
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan acceptResult, 1)
	go func() {
		c, err := ln.Accept()
		acceptCh <- acceptResult{c, err}
	}()

	// If OpenOCD exits before it ever connects (bad config, etc.), close the
	// listener so Accept returns and we don't block forever.
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var ocConn net.Conn
	select {
	case res := <-acceptCh:
		if res.err != nil {
			_ = cmd.Wait()
			return fmt.Errorf("accept openocd connection: %w", res.err)
		}
		ocConn = res.conn
	case waitErr := <-waitCh:
		ln.Close()
		<-acceptCh // drain
		if waitErr != nil {
			return fmt.Errorf("openocd exited before connecting: %w", waitErr)
		}
		return errors.New("openocd exited before connecting to the bridge")
	}
	defer ocConn.Close()

	// Pipe bytes both ways until OpenOCD exits. Closing both ends on exit
	// unblocks the copies.
	var wg sync.WaitGroup
	var openocdExited, podDropped atomic.Bool
	var podReadErr atomic.Pointer[error]
	wg.Add(2)
	go func() { defer wg.Done(); _ = strat.copyToPod(podConn, ocConn) }()
	go func() {
		defer wg.Done()
		err := strat.copyFromPod(ocConn, podConn)
		// The pod->OpenOCD copy ended. If OpenOCD is still running, the pod
		// closed the link first (EOF/error): stop OpenOCD now rather than let it
		// spin on a dead socket spewing errors.
		if !openocdExited.Load() {
			if err != nil {
				podReadErr.Store(&err)
			}
			podDropped.Store(true)
			cancelRun()
		}
	}()

	waitErr := <-waitCh
	openocdExited.Store(true)
	// OpenOCD has exited; closing both ends unblocks the copy goroutines and
	// (pod side) makes the firmware disarm and return to JSON.
	_ = ocConn.Close()
	_ = podConn.Close()
	wg.Wait()

	if podDropped.Load() {
		cause := "EOF"
		if p := podReadErr.Load(); p != nil {
			cause = (*p).Error()
		}
		return fmt.Errorf("pod connection closed mid-flash: the SWD link dropped before OpenOCD finished (pod read: %s; see openocd output above)", cause)
	}
	if waitErr != nil {
		// A failed DAP connect (no target on the wire) is the common, actionable
		// case — flag it distinctly so the caller can guide power/wiring.
		if scanner.matched.Load() {
			return fmt.Errorf("%w: %w", ErrTargetUnreachable, waitErr)
		}
		return fmt.Errorf("openocd failed: %w", waitErr)
	}
	return nil
}
