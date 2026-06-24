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
	"sync"
	"time"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// benchPodVID is the USB vendor ID (Raspberry Pi) the RP2350 CDC-ACM console
// enumerates under. Matched case-insensitively against enumerator VID strings.
const benchPodVID = "2E8A"

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

// dapFlushDelay gives the final leave frame time to reach the firmware over USB
// before the port is closed, so the probe is disarmed promptly rather than only
// by the firmware's 60s inactivity watchdog.
const dapFlushDelay = 50 * time.Millisecond

// errPortVanished is returned by sendCommand when the port reaches EOF before a
// prompt. For most commands that is a failure; for Bootsel it is the expected
// success signal (the device reboots into the UF2 bootloader and the CDC-ACM
// port disappears).
var errPortVanished = errors.New("serial port vanished (device rebooted)")

// portLister is a test seam mirroring capabilities.serialPortLister so unit
// tests can enumerate fake ports without touching real USB.
var portLister = enumerator.GetDetailedPortsList

// DetectPort chooses the bench-pod serial console.
//
//	explicit != ""  -> used verbatim, no enumeration.
//	0 matches       -> error (device unplugged? wrong cable? pass --connection <device>).
//	1 match         -> that port.
//	>1 matches      -> error listing candidates; the user must pass --connection <device>.
//
// Unlike capabilities.DetectSerial there is no per-OS default fallback: the VID
// filter is the whole point, and guessing the wrong port for a console that can
// trigger `bootsel` is unsafe. --connection <device> is the escape hatch.
func DetectPort(explicit string) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit, nil
	}
	ports, err := portLister()
	if err != nil {
		return "", fmt.Errorf("serial port enumeration is not supported on this OS; pass --connection <device>: %w", err)
	}
	type cand struct{ name, product, serial string }
	var cands []cand
	for _, p := range ports {
		if p == nil || strings.TrimSpace(p.Name) == "" || !p.IsUSB {
			continue
		}
		if strings.EqualFold(p.VID, benchPodVID) {
			cands = append(cands, cand{name: p.Name, product: p.Product, serial: p.SerialNumber})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].name < cands[j].name })

	switch len(cands) {
	case 0:
		return "", fmt.Errorf("no bench-pod serial console found (USB VID %s). Is the device plugged in? Pass --connection <device> to override", benchPodVID)
	case 1:
		return cands[0].name, nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "multiple bench-pod serial consoles found (USB VID %s); pass --connection <device> to choose one:", benchPodVID)
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

// Console is an open serial connection to the firmware command prompt.
type Console struct {
	rw     io.ReadWriteCloser
	prompt string
	// logf, when non-nil, receives human-readable diagnostics (connection +
	// per-command activity). Open wires it to serialLogf; tests leave it nil.
	logf func(format string, args ...any)
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
	serialLogf("connecting to bench-pod console at %s (%d 8N1)...", name, baudRate)
	port, err := serial.Open(name, &serial.Mode{BaudRate: baudRate})
	if err != nil {
		return nil, "", fmt.Errorf("open serial port %s: %w", name, err)
	}
	serialLogf("connected to %s", name)
	c := newConsole(port)
	c.logf = serialLogf
	return c, name, nil
}

// newConsole wraps an already-open transport. Used by Open and by tests.
func newConsole(rw io.ReadWriteCloser) *Console {
	return &Console{rw: rw, prompt: defaultPrompt}
}

// OpenBenchpod opens the bench-pod serial console, identifying it by probe.
//
// With an explicit device it opens that verbatim (the caller chose it). Otherwise
// it enumerates USB serial ports — the `preferred` hint first (a previously-found
// device cached by the caller), then the Raspberry Pi VID, then any other USB
// serial device (both /dev/cu.* and /dev/tty.*) — and probes each with `status`,
// returning the first whose output identifies a bench pod. This avoids latching
// onto the CMSIS-DAP debug probe's CDC (same VID) or another serial device.
// probeTimeout bounds each probe (a real bench pod answers in well under a second;
// only non-bench-pod ports run the timeout out).
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
		return nil, "", errors.New("no USB serial ports found; plug in the bench pod or pass --connection <device>")
	}
	var tried []string
	for _, name := range cands {
		serialLogf("probing %s for a bench-pod console...", name)
		port, err := serial.Open(name, &serial.Mode{BaudRate: baudRate})
		if err != nil {
			serialLogf("  %s: open failed: %v", name, err)
			continue
		}
		c := newConsole(port)
		c.logf = serialLogf
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		ok := c.IsBenchpod(ctx)
		cancel()
		if ok {
			serialLogf("  %s: identified as a bench pod", name)
			return c, name, nil
		}
		serialLogf("  %s: not a bench pod (no %q in status)", name, benchpodMarker)
		_ = c.Close()
		tried = append(tried, name)
	}
	return nil, "", fmt.Errorf("no bench-pod console found among %d probed port(s) [%s]; pass --connection <device> to force one",
		len(tried), strings.Join(tried, ", "))
}

// serialGlobs are the /dev node patterns scanned (in addition to the enumerator)
// so the dial-in /dev/tty.* variants — and anything the enumerator misses — are
// probed too. No-op on Windows (the patterns match nothing there; the enumerator
// supplies COM ports).
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

// candidatePorts lists serial ports to probe: the enumerator's USB ports plus the
// globbed /dev/cu.* and /dev/tty.* nodes (deduped), ordered Raspberry Pi VID first
// (the firmware's native console) then everything else by name.
func candidatePorts() ([]string, error) {
	// VID by port name, from the enumerator (cross-platform), for ordering.
	vid := map[string]string{}
	if ports, err := portLister(); err == nil {
		for _, p := range ports {
			if p != nil && p.IsUSB && strings.TrimSpace(p.Name) != "" {
				vid[p.Name] = p.VID
			}
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
	for n := range vid {
		add(n)
	}
	for _, m := range globPorts() {
		add(m)
	}
	if len(names) == 0 {
		return nil, errors.New("no USB serial ports found; pass --connection <device>")
	}
	sort.SliceStable(names, func(i, j int) bool {
		pi, pj := portPrio(vid[names[i]]), portPrio(vid[names[j]])
		if pi != pj {
			return pi < pj
		}
		return names[i] < names[j]
	})
	return names, nil
}

func portPrio(vid string) int {
	if strings.EqualFold(vid, benchPodVID) {
		return 0
	}
	return 1
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
	c.logln("> %s", display)
	if err := c.writeLine(line); err != nil {
		return "", fmt.Errorf("write command: %w", err)
	}
	if rt, ok := c.rw.(readTimeoutSetter); ok {
		_ = rt.SetReadTimeout(perReadTimeout)
	}

	var acc []byte
	buf := make([]byte, 512)
	for {
		if err := ctx.Err(); err != nil {
			c.logln("< no prompt after %q — timed out (%d bytes received: %q)",
				display, len(acc), maskPassword(tail(acc, 120), secret))
			return string(acc), fmt.Errorf("waiting for prompt after %q: %w (got: %q)",
				display, err, maskPassword(tail(acc, 200), secret))
		}
		n, err := c.rw.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
			if promptSeen(string(acc), c.prompt) {
				c.logln("< got prompt after %q (%d bytes)", display, len(acc))
				return string(acc), nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return string(acc), errPortVanished
			}
			return string(acc), fmt.Errorf("read serial: %w", err)
		}
		// n==0, err==nil is a per-read timeout (go.bug.st/serial); loop and
		// let the ctx guard above enforce the overall deadline.
	}
}

// WifiSetResult reports what the firmware did with submitted credentials.
type WifiSetResult struct {
	Persisted bool   // saw "[cfg] credentials written to flash"
	Joined    bool   // saw "[wifi] join OK"
	IP        string // parsed from "[wifi] join OK  ip=<ip>"
	Raw       string // full captured output (password already masked)
}

// WifiSet stores credentials and (re)joins the AP. ssid/password are quoted for
// the firmware shell. A "[wifi] join failed" marker (without a join OK) yields
// an error alongside a result with Persisted reflecting whether creds were saved.
func (c *Console) WifiSet(ctx context.Context, ssid, password string) (WifiSetResult, error) {
	qSSID, err := quoteArg(ssid)
	if err != nil {
		return WifiSetResult{}, fmt.Errorf("ssid: %w", err)
	}
	qPass, err := quoteArg(password)
	if err != nil {
		return WifiSetResult{}, fmt.Errorf("password: %w", err)
	}
	// Send the real command but show a masked form in logs/errors; the firmware
	// echoes typed chars, so `password` is also redacted from any captured output.
	display := "wifi-set " + qSSID + ` "***"`
	out, err := c.sendCommandRedacted(ctx, "wifi-set "+qSSID+" "+qPass, display, password)
	res := WifiSetResult{
		Persisted: strings.Contains(out, "[cfg] credentials written to flash"),
		Raw:       maskPassword(out, password),
	}
	// wifi-set delegates to the esp32-reconnect ladder, which reports success as
	// "[wifi] connected  ip=<ip>". Accept the older direct-join "[wifi] join OK"
	// marker too so a mixed firmware/CLI still works.
	joinMarker := ""
	switch {
	case strings.Contains(out, "[wifi] connected"):
		joinMarker = "[wifi] connected"
	case strings.Contains(out, "[wifi] join OK"):
		joinMarker = "[wifi] join OK"
	}
	if joinMarker != "" {
		res.Joined = true
		res.IP = parseIPAfter(out, joinMarker)
	}
	c.logln("wifi-set result: persisted=%t joined=%t ip=%s", res.Persisted, res.Joined, res.IP)
	if err != nil {
		return res, err // already masked by sendCommandRedacted (secret = password)
	}
	if !res.Joined && (strings.Contains(out, "[wifi] connect failed") ||
		strings.Contains(out, "[wifi] join failed") ||
		strings.Contains(out, "ESP32 still unreachable")) {
		return res, errors.New("wifi join failed (credentials were saved; check the password/SSID and signal)")
	}
	return res, nil
}

// BringupLines returns the WiFi / TCP / ESP32 bring-up log lines from captured
// console output (e.g. WifiSetResult.Raw), so callers can show the connection
// result and assigned IP without the lower-level AT/boot noise.
func BringupLines(raw string) []string {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[wifi]") || strings.HasPrefix(t, "[tcp]") ||
			strings.HasPrefix(t, "[esp32]") || strings.HasPrefix(t, "ESP32 ") {
			out = append(out, t)
		}
	}
	return out
}

// WifiStatus is the parsed result of show-wifi (firmware: wifi-show).
type WifiStatus struct {
	SSID  string
	State string
	IP    string
	RSSI  string
	Raw   string
}

// WifiShow reports stored SSID, WiFi state, IP, and RSSI. Missing fields are
// left empty rather than treated as errors, since async log lines may displace
// them in a given capture window.
func (c *Console) WifiShow(ctx context.Context) (WifiStatus, error) {
	out, err := c.sendCommand(ctx, "wifi-show")
	if err != nil {
		return WifiStatus{Raw: out}, err
	}
	return WifiStatus{
		SSID:  fieldValue(out, "ssid"),
		State: fieldValue(out, "state"),
		IP:    fieldValue(out, "ip"),
		RSSI:  fieldValue(out, "rssi"),
		Raw:   out,
	}, nil
}

// WifiClear erases stored credentials. A reboot is needed to fully apply.
func (c *Console) WifiClear(ctx context.Context) error {
	_, err := c.sendCommand(ctx, "wifi-clear")
	return err
}

// TargetPower enables (on) or disables a target power eFuse over the console
// (firmware: `target-power <1|2> <on|off>`). efuse is 1 (internal 5V) or 2
// (external). An "ERROR:" line from the firmware is reported as an error.
func (c *Console) TargetPower(ctx context.Context, efuse int, on bool) error {
	state := "off"
	if on {
		state = "on"
	}
	out, err := c.sendCommand(ctx, fmt.Sprintf("target-power %d %s", efuse, state))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "ERROR:") {
			return fmt.Errorf("firmware rejected target-power: %s", trimmed)
		}
	}
	return nil
}

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

// DAPStart enters the firmware's length-framed CMSIS-DAP probe mode over the
// console. It sends `dap-start <swclk> <swdio>[ <nreset>]`, then reads console
// lines until the firmware prints the `dap ready` sentinel; any `ERROR:`/`usage:`
// line before it is a handshake failure. On success the port is switched to
// blocking reads and an io.ReadWriteCloser carrying the raw framed DAP stream is
// returned — ready to hand to openocd.BridgeDAP. Its Close sends a zero-length
// frame to disarm the probe and return the console to its prompt, mirroring how
// closing the TCP connection returns the pod to a safe state. The firmware's 60s
// inactivity watchdog is the backstop if Close never runs.
//
// swclk/swdio (and the optional nreset) are LA pin numbers (1-12); the caller is
// responsible for parsing/validating them (see parseLAPin in the CLI).
//
// If the firmware never reports "dap ready" before ctx expires (or emits an
// error line), no connection is returned and the error carries the full console
// transcript captured during the handshake, so the caller can show what the pod
// actually said instead of launching OpenOCD against a link that isn't armed.
func (c *Console) DAPStart(ctx context.Context, swclk, swdio int, nreset *int) (io.ReadWriteCloser, error) {
	cmd := fmt.Sprintf("dap-start %d %d", swclk, swdio)
	if nreset != nil {
		cmd += fmt.Sprintf(" %d", *nreset)
	}
	if err := c.writeLine(cmd); err != nil {
		return nil, fmt.Errorf("write dap-start: %w", err)
	}
	if rt, ok := c.rw.(readTimeoutSetter); ok {
		_ = rt.SetReadTimeout(perReadTimeout)
	}

	// Accumulate every line so a handshake failure can report the firmware's
	// actual output rather than just the last partial read.
	var transcript strings.Builder
	record := func(line string) {
		transcript.WriteString(line)
		transcript.WriteByte('\n')
	}

	for {
		line, err := c.readLine(ctx)
		if err != nil {
			if line != "" {
				record(line)
			}
			return nil, fmt.Errorf("dap-start: pod never reported %q (%w)%s",
				dapReadySentinel, err, transcriptSuffix(transcript.String()))
		}
		record(line)
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == dapReadySentinel:
			// Raw DAP mode is open for the whole flash session, which can outlast
			// any per-read timeout — switch to blocking reads so the bridge's
			// pumps block until data/close rather than spinning.
			if rt, ok := c.rw.(readTimeoutSetter); ok {
				_ = rt.SetReadTimeout(serial.NoTimeout)
			}
			return &dapConn{rw: c.rw}, nil
		case strings.HasPrefix(trimmed, "ERROR:"), strings.HasPrefix(trimmed, "usage:"):
			return nil, fmt.Errorf("dap-start rejected by firmware: %s%s",
				trimmed, transcriptSuffix(transcript.String()))
		}
		// Otherwise: the command echo or an async log line — keep reading.
	}
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
			if errors.Is(err, io.EOF) {
				return string(buf), errPortVanished
			}
			return string(buf), fmt.Errorf("read serial: %w", err)
		}
		// n==0, err==nil is a per-read timeout; loop under the ctx guard above.
	}
}

// dapConn is the raw framed CMSIS-DAP stream returned by DAPStart. Read/Write
// pass straight through to the serial port; Close (idempotent) sends a
// zero-length frame to disarm the probe, then closes the port. It deliberately
// does NOT read after the leave frame: openocd.BridgeDAP closes this connection
// while its own pump goroutine is still reading the same port, so a concurrent
// read here would race it. Closing the port unblocks that goroutine, and the
// leave frame (with the 60s watchdog as a backstop) is enough to leave the
// firmware in a safe state.
type dapConn struct {
	rw        io.ReadWriteCloser
	closeOnce sync.Once
	closeErr  error
}

func (s *dapConn) Read(p []byte) (int, error)  { return s.rw.Read(p) }
func (s *dapConn) Write(p []byte) (int, error) { return s.rw.Write(p) }

func (s *dapConn) Close() error {
	s.closeOnce.Do(func() {
		if _, err := s.rw.Write([]byte{0x00, 0x00}); err == nil {
			// Let the zero-length leave frame flush over USB before dropping the port.
			time.Sleep(dapFlushDelay)
		}
		s.closeErr = s.rw.Close()
	})
	return s.closeErr
}

// quoteArg double-quotes an argument for the firmware shell. Embedded double
// quotes are not currently supported by the firmware parser, so we reject them
// with a clear error rather than guess at an escaping scheme.
func quoteArg(s string) (string, error) {
	if strings.Contains(s, `"`) {
		return "", errors.New(`value may not contain a double-quote (") character`)
	}
	return `"` + s + `"`, nil
}

// maskPassword replaces occurrences of the cleartext password in captured
// output with "***" so it is never logged. No-op for empty passwords.
func maskPassword(out, password string) string {
	if password == "" {
		return out
	}
	return strings.ReplaceAll(out, password, "***")
}

// parseIPAfter returns the value of an "ip=<value>" token appearing after the
// given marker in s, or "" if none is found.
func parseIPAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, "ip=")
	if j < 0 {
		return ""
	}
	return firstToken(rest[j+len("ip="):])
}

// fieldValue scans s line-by-line for a "<label>:" or "<label>=" prefix
// (case-insensitive) and returns the trimmed remainder of that line. Returns ""
// if the label is not found.
func fieldValue(s, label string) string {
	label = strings.ToLower(label)
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		for _, sep := range []string{":", "="} {
			prefix := label + sep
			if strings.HasPrefix(lower, prefix) {
				return strings.TrimSpace(trimmed[len(prefix):])
			}
		}
	}
	return ""
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
