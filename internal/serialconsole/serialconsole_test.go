package serialconsole

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial/enumerator"
)

// fakeConsole is an in-memory io.ReadWriteCloser that simulates the firmware
// serial console. onWrite is invoked for each written command line (the
// trailing "\n" stripped) and stages the device's reply — echo, interleaved
// async log lines, markers, and the "> " prompt — into the read buffer. When
// closeAfter is set, once the staged bytes are drained Read returns io.EOF,
// simulating the port vanishing on `bootsel`.
type fakeConsole struct {
	mu         sync.Mutex
	toRead     bytes.Buffer
	written    bytes.Buffer // logical command bytes (clear-line backspaces stripped)
	rawWritten bytes.Buffer // every byte written, including the clear-line prefix
	onWrite    func(line string, out *bytes.Buffer)
	closeAfter bool
	closed     bool
}

func (f *fakeConsole) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.toRead.Len() == 0 {
		if f.closeAfter || f.closed {
			return 0, io.EOF
		}
		return 0, nil // per-read timeout, like go.bug.st/serial
	}
	return f.toRead.Read(p)
}

func (f *fakeConsole) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawWritten.Write(p)
	// The firmware treats a backspace on an empty line as a no-op; the CLI's
	// clear-line prefix is all backspaces, so strip them to recover the logical
	// command bytes the rest of the fake (and the assertions) care about.
	logical := bytes.ReplaceAll(p, []byte{0x08}, nil)
	f.written.Write(logical)
	line := strings.TrimRight(string(logical), "\n")
	if f.onWrite != nil {
		f.onWrite(line, &f.toRead)
	}
	return len(p), nil
}

func (f *fakeConsole) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeConsole) SetReadTimeout(time.Duration) error { return nil }

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func withLister(t *testing.T, fn func() ([]*enumerator.PortDetails, error)) {
	t.Helper()
	portLister = fn
	t.Cleanup(func() { portLister = enumerator.GetDetailedPortsList })
}

// withGlobber stubs the /dev glob scan so candidatePorts tests don't pick up the
// real serial devices on the test machine.
func withGlobber(t *testing.T, names ...string) {
	t.Helper()
	saved := globPorts
	globPorts = func() []string { return names }
	t.Cleanup(func() { globPorts = saved })
}

func TestDetectPortExplicitWins(t *testing.T) {
	called := false
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		called = true
		return nil, nil
	})
	got, err := DetectPort("/dev/cu.usbmodem99")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/dev/cu.usbmodem99" {
		t.Fatalf("device: %q", got)
	}
	if called {
		t.Fatal("enumerator should not be called when explicit is set")
	}
}

func TestDetectPortSingleMatch(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.Bluetooth", IsUSB: false},
			{Name: "/dev/cu.usbserial-X", IsUSB: true, VID: "1234"},
			{Name: "/dev/cu.usbmodem1101", IsUSB: true, VID: "2e8a", PID: "0009"},
		}, nil
	})
	got, err := DetectPort("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/dev/cu.usbmodem1101" {
		t.Fatalf("device: %q", got)
	}
}

func TestDetectPortNoMatch(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.usbserial-X", IsUSB: true, VID: "1234"},
		}, nil
	})
	_, err := DetectPort("")
	if err == nil {
		t.Fatal("expected error when no VID 2E8A port present")
	}
	if !strings.Contains(err.Error(), benchPodVID) || !strings.Contains(err.Error(), "--connection") {
		t.Fatalf("error should mention VID and override flag: %v", err)
	}
}

func TestDetectPortMultipleMatches(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.usbmodemB", IsUSB: true, VID: "2E8A", Product: "Bench Pod", SerialNumber: "B2"},
			{Name: "/dev/cu.usbmodemA", IsUSB: true, VID: "2E8A", SerialNumber: "A1"},
		}, nil
	})
	_, err := DetectPort("")
	if err == nil {
		t.Fatal("expected error with multiple matches")
	}
	for _, want := range []string{"/dev/cu.usbmodemA", "/dev/cu.usbmodemB", "--connection"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
	// Sorted: A before B.
	if strings.Index(err.Error(), "usbmodemA") > strings.Index(err.Error(), "usbmodemB") {
		t.Fatalf("candidates should be sorted: %v", err)
	}
}

func TestDetectPortIgnoresNonUSB(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		// Right VID but not flagged USB -> ignored.
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.weird", IsUSB: false, VID: "2E8A"},
		}, nil
	})
	if _, err := DetectPort(""); err == nil {
		t.Fatal("non-USB port with matching VID should be ignored")
	}
}

func TestDetectPortEnumeratorError(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return nil, errors.New("not implemented")
	})
	_, err := DetectPort("")
	if err == nil || !strings.Contains(err.Error(), "--connection") {
		t.Fatalf("expected enumeration error pointing at --connection: %v", err)
	}
}

func TestWifiSetJoinOK(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString(line + "\r\n") // echo
		out.WriteString("[cfg] credentials written to flash\r\n")
		out.WriteString("[wifi] joining \"MyNet\"...\r\n")
		out.WriteString("[wifi] join OK  ip=192.168.1.42\r\n")
		out.WriteString("> ")
	}}
	res, err := newConsole(fc).WifiSet(testContext(t), "MyNet", "s3cr3t")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Persisted || !res.Joined {
		t.Fatalf("persisted=%v joined=%v", res.Persisted, res.Joined)
	}
	if res.IP != "192.168.1.42" {
		t.Fatalf("ip: %q", res.IP)
	}
	if strings.Contains(res.Raw, "s3cr3t") {
		t.Fatalf("password leaked into Raw: %q", res.Raw)
	}
	if !strings.HasPrefix(fc.written.String(), `wifi-set "MyNet" "s3cr3t"`) {
		t.Fatalf("written command: %q", fc.written.String())
	}
}

func TestWifiSetJoinFailed(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("[cfg] credentials written to flash\r\n")
		out.WriteString(`[wifi] join failed AT+CWJAP="ssid","******"` + "\r\n")
		out.WriteString("> ")
	}}
	res, err := newConsole(fc).WifiSet(testContext(t), "MyNet", "bad")
	if err == nil {
		t.Fatal("expected join-failed error")
	}
	if !res.Persisted {
		t.Fatal("credentials should still report persisted")
	}
	if res.Joined {
		t.Fatal("should not report joined")
	}
}

// TestWifiSetReconnectOutput covers wifi-set delegating to esp32-reconnect:
// success is "[wifi] connected  ip=...", and BringupLines surfaces the transcript.
func TestWifiSetReconnectOutput(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("[cfg] credentials written to flash\r\n")
		out.WriteString("[esp32] link up on EXTERNAL ESP32 (GPIO40/41)\r\n")
		out.WriteString("[wifi] connecting to \"MyNet\"...\r\n")
		out.WriteString("[wifi] connected  ip=192.168.1.42\r\n")
		out.WriteString("[tcp] listening on 192.168.1.42:8080\r\n")
		out.WriteString("> ")
	}}
	res, err := newConsole(fc).WifiSet(testContext(t), "MyNet", "pw")
	if err != nil {
		t.Fatalf("WifiSet: %v", err)
	}
	if !res.Persisted || !res.Joined {
		t.Fatalf("persisted=%t joined=%t", res.Persisted, res.Joined)
	}
	if res.IP != "192.168.1.42" {
		t.Fatalf("ip = %q, want 192.168.1.42", res.IP)
	}
	joined := strings.Join(BringupLines(res.Raw), "\n")
	for _, want := range []string{"[wifi] connected  ip=192.168.1.42", "[tcp] listening", "[esp32] link up"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("BringupLines missing %q in:\n%s", want, joined)
		}
	}
}

// TestWifiSetReconnectFailed covers the reconnect failure marker.
func TestWifiSetReconnectFailed(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("[cfg] credentials written to flash\r\n")
		out.WriteString("[wifi] connect failed — check SSID/password and AP availability.\r\n")
		out.WriteString("> ")
	}}
	res, err := newConsole(fc).WifiSet(testContext(t), "MyNet", "bad")
	if err == nil {
		t.Fatal("expected connect-failed error")
	}
	if !res.Persisted || res.Joined {
		t.Fatalf("persisted=%t joined=%t", res.Persisted, res.Joined)
	}
}

// TestWifiSetMasksPasswordOnTimeout pins that a timeout (or any) error from
// WifiSet never contains the cleartext password — neither in the echoed command
// nor in the captured "got: ..." tail — while still surfacing a masked form.
func TestWifiSetMasksPasswordOnTimeout(t *testing.T) {
	const pw = "abea89xp-secret"
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		// Firmware echoes typed chars but (here) never prints a prompt, so the
		// password lands in the captured output and WifiSet times out.
		out.WriteString(line + "\r\n")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := newConsole(fc).WifiSet(ctx, "MyNet", pw)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("error leaked the password: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("expected a masked password (***) in the error, got: %q", err.Error())
	}
}

func TestIsBenchpod(t *testing.T) {
	// A real bench pod: `status` output carries the "benchpod" marker + prompt.
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("device   : benchpod\r\n")
		out.WriteString("firmware : 0.2.0\r\n")
		out.WriteString("> ")
	}}
	if !newConsole(fc).IsBenchpod(testContext(t)) {
		t.Fatal("expected IsBenchpod=true for bench-pod status output")
	}

	// Some other serial device: emits noise, never the marker or a prompt.
	other := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("\x00\x00 not a bench pod\r\n")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if newConsole(other).IsBenchpod(ctx) {
		t.Fatal("expected IsBenchpod=false for a non-bench-pod device")
	}
}

func TestCandidatePortsOrderAndFilter(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.other", IsUSB: true, VID: "10C4"},   // CP2102 etc. (prio 1)
			{Name: "/dev/cu.pi", IsUSB: true, VID: "2E8A"},      // Raspberry Pi VID (prio 0)
			{Name: "/dev/cu.serialbus", IsUSB: false, VID: "X"}, // not USB -> excluded
		}, nil
	})
	withGlobber(t) // no globbed devices in this case

	got, err := candidatePorts()
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, got, []string{"/dev/cu.pi", "/dev/cu.other"}) // 2E8A first; non-USB dropped
}

func TestCandidatePortsIncludesGlobbedTTY(t *testing.T) {
	// Enumerator returns only the cu.* names (with VID); the globber adds the
	// tty.* dial-in variants (no VID). cu.<2E8A> first, then the rest by name.
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.usbmodem1", IsUSB: true, VID: "2E8A"},
			{Name: "/dev/cu.SLAB", IsUSB: true, VID: "10C4"},
		}, nil
	})
	withGlobber(t,
		"/dev/cu.usbmodem1", // dup of an enumerator entry -> deduped
		"/dev/tty.usbmodem1",
		"/dev/tty.usbserial-0001",
	)
	got, err := candidatePorts()
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, got, []string{
		"/dev/cu.usbmodem1",       // 2E8A (prio 0)
		"/dev/cu.SLAB",            // prio 1, sorted by name
		"/dev/tty.usbmodem1",      // globbed-only (prio 1)
		"/dev/tty.usbserial-0001", // globbed-only (prio 1)
	})
}

func TestPromptSeen(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"foo\r\n> ", true},                      // real prompt after a CRLF
		{"foo\n> ", true},                        // real prompt after a LF
		{"> ", true},                             // prompt at the very start
		{"ESP32 TX -> RP RX, RP TX -> X", false}, // "> " inside "-> " mid-line
		{"[esp32] probe attempt 1/3", false},     // no prompt at all
	}
	for _, c := range cases {
		if got := promptSeen(c.s, "> "); got != c.want {
			t.Errorf("promptSeen(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// TestSendCommandIgnoresMidLineArrow pins that sendCommand reads through a "> "
// that appears inside output (the "ESP32 TX -> RP RX" wiring hint) and only stops
// at the real line-start prompt — so a long reconnect isn't cut off early.
func TestSendCommandIgnoresMidLineArrow(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("[esp32] probe attempt 1/3\r\n")
		out.WriteString("[esp32] ERROR: no response — check wiring (ESP32 TX -> RP RX, RP TX -> ESP32 RX).\r\n")
		out.WriteString("[esp32] trying external ESP32 on GPIO40/41\r\n")
		out.WriteString("ESP32 still unreachable (tried internal + external pins).\r\n")
		out.WriteString("> ")
	}}
	out, err := newConsole(fc).sendCommand(testContext(t), "esp32-reconnect")
	if err != nil {
		t.Fatalf("sendCommand: %v", err)
	}
	if !strings.Contains(out, "ESP32 still unreachable") {
		t.Fatalf("read stopped early on a mid-line %q; got:\n%s", "> ", out)
	}
}

func TestPreferFirst(t *testing.T) {
	cands := []string{"/dev/cu.a", "/dev/cu.b", "/dev/cu.c"}
	// Hint present -> moved to front.
	assertOrder(t, preferFirst("/dev/cu.b", cands), []string{"/dev/cu.b", "/dev/cu.a", "/dev/cu.c"})
	// Hint absent -> unchanged.
	assertOrder(t, preferFirst("/dev/cu.x", cands), cands)
	// Empty hint -> unchanged.
	assertOrder(t, preferFirst("", cands), cands)
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidatePorts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("candidatePorts = %v, want %v", got, want)
		}
	}
}

func TestWifiSetInterleavedAndSplitMarkers(t *testing.T) {
	// Echo + async log lines interleaved with the command output, and the
	// join-OK marker delivered across two reads, to prove the substring scan
	// over the accumulator tolerates noise and reads that split a marker.
	fc := &fakeConsole{}
	fc.onWrite = func(line string, out *bytes.Buffer) {
		// Note: Write already holds fc.mu when it calls onWrite.
		out.WriteString("[boot] esp32 ready\r\n")
		out.WriteString(line + "\r\n") // echo
		out.WriteString("[cfg] credentials written to flash\r\n")
		out.WriteString("[wifi] reconnect attempt 1\r\n")
		out.WriteString("[wifi] join OK  ip=10.0.") // marker tail arrives next read
	}
	c := newConsole(fc)
	// Stage the remainder + prompt to be appended after the first read drains.
	go func() {
		time.Sleep(20 * time.Millisecond)
		fc.mu.Lock()
		fc.toRead.WriteString("0.7\r\n> ")
		fc.mu.Unlock()
	}()
	res, err := c.WifiSet(testContext(t), "Net", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Persisted || !res.Joined {
		t.Fatalf("persisted=%v joined=%v", res.Persisted, res.Joined)
	}
	if res.IP != "10.0.0.7" {
		t.Fatalf("ip parsed across split read: %q", res.IP)
	}
}

func TestWifiShowParse(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("wifi-show\r\n")
		out.WriteString("SSID: MyNet\r\n")
		out.WriteString("password: *\r\n")
		out.WriteString("State: connected\r\n")
		out.WriteString("IP: 192.168.1.42\r\n")
		out.WriteString("RSSI: -57 dBm\r\n")
		out.WriteString("> ")
	}}
	st, err := newConsole(fc).WifiShow(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.SSID != "MyNet" {
		t.Fatalf("ssid: %q", st.SSID)
	}
	if st.State != "connected" {
		t.Fatalf("state: %q", st.State)
	}
	if st.IP != "192.168.1.42" {
		t.Fatalf("ip: %q", st.IP)
	}
	if st.RSSI != "-57 dBm" {
		t.Fatalf("rssi: %q", st.RSSI)
	}
}

func TestStatus(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		if line != "status" {
			return
		}
		out.WriteString("status\r\n") // command echo
		out.WriteString("[boot] async log line\r\n")
		out.WriteString("firmware: v1.2.3\r\n")
		out.WriteString("uptime: 42s\r\n")
		out.WriteString("wifi: connected ip=192.168.1.42\r\n")
		out.WriteString("> ")
	}}
	got, err := newConsole(fc).Status(testContext(t))
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// Echo and the trailing prompt are stripped; content lines remain.
	if strings.Contains(got, "\r") {
		t.Fatalf("CRLF not normalised: %q", got)
	}
	if strings.HasSuffix(strings.TrimSpace(got), ">") {
		t.Fatalf("trailing prompt not stripped: %q", got)
	}
	for _, want := range []string{"firmware: v1.2.3", "uptime: 42s", "wifi: connected ip=192.168.1.42", "[boot] async log line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status output missing %q: %q", want, got)
		}
	}
	// The echoed "status" line should not survive as its own line.
	for _, ln := range strings.Split(got, "\n") {
		if strings.TrimSpace(ln) == "status" {
			t.Fatalf("command echo not stripped: %q", got)
		}
	}
	if strings.TrimSpace(fc.written.String()) != "status" {
		t.Fatalf("written command: %q", fc.written.String())
	}
}

func TestBootselEOFAsSuccess(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("entering BOOTSEL (USB UF2 drive) — drop firmware.uf2 to flash...\r\n")
	}, closeAfter: true}
	if err := newConsole(fc).Bootsel(testContext(t)); err != nil {
		t.Fatalf("bootsel should succeed on marker+EOF: %v", err)
	}
}

func TestBootselMarkerThenPrompt(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("entering BOOTSEL (USB UF2 drive)...\r\n")
		out.WriteString("> ")
	}}
	if err := newConsole(fc).Bootsel(testContext(t)); err != nil {
		t.Fatalf("bootsel should succeed on marker+prompt: %v", err)
	}
}

func TestWifiClear(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("[cfg] credentials erased\r\n> ")
	}}
	if err := newConsole(fc).WifiClear(testContext(t)); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(fc.written.String()) != "wifi-clear" {
		t.Fatalf("written: %q", fc.written.String())
	}
}

func TestSendCommandTimeout(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("[wifi] noise but no prompt\r\n") // never emits "> "
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := newConsole(fc).sendCommand(ctx, "wifi-show")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
	if !strings.Contains(err.Error(), "noise") {
		t.Fatalf("timeout error should include captured tail: %v", err)
	}
}

func TestDAPStartReadyHandshake(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		switch {
		case strings.HasPrefix(line, "dap-start"):
			out.WriteString(line + "\r\n")               // command echo
			out.WriteString("[swd] arming probe...\r\n") // async noise
			out.WriteString("dap ready\n")               // sentinel
		}
	}}
	c := newConsole(fc)
	conn, err := c.DAPStart(testContext(t), 2, 3, nil)
	if err != nil {
		t.Fatalf("DAPStart: %v", err)
	}
	if !strings.HasPrefix(fc.written.String(), "dap-start 2 3\n") {
		t.Fatalf("written command: %q", fc.written.String())
	}
	// Close must emit the zero-length leave frame to disarm the probe.
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(fc.written.String(), "\x00\x00") {
		t.Fatalf("Close did not send the zero-length leave frame: %q", fc.written.String())
	}
	// Close is idempotent.
	if err := conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestDAPStartWithNreset(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		if strings.HasPrefix(line, "dap-start") {
			out.WriteString("dap ready\n")
		}
	}}
	nreset := 9
	conn, err := newConsole(fc).DAPStart(testContext(t), 2, 3, &nreset)
	if err != nil {
		t.Fatalf("DAPStart: %v", err)
	}
	defer conn.Close()
	if !strings.HasPrefix(fc.written.String(), "dap-start 2 3 9\n") {
		t.Fatalf("nreset not included in command: %q", fc.written.String())
	}
}

func TestDAPStartRejected(t *testing.T) {
	for _, reply := range []string{
		"ERROR: gpio not in controlled set",
		"ERROR: swd busy",
		"usage: dap-start <swclk> <swdio> [nreset]",
	} {
		reply := reply
		fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
			if strings.HasPrefix(line, "dap-start") {
				out.WriteString(line + "\r\n")
				out.WriteString(reply + "\r\n")
				out.WriteString("> ")
			}
		}}
		_, err := newConsole(fc).DAPStart(testContext(t), 2, 3, nil)
		if err == nil {
			t.Fatalf("expected failure for reply %q", reply)
		}
		if !strings.Contains(err.Error(), strings.TrimSpace(reply)) {
			t.Fatalf("error should surface the reply %q: %v", reply, err)
		}
	}
}

func TestDAPStartRawStreamPassthrough(t *testing.T) {
	// After `dap ready`, the wrapper passes bytes straight through: stage a
	// sample reply and confirm Read returns it unframed.
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		if strings.HasPrefix(line, "dap-start") {
			out.WriteString("dap ready\n")
			out.WriteString("01") // raw bitbang sample replies
		}
	}}
	conn, err := newConsole(fc).DAPStart(testContext(t), 2, 3, nil)
	if err != nil {
		t.Fatalf("DAPStart: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 2)
	n, _ := conn.Read(buf)
	if string(buf[:n]) != "01" {
		t.Fatalf("raw passthrough = %q, want 01", buf[:n])
	}
}

func TestSendCommandClearsStaleLine(t *testing.T) {
	fc := &fakeConsole{onWrite: func(_ string, out *bytes.Buffer) {
		out.WriteString("> ")
	}}
	if _, err := newConsole(fc).sendCommand(testContext(t), "status"); err != nil {
		t.Fatal(err)
	}
	// The raw bytes must be a full line of backspaces (to erase any stale partial
	// input the firmware's line editor may hold) followed by the command + "\n".
	want := append(append([]byte{}, clearLineSeq...), []byte("status\n")...)
	if got := fc.rawWritten.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("raw write = %q, want %d backspaces then %q", got, lineBufSize, "status\n")
	}
	if len(clearLineSeq) != lineBufSize {
		t.Fatalf("clearLineSeq length = %d, want %d", len(clearLineSeq), lineBufSize)
	}
}

func TestDAPStartClearsStaleLine(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		if strings.HasPrefix(line, "dap-start") {
			out.WriteString("dap ready\n")
		}
	}}
	conn, err := newConsole(fc).DAPStart(testContext(t), 2, 3, nil)
	if err != nil {
		t.Fatalf("DAPStart: %v", err)
	}
	defer conn.Close()
	if !bytes.HasPrefix(fc.rawWritten.Bytes(), clearLineSeq) {
		t.Fatalf("dap-start not preceded by the clear-line prefix: %q", fc.rawWritten.Bytes())
	}
}

func TestQuoteArgRejectsEmbeddedQuote(t *testing.T) {
	if _, err := quoteArg(`my"net`); err == nil {
		t.Fatal("expected error for embedded double-quote")
	}
	got, err := quoteArg("My Net")
	if err != nil {
		t.Fatal(err)
	}
	if got != `"My Net"` {
		t.Fatalf("quoted: %q", got)
	}
}
