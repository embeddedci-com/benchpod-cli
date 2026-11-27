// Package serialconsole drives the bench-pod firmware's USB CDC-ACM serial
// console (the text, line-oriented channel on the RP2350's native USB port).
// It is distinct from internal/tcpclient, which speaks the firmware's TCP/JSON
// API: the serial console is what provisions WiFi credentials and hands off to
// the UF2 bootloader before the device is ever on the network.
//
// The console echoes typed characters, prints a "> " prompt after each command,
// and interleaves asynchronous boot / WiFi / AT-modem log lines with command
// output. Callers therefore parse by scanning the accumulated output for
// documented marker substrings rather than assuming clean, contiguous lines.
package serialconsole

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// benchPodVIDs are the USB vendor IDs a bench-pod CDC-ACM console has enumerated
// under, matched case-insensitively against enumerator VID strings:
//
//	2E8A — Raspberry Pi, the retired RP2350 pod's native USB port.
//	0483 — STMicroelectronics, the STM32H563 pod up to firmware 4.0.4 (ST's
//	       stock CDC VID/PID 0483:5740).
//	1209 — pid.codes. Pre-release builds of firmware 3.9.0 used the test ID 1209:0001;
//	       the pod's own ID 1209:EC10 is used from firmware 4.0.5.
//
// None of these IDs identifies a pod by itself (0483:5740 is shared by every ST Virtual COM
// Port: spectrum analyzers, NanoVNAs, customers' boards), so a VID only ORDERS ports (see
// portPrio). What allows a port to be opened and written to is isPodPort.
var benchPodVIDs = []string{"2E8A", "0483", "1209"}

// isBenchPodVID reports whether vid is one of benchPodVIDs.
func isBenchPodVID(vid string) bool {
	for _, want := range benchPodVIDs {
		if strings.EqualFold(vid, want) {
			return true
		}
	}
	return false
}

// podVID and podPID are the pod's own pid.codes USB ID, 1209:EC10.
const (
	podVID = "1209"
	podPID = "EC10"
)

// isPodOwnID reports whether vid:pid is exactly the pod's own USB ID.
func isPodOwnID(vid, pid string) bool {
	return strings.EqualFold(strings.TrimSpace(vid), podVID) && strings.EqualFold(strings.TrimSpace(pid), podPID)
}

// baudRate is nominal: CDC-ACM ignores the on-wire rate, but terminal tools
// expect 115200 8N1 (8N1 are the zero-value defaults of serial.Mode).
const baudRate = 115200

// defaultPrompt is the firmware's command prompt. A command's output is
// considered complete once this substring appears in the accumulated bytes.
const defaultPrompt = "> "

// benchpodMarker is the substring the firmware's `status` prints to identify
// itself ("device : benchpod"). The CLI greps for it to tell a real bench-pod
// console apart from other USB-serial devices that share the Raspberry Pi USB
// VID — notably the CMSIS-DAP debug probe's CDC.
const benchpodMarker = "benchpod"

// lineBufSize mirrors the firmware console's LINE_BUF_SIZE (rp2350 console.c):
// the most characters its line editor holds. clearLineSeq is that many
// backspaces. The firmware ignores Ctrl-U but honors backspace/DEL, treating a
// backspace on an empty line as a no-op — so sending lineBufSize backspaces
// before every command erases any partial line a previous interactive session
// left behind (e.g. a stray "y") without executing it, so "status" can never
// arrive as "ystatus".
const lineBufSize = 128

var clearLineSeq = bytes.Repeat([]byte{0x08}, lineBufSize)

// perReadTimeout bounds each Read so the loop can re-check the context deadline
// even when the device is silent. go.bug.st/serial signals a read timeout by
// returning (0, nil), so this only affects responsiveness, never correctness.
const perReadTimeout = 250 * time.Millisecond

// dapReadySentinel is the line the firmware prints once the console has switched
// into raw length-framed CMSIS-DAP mode. DAPStart reads lines until it sees
// exactly this; any "ERROR:"/"usage:" line before it is a handshake failure.
const dapReadySentinel = "dap ready"

// errPortVanished is returned by sendCommand when the port reaches EOF before a
// prompt. For most commands that is a failure; for Bootsel it is the expected
// success signal (the device reboots into the UF2 bootloader and the CDC-ACM
// port disappears).
var errPortVanished = errors.New("USB console vanished (device rebooted)")

// portGone reports whether a read error means the USB console went away (the device
// rebooted). Linux gives EOF; macOS gives go.bug.st/serial's PortClosed error instead,
// which made `flash-self --enter-dfu` fail with "read serial: Port has been closed"
// although the pod had entered DFU.
func portGone(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	var pe *serial.PortError
	return errors.As(err, &pe) && pe.Code() == serial.PortClosed
}

// portLister is a test seam mirroring capabilities.serialPortLister so unit
// tests can enumerate fake ports without touching real USB. It wraps the call
// because go.bug.st/serial 1.8 made GetDetailedPortsList variadic (optional
// active-probe filters), which the seam does not need.
var portLister = func() ([]*enumerator.PortDetails, error) { return enumerator.GetDetailedPortsList() }

// isPodPort is the one rule for opening a port during automatic detection (no explicit device).
// A port may be opened and written to only when its USB descriptors say it is a pod:
//
//  1. VID:PID is exactly the pod's own ID, 1209:EC10 (firmware 4.0.5 and later); or
//  2. the manufacturer string is "EmbeddedCI" (firmware 3.9.0 and later, pre-release 3.9.0
//     builds on 1209:0001, and 3.8.x and older, all on 0483:5740); or
//  3. the product string contains "bench" ("BenchPod", "bench-pod STM32H563 CDC").
//
// Everything else is never opened: other devices on a pod-capable VID ("tinySA4", "STM32
// Virtual ComPort", ST-Links) and ports with no USB information. Writing a console command into
// an instrument can freeze it. An explicitly given device bypasses this rule.
func isPodPort(p *enumerator.PortDetails) bool {
	if p == nil || !p.IsUSB {
		return false
	}
	if isPodOwnID(p.VID, p.PID) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(p.Manufacturer), "EmbeddedCI") {
		return true
	}
	return strings.Contains(strings.ToLower(p.Product), "bench")
}

// noPodRule is the hint appended to "no pod found" errors.
const noPodRule = "looked for USB ID 1209:EC10 or an EmbeddedCI/BenchPod device; pass --connection <device> to use another port"

// DetectPort chooses the bench-pod serial console.
//
//	explicit != ""  -> used verbatim, no enumeration.
//	0 matches       -> error (device unplugged? wrong cable? pass --connection <device>).
//	1 match         -> that port.
//	>1 matches      -> error listing candidates; the user must pass --connection <device>.
//
// Unlike capabilities.DetectSerial there is no per-OS default fallback: the isPodPort
// filter is the whole point, and guessing the wrong port for a console that can
// trigger `bootsel` is unsafe. --connection <device> is the escape hatch.
func DetectPort(explicit string) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit, nil
	}
	ports, err := portLister()
	if err != nil {
		return "", fmt.Errorf("USB port enumeration is not supported on this OS; pass --connection <device>: %w", err)
	}
	type cand struct{ name, product, serial string }
	var cands []cand
	for _, p := range ports {
		if p == nil || strings.TrimSpace(p.Name) == "" || !p.IsUSB {
			continue
		}
		if isPodPort(p) {
			cands = append(cands, cand{name: p.Name, product: p.Product, serial: p.SerialNumber})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].name < cands[j].name })

	switch len(cands) {
	case 0:
		return "", noPod(fmt.Errorf("no bench-pod USB console found. Is the device plugged in? (%s)", noPodRule))
	case 1:
		return cands[0].name, nil
	default:
		var b strings.Builder
		b.WriteString("multiple bench-pod USB consoles found; pass --connection <device> to choose one:")
		for _, c := range cands {
			b.WriteString("\n  " + c.name)
			detail := strings.TrimSpace(c.product)
			if c.serial != "" {
				if detail != "" {
					detail += ", "
				}
				detail += "serial " + c.serial
			}
			if detail != "" {
				b.WriteString(" (" + detail + ")")
			}
		}
		return "", errors.New(b.String())
	}
}

// readTimeoutSetter is implemented by *serial.Port (and the test fake) so
// sendCommand can bound individual reads. Feature-detected; the deadline guard
// works regardless of whether the transport honors it.
type readTimeoutSetter interface {
	SetReadTimeout(time.Duration) error
}

// inputResetter is implemented by *serial.Port: it drops bytes received but not yet read.
type inputResetter interface {
	ResetInputBuffer() error
}

// Console is an open serial connection to the firmware command prompt.
type Console struct {
	rw     io.ReadWriteCloser
	prompt string
	// logf, when non-nil, receives human-readable diagnostics (connection +
	// per-command activity). Open wires it to serialLogf; tests leave it nil.
	logf func(format string, args ...any)
	// sigProbed / sigSupported cache whether the firmware takes upload-sig (asked once per
	// console session, on the first signed upload).
	sigProbed    bool
	sigSupported bool
}

// serialLogf emits a "[serial] " diagnostic line to the standard logger (stderr),
// matching the rest of the CLI's log.Printf output.
func serialLogf(format string, args ...any) {
	log.Printf("[serial] "+format, args...)
}

// logln forwards to c.logf when set, so library diagnostics are visible from the
// CLI (via Open) but silent in unit tests (which construct Consoles directly).
func (c *Console) logln(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

// Open auto-detects (or uses explicitDevice), opens it at 115200 8N1, and
// returns the Console plus the chosen port path.
func Open(explicitDevice string) (*Console, string, error) {
	name, err := DetectPort(explicitDevice)
	if err != nil {
		return nil, "", err
	}
	serialLogf("connecting to bench-pod USB console at %s (%d 8N1)...", name, baudRate)
	port, err := serial.Open(name, &serial.Mode{BaudRate: baudRate})
	if err != nil {
		return nil, "", noPod(fmt.Errorf("open USB port %s: %w", name, err))
	}
	serialLogf("connected to %s", name)
	c := newConsole(port)
	c.logf = serialLogf
	return c, name, nil
}

// noPodError is a failure to find or open the pod's USB console. Its text is err's; it also
// matches tcpclient.ErrUnreachable, so the CLI exits as for a pod it cannot reach.
type noPodError struct{ err error }

func noPod(err error) error { return &noPodError{err: err} }

// isPermissionDenied reports whether opening a serial port failed for lack of access (Linux: the
// device belongs to the dialout group).
func isPermissionDenied(err error) bool {
	var pe *serial.PortError
	if errors.As(err, &pe) && pe.Code() == serial.PermissionDenied {
		return true
	}
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "permission denied")
}

// deniedHint explains a pod console this user may not open, or "" when none was refused.
func deniedHint(denied []string) string {
	if len(denied) == 0 {
		return ""
	}
	return fmt.Sprintf("; %s could not be opened (permission denied): on Linux add yourself to the dialout group "+
		"(`sudo usermod -aG dialout $USER`, then log in again) or install the udev rule 70-benchpod.rules. "+
		"On firmware 3.9.0+ the network commands work over USB without the console", strings.Join(denied, ", "))
}

func (e *noPodError) Error() string        { return e.err.Error() }
func (e *noPodError) Unwrap() error        { return e.err }
func (e *noPodError) Is(target error) bool { return target == tcpclient.ErrUnreachable }

// newConsole wraps an already-open transport. Used by Open and by tests.
func newConsole(rw io.ReadWriteCloser) *Console {
	return &Console{rw: rw, prompt: defaultPrompt}
}

// OpenBenchpod opens the bench-pod serial console, identifying it by probe.
//
// With an explicit device it opens that verbatim (the caller chose it, no filtering). Otherwise
// it enumerates USB serial ports, keeps only those isPodPort allows (and their macOS cu/tty
// twins), tries the `preferred` hint first (a previously-found device cached by the caller),
// then the pod's own ID, and probes each with `status`, returning the first whose output
// identifies a bench pod. Other devices (spectrum analyzers, ST-Links, the CMSIS-DAP probe's CDC)
// are never opened. probeTimeout bounds each probe.
func OpenBenchpod(explicit, preferred string, probeTimeout time.Duration) (*Console, string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return Open(explicit)
	}
	cands, err := candidatePorts()
	if err != nil {
		return nil, "", err
	}
	cands = preferFirst(strings.TrimSpace(preferred), cands)
	if len(cands) == 0 {
		return nil, "", noPod(errors.New("no pod USB port found; plug in the pod (" + noPodRule + ")"))
	}
	var tried, denied []string
	for _, name := range cands {
		serialLogf("probing %s for a bench-pod console...", name)
		c, err := openPort(name)
		if err != nil {
			serialLogf("  %s: open failed: %v", name, err)
			if isPermissionDenied(err) {
				denied = append(denied, name)
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		ok := c.IsBenchpod(ctx)
		cancel()
		if ok {
			serialLogf("  %s: identified as a pod", name)
			return c, name, nil
		}
		serialLogf("  %s: not a pod (no %q in status)", name, benchpodMarker)
		_ = c.Close()
		tried = append(tried, name)
	}
	return nil, "", noPod(fmt.Errorf("no bench-pod console found among %d probed USB port(s) [%s]%s; %s",
		len(tried), strings.Join(tried, ", "), deniedHint(denied), noPodRule))
}

// serialGlobs are the /dev node patterns scanned (in addition to the enumerator) to find the
// dial-in /dev/tty.* twin of an allowed /dev/cu.* port. A globbed node is only a candidate when
// the enumerator lists a pod port with the same name (see candidatePortsIDs). No-op on Windows.
var serialGlobs = []string{
	"/dev/cu.usb*", "/dev/tty.usb*", // usbserial / usbmodem (CDC-ACM)
	"/dev/cu.SLAB*", "/dev/tty.SLAB*", // Silicon Labs CP210x
	"/dev/cu.wch*", "/dev/tty.wch*", // WCH CH340 / CH9102
	"/dev/ttyUSB*", "/dev/ttyACM*", // Linux
}

// globPorts returns the /dev nodes matching serialGlobs. A package var (mirroring
// portLister) so tests can stub the filesystem scan.
var globPorts = func() []string {
	var out []string
	for _, pat := range serialGlobs {
		m, _ := filepath.Glob(pat)
		out = append(out, m...)
	}
	return out
}

// candidatePorts lists the serial ports automatic detection may open: only ports isPodPort
// allows. The enumerator's USB ports are filtered by isPodPort; a globbed /dev/cu.* or /dev/tty.*
// node is added only when its twin (same name after the cu./tty. prefix) is an allowed port, so
// a node with no USB info is never a candidate. Deduped, the pod's own ID first, then by name.
func candidatePorts() ([]string, error) {
	names, _, err := candidatePortsVID()
	return names, err
}

// candidatePortsVID is candidatePorts plus each port's USB VID ("" when the enumerator did not
// list it, as for the /dev/tty.* dial-in twins).
func candidatePortsVID() ([]string, map[string]string, error) {
	names, vid, _, err := candidatePortsIDs()
	return names, vid, err
}

// candidatePortsIDs is candidatePortsVID plus each port's USB PID.
func candidatePortsIDs() ([]string, map[string]string, map[string]string, error) {
	vid := map[string]string{}
	pid := map[string]string{}
	allowed := map[string]bool{} // twinName of every allowed port
	var enumerated []string
	if ports, err := portLister(); err == nil {
		for _, p := range ports {
			if p == nil || strings.TrimSpace(p.Name) == "" || !isPodPort(p) {
				continue
			}
			vid[p.Name] = p.VID
			pid[p.Name] = p.PID
			allowed[twinName(p.Name)] = true
			enumerated = append(enumerated, p.Name)
		}
	}

	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, n := range enumerated {
		add(n)
	}
	for _, m := range globPorts() {
		if allowed[twinName(m)] {
			add(m)
		}
	}
	// An empty list is a finding, not a failure: `discover` reports "nothing to
	// probe" while OpenBenchpod turns it into its own actionable error.
	sort.SliceStable(names, func(i, j int) bool {
		pi, pj := portPrio(vid[names[i]], pid[names[i]]), portPrio(vid[names[j]], pid[names[j]])
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	return names, vid, pid, nil
}

// portPrio orders the allowed ports: 0 the pod's own ID (1209:EC10), 1 any other
// pod-capable VID, 2 everything else (an EmbeddedCI/BenchPod string on another ID, or a twin).
func portPrio(vid, pid string) int {
	switch {
	case isPodOwnID(vid, pid):
		return 0
	case isBenchPodVID(vid):
		return 1
	}
	return 2
}

// preferFirst moves want to the front of names (when present), so a cached
// hint is probed before everything else. A no-op for an empty/absent want.
func preferFirst(want string, names []string) []string {
	if want == "" {
		return names
	}
	out := make([]string, 0, len(names)+1)
	found := false
	for _, n := range names {
		if n == want {
			found = true
			continue
		}
		out = append(out, n)
	}
	if found {
		return append([]string{want}, out...)
	}
	return names
}

// Close closes the underlying transport.
func (c *Console) Close() error { return c.rw.Close() }

// writeLine clears any stale partial input in the firmware's line editor, then
// writes line + "\n". The clear sequence and the command go out in a single
// write so the command is never appended to leftover bytes. See clearLineSeq.
func (c *Console) writeLine(line string) error {
	buf := make([]byte, 0, len(clearLineSeq)+len(line)+1)
	buf = append(buf, clearLineSeq...)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	if _, err := c.rw.Write(buf); err != nil {
		return err
	}
	return nil
}

// sendCommand writes line (clearing any stale partial input first) and reads
// until the prompt appears or ctx expires, returning everything captured. On EOF
// before a prompt it returns the accumulated output and errPortVanished so
// Bootsel can treat that as success.
func (c *Console) sendCommand(ctx context.Context, line string) (string, error) {
	return c.sendCommandRedacted(ctx, line, line, "")
}

// sendCommandRedacted is sendCommand with redaction for sensitive commands:
//   - line:    the bytes actually written to the wire.
//   - display: the form shown in logs and error messages (e.g. password masked).
//   - secret:  any occurrence is masked (-> "***") in captured output before it
//     reaches a log line or error message, so the firmware's echo can't leak it.
//
// Non-sensitive callers go through sendCommand (display == line, secret == "").
func (c *Console) sendCommandRedacted(ctx context.Context, line, display, secret string) (string, error) {
	return c.sendCommandUntil(ctx, line, display, secret, nil)
}

// echoGrace bounds how long a prompt that arrives before the command's echo waits for the
// echo. The firmware echoes typed characters at once, so a prompt ahead of the echo is
// normally a stale one left from an earlier command; a console that does not echo still
// answers, this much later.
const echoGrace = 100 * time.Millisecond

// echoNeedleLen is how much of the command its echo is matched on: enough to be specific,
// short enough to fit the firmware's line editor and to survive an async log line landing
// late in a long echo.
const echoNeedleLen = 24

// replyGrace bounds how long sendCommandUntil keeps reading after a prompt
// arrives before the reply looks complete. Async log lines are written in
// chunks, so a fragment such as "> connected" can land right after one of the
// reply's own line breaks and look like the prompt; the real prompt follows
// within milliseconds. The grace only applies when complete() says the reply
// is short, so a firmware that simply omits a line costs this much, no more.
const replyGrace = 500 * time.Millisecond

// sendCommandUntil is sendCommandRedacted with an optional completeness check:
// when complete is non-nil, a prompt only ends the read once complete(acc)
// reports the whole reply is there, or replyGrace after the first prompt.
func (c *Console) sendCommandUntil(ctx context.Context, line, display, secret string, complete func(string) bool) (string, error) {
	return c.sendCommandWait(ctx, line, display, secret, complete, replyGrace)
}

// sendCommandWait is sendCommandUntil with the grace after an early prompt as a parameter. A
// grace of 0 waits for complete(acc) (or ctx) however many prompts come first: for a command
// whose result follows its prompt (the built-in Wi-Fi's wifi-scan prints the networks when the
// scan ends, a few seconds after the prompt).
func (c *Console) sendCommandWait(ctx context.Context, line, display, secret string, complete func(string) bool, grace time.Duration) (string, error) {
	c.logln("> %s", display)
	// Drop what is still unread (a prompt or the tail of an earlier reply), so it cannot
	// end this command's read before the reply has arrived.
	if ir, ok := c.rw.(inputResetter); ok {
		_ = ir.ResetInputBuffer()
	}
	if err := c.writeLine(line); err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}
	if rt, ok := c.rw.(readTimeoutSetter); ok {
		_ = rt.SetReadTimeout(perReadTimeout)
	}

	var acc []byte
	var graceEnd time.Time // set when a prompt arrives before the reply is complete
	var echoWait time.Time // set when a prompt arrives before the command's echo
	echo := line
	if len(echo) > echoNeedleLen {
		echo = echo[:echoNeedleLen]
	}
	// replyEnded reports whether acc holds this command's closing prompt: one after the
	// echo, or (for a console that never echoes) any prompt once echoGrace has passed.
	replyEnded := func(s string) bool {
		if i := strings.Index(s, echo); i >= 0 && echo != "" {
			return strings.Contains(s[i+len(echo):], "\n"+c.prompt)
		}
		if !promptSeen(s, c.prompt) {
			return false
		}
		if echoWait.IsZero() {
			echoWait = time.Now().Add(echoGrace)
		}
		return time.Now().After(echoWait)
	}
	buf := make([]byte, 512)
	for {
		if !graceEnd.IsZero() && time.Now().After(graceEnd) {
			c.logln("< reply to %q looks incomplete; using what arrived (%d bytes)", display, len(acc))
			return string(acc), nil
		}
		if err := ctx.Err(); err != nil {
			c.logln("< no prompt after %q — timed out (%d bytes received: %q)",
				display, len(acc), maskPassword(tail(acc, 120), secret))
			return string(acc), fmt.Errorf("waiting for prompt after %q: %w (got: %q)",
				display, err, maskPassword(tail(acc, 200), secret))
		}
		n, err := c.rw.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
		}
		// Re-check on silence too while a prompt waits for the echo (echoGrace).
		if n > 0 || (!echoWait.IsZero() && graceEnd.IsZero()) {
			if replyEnded(string(acc)) {
				if complete == nil || complete(string(acc)) {
					c.logln("< got prompt after %q (%d bytes)", display, len(acc))
					return string(acc), nil
				}
				if graceEnd.IsZero() && grace > 0 {
					graceEnd = time.Now().Add(grace)
				}
			}
		}
		if err != nil {
			if portGone(err) {
				return string(acc), errPortVanished
			}
			return string(acc), fmt.Errorf("read serial: %w", err)
		}
		// n==0, err==nil is a per-read timeout (go.bug.st/serial); loop and
		// let the ctx guard above enforce the overall deadline.
	}
}

var errNoPowerReply = errors.New("no power reply yet")

// Status runs the firmware's `status` console command and returns its output as
// presentable text (the console prints human-readable lines, not JSON). The
// echoed command and the trailing prompt are stripped; interleaved async log
// lines are left intact.
func (c *Console) Status(ctx context.Context) (string, error) {
	out, err := c.sendCommand(ctx, "status")
	if err != nil {
		return "", err
	}
	return cleanConsoleOutput(out, "status"), nil
}

// IsBenchpod runs `status` and reports whether the output identifies a bench pod
// (contains benchpodMarker, "benchpod"). A non-bench-pod port typically never
// prints the prompt, so sendCommand runs the deadline out; either way we inspect
// whatever was captured, so a probe is bounded by ctx.
func (c *Console) IsBenchpod(ctx context.Context) bool {
	out, _ := c.sendCommand(ctx, "status")
	return strings.Contains(strings.ToLower(out), benchpodMarker)
}

// cleanConsoleOutput tidies raw sendCommand output for display: it normalises
// CRLF, removes the trailing "> " prompt the firmware prints after a command,
// and drops the echoed command line. Content lines (including async log lines)
// are preserved as-is.
func cleanConsoleOutput(raw, cmd string) string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// Strip the trailing prompt ("> ") and any padding around it.
	s = strings.TrimRight(s, " \t\n")
	s = strings.TrimSuffix(s, ">")
	s = strings.TrimRight(s, " \t\n")
	kept := make([]string, 0)
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) == cmd {
			continue // command echo
		}
		kept = append(kept, ln)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n")
}

// Bootsel reboots the device into the UF2 bootloader. Success is either the
// "entering BOOTSEL" marker or the port vanishing (EOF) right after the command.
func (c *Console) Bootsel(ctx context.Context) error {
	out, err := c.sendCommand(ctx, "bootsel")
	if strings.Contains(out, "entering BOOTSEL") {
		return nil
	}
	if errors.Is(err, errPortVanished) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("device did not acknowledge bootsel")
}

// Dfu reboots an STM32 bench pod into its ROM USB DFU bootloader (the STM32
// analog of Bootsel; RP2350 pods don't have this command). The device then
// re-enumerates as an STM32 DFU device and the firmware can be rewritten with
// dfu-util (see the `flash-self` command). Success is either the "entering DFU"
// marker or the port vanishing (EOF, or PortClosed on macOS) as USB re-enumerates.
func (c *Console) Dfu(ctx context.Context) error {
	out, err := c.sendCommand(ctx, "dfu")
	if strings.Contains(out, "entering DFU") {
		return nil
	}
	if errors.Is(err, errPortVanished) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("device did not acknowledge dfu (RP2350 pods use `bootsel` instead)")
}

// transcriptSuffix formats captured console output for a handshake error,
// returning "" when there is nothing to show.
func transcriptSuffix(t string) string {
	t = strings.TrimSpace(t)
	if t == "" {
		return ""
	}
	return "; pod output:\n" + t
}

// readLine reads one '\n'-terminated line a byte at a time, honoring per-read
// timeouts ((0,nil) from go.bug.st/serial) and the ctx deadline. Reading single
// bytes guarantees no bytes past the newline are consumed — important right
// before the framed CMSIS-DAP stream begins. The trailing newline is stripped;
// any trailing '\r' is left for the caller's TrimSpace.
func (c *Console) readLine(ctx context.Context) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		if err := ctx.Err(); err != nil {
			return string(buf), fmt.Errorf("%w (got: %q)", err, tail(buf, 120))
		}
		n, err := c.rw.Read(b)
		if n > 0 {
			if b[0] == '\n' {
				return string(buf), nil
			}
			buf = append(buf, b[0])
		}
		if err != nil {
			if portGone(err) {
				return string(buf), errPortVanished
			}
			return string(buf), fmt.Errorf("read serial: %w", err)
		}
		// n==0, err==nil is a per-read timeout; loop under the ctx guard above.
	}
}

// firstToken returns the leading whitespace-delimited token of s.
func firstToken(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// promptSeen reports whether the firmware's command prompt has appeared at a line
// boundary in s. The firmware always prints the prompt ("> ") at the start of a
// line (print_prompt → fputs, after the trailing newline of the prior output), so
// matching only at a line start avoids false-positives on a "> " that occurs
// inside command output — notably the wiring hint "ESP32 TX -> RP RX", whose
// "-> " contains "> " and would otherwise end the read after the first probe.
func promptSeen(s, prompt string) bool {
	return strings.HasPrefix(s, prompt) || strings.Contains(s, "\n"+prompt)
}

// tail returns up to the last n bytes of b, for diagnostic messages.
func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// nrstPinMarker is what the firmware's console `status` prints on its board line
// when the pod has the dedicated target-reset pin (rev3+): `... nrst_pin=yes`.
const nrstPinMarker = "nrst_pin=yes"

// HasNRSTPin reports whether the pod owns a dedicated target-reset line, by
// reading the console `status` board line. See tcpclient.Client.HasNRSTPin for
// why `flash` needs this; a pod too old to print the marker answers false, which
// is correct — it has no reset pin.
func (c *Console) HasNRSTPin(ctx context.Context) (bool, error) {
	out, err := c.Status(ctx)
	if err != nil {
		return false, err
	}
	return strings.Contains(strings.ToLower(out), nrstPinMarker), nil
}

// maskPassword replaces password in out (console output can echo a secret back) with ***.
func maskPassword(out, password string) string {
	if password == "" {
		return out
	}
	return strings.ReplaceAll(out, password, "***")
}
