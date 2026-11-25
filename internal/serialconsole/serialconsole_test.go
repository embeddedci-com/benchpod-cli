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

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
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
	saved := portLister
	portLister = fn
	t.Cleanup(func() { portLister = saved })
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
	for _, vid := range benchPodVIDs {
		if !strings.Contains(err.Error(), vid) {
			t.Fatalf("error should mention VID %s: %v", vid, err)
		}
	}
	if !strings.Contains(err.Error(), "--connection") {
		t.Fatalf("error should mention the override flag: %v", err)
	}
	if !errors.Is(err, tcpclient.ErrUnreachable) {
		t.Fatalf("no pod on USB should be an unreachable pod: %v", err)
	}
}

func TestOpenBenchpodWithNoPortsIsUnreachable(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) { return nil, nil })
	withGlobber(t)
	_, _, err := OpenBenchpod("", "", time.Second)
	if err == nil || !errors.Is(err, tcpclient.ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "no USB ports found") {
		t.Fatalf("text changed: %v", err)
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

// wifiShowV3Interleaved is a wifi-show reply as a BenchPod v3 prints it while
// Wi-Fi retries: async log lines before the echo, between the echo and the
// first field, and between the fields.
const wifiShowV3Interleaved = "[cloud] state -> backoff [BACKOFF]  retry in 5000 ms\r\n" +
	"wifi-show\r\n" +
	"[wifi] association failed — backoff\n" +
	"  ssid: ATTQRSKVkI\r\n" +
	"[esp] ESP32-C3 reset released — awaiting boot event\n" +
	"  state: backoff\r\n" +
	"[cloud] connect failed (no route)\n" +
	"  ip: (none)\r\n" +
	"[wifi] association failed — backoff\n" +
	"  rssi: -128 dBm\r\n" +
	"> "

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

// HasNRSTPin reads the console `status` board line, which is how flash decides
// whether connect-under-reset is possible.
func TestHasNRSTPin(t *testing.T) {
	for _, tc := range []struct {
		name  string
		board string
		want  bool
	}{
		{"v3", "  board  : stm32h563  fw v1.2.3  rev v3  nrst_pin=yes  usb_cc=yes", true},
		{"v2", "  board  : stm32h563  fw v1.2.3  rev v2  nrst_pin=no  usb_cc=no", false},
		{"old firmware", "  board  : stm32h563  fw v0.9.0", false},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
				if strings.HasPrefix(line, "status") {
					out.WriteString(line + "\r\n")
					out.WriteString("  device : benchpod\r\n")
					out.WriteString(tc.board + "\r\n")
					out.WriteString("> ")
				}
			}}
			got, err := newConsole(fc).HasNRSTPin(testContext(t))
			if err != nil {
				t.Fatalf("HasNRSTPin: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasNRSTPin = %v, want %v", got, tc.want)
			}
		})
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

func TestParseSerialPodSplitsBoardAndFirmware(t *testing.T) {
	// Verbatim shape of the STM32 firmware's `status` output (console.c), with an
	// async log line interleaved the way the console really does it.
	raw := strings.Join([]string{
		"  device : benchpod",
		"  mac    : 02:1a:2b:3c:4d:5e",
		"  ip     : 192.168.1.213",
		"[wifi] connected",
		"  board  : bench-pod  fw v1.4.2",
		"  adc    : 16-bit x2, 30000 mV full-scale",
	}, "\n")

	p := parseSerialPod("/dev/cu.usbmodem1103", raw)
	if p.Device != "/dev/cu.usbmodem1103" {
		t.Fatalf("device: %q", p.Device)
	}
	if p.Board != "bench-pod" {
		t.Fatalf("board: %q", p.Board)
	}
	if p.Firmware != "v1.4.2" {
		t.Fatalf("firmware: %q", p.Firmware)
	}
	if p.IP != "192.168.1.213" {
		t.Fatalf("ip: %q", p.IP)
	}
	if p.MAC != "02:1a:2b:3c:4d:5e" {
		t.Fatalf("mac: %q", p.MAC)
	}
	if !p.Addressed() {
		t.Fatal("a pod with a lease should report Addressed")
	}
}

func TestParseSerialPodWithoutLease(t *testing.T) {
	p := parseSerialPod("/dev/ttyACM0", "  device : benchpod\n  ip     : 0.0.0.0\n  board  : bench-pod\n")
	if p.Addressed() {
		t.Fatal("0.0.0.0 is not a usable address")
	}
	if p.Board != "bench-pod" || p.Firmware != "" {
		t.Fatalf("board=%q firmware=%q", p.Board, p.Firmware)
	}
}

func TestIsBenchPodVIDCoversBothPodGenerations(t *testing.T) {
	for _, vid := range []string{"2E8A", "2e8a", "0483", "1209"} {
		if !isBenchPodVID(vid) {
			t.Fatalf("VID %s should be recognised as a bench-pod console", vid)
		}
	}
	if isBenchPodVID("1234") {
		t.Fatal("an unrelated VID should not be recognised")
	}
}

func TestCandidatePortsOrdersBothPodVIDsFirst(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.usbserial-other", IsUSB: true, VID: "1234"},
			{Name: "/dev/cu.usbmodem-stm", IsUSB: true, VID: "0483", PID: "5740"},
		}, nil
	})
	withGlobber(t)

	got, err := candidatePorts()
	if err != nil {
		t.Fatal(err)
	}
	// The STM32 pod's VID must sort ahead of an unrelated USB serial device, so
	// the real pod is probed first rather than after every other adapter.
	if len(got) == 0 || got[0] != "/dev/cu.usbmodem-stm" {
		t.Fatalf("candidate order = %v, want the 0483 port first", got)
	}
}

func TestParseSerialPodReadsCloudRegistration(t *testing.T) {
	// Verbatim shape of the firmware's registered cloud line (console.c).
	raw := strings.Join([]string{
		"  device : benchpod",
		"  ip     : 192.168.1.213",
		"  board  : bench-pod  fw v1.4.2",
		"  cloud  : registered  state=connected  device_id=7f3a91c2-0000-4000-8000-abcdef012345",
		"  reset  : power-on",
	}, "\n")

	p := parseSerialPod("/dev/cu.usbmodem1103", raw)
	if !p.CloudKnown {
		t.Fatal("a firmware that printed the cloud line should be CloudKnown")
	}
	if !p.Registered {
		t.Fatal("registered pod not detected")
	}
	if p.CloudState != "connected" {
		t.Fatalf("cloud state: got %q want connected", p.CloudState)
	}
	if p.DeviceID != "7f3a91c2-0000-4000-8000-abcdef012345" {
		t.Fatalf("device id: got %q", p.DeviceID)
	}
}

func TestParseSerialPodReadsUnregistered(t *testing.T) {
	raw := "  device : benchpod\n  board  : bench-pod  fw v1.4.2\n  cloud  : not registered\n"
	p := parseSerialPod("/dev/ttyACM0", raw)
	if !p.CloudKnown {
		t.Fatal("the line was present, so the state is known")
	}
	if p.Registered {
		t.Fatal("an unregistered pod was read as registered")
	}
	if p.CloudState != "" || p.DeviceID != "" {
		t.Fatalf("unregistered pod carried state=%q device_id=%q", p.CloudState, p.DeviceID)
	}
}

// Firmware predating the cloud line must read as UNKNOWN, never as unregistered: telling
// somebody their pod is unclaimed when we cannot tell would send them to re-register a pod that
// is already working.
func TestParseSerialPodOlderFirmwareLeavesCloudUnknown(t *testing.T) {
	raw := "  device : benchpod\n  ip     : 192.168.1.213\n  board  : bench-pod  fw v1.3.0\n"
	p := parseSerialPod("/dev/ttyACM0", raw)
	if p.CloudKnown {
		t.Fatal("firmware with no cloud line must leave the registration unknown")
	}
	if p.Registered {
		t.Fatal("unknown must not read as registered")
	}
}

// portGone: EOF and wrapped EOF count; an unrelated error does not. (go.bug.st/serial's
// PortClosed error cannot be built outside its package; portGone matches it by Code().)
func TestPortGone(t *testing.T) {
	if !portGone(io.EOF) || !portGone(errors.Join(errors.New("read"), io.EOF)) {
		t.Error("EOF must count as the port going away")
	}
	if portGone(errors.New("read serial: some other failure")) {
		t.Error("an unrelated error must not count")
	}
}

// slowEchoConsole delivers the echo and reply a little after the write, like a real port: a
// stale prompt already in the input arrives first.
type slowEchoConsole struct {
	fakeConsole
	resets int
}

func (s *slowEchoConsole) ResetInputBuffer() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets++
	s.toRead.Reset()
	return nil
}

// A prompt left over from an earlier command must not end the read before the reply: the
// input is flushed before the write.
func TestStalePromptIsFlushedBeforeTheCommand(t *testing.T) {
	fc := &slowEchoConsole{}
	fc.toRead.WriteString("\r\n> ") // stale
	fc.onWrite = func(line string, out *bytes.Buffer) {
		out.WriteString(line + "\r\n")
		out.WriteString("device   : benchpod\r\nfirmware : 3.6.0\r\n> ")
	}
	out, err := newConsole(fc).Status(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if fc.resets != 1 || !strings.Contains(out, "firmware : 3.6.0") {
		t.Fatalf("resets=%d out=%q", fc.resets, out)
	}
}

// Without a way to flush (or with the stale prompt arriving late), the prompt only counts once
// it follows the command's echo.
func TestStalePromptBeforeTheEchoDoesNotEndTheRead(t *testing.T) {
	fc := &fakeConsole{}
	fc.toRead.WriteString("\r\n> ") // stale, read before the echo
	pending := ""
	fc.onWrite = func(line string, out *bytes.Buffer) {
		pending = line + "\r\ndevice   : benchpod\r\nfirmware : 3.6.0\r\n> "
	}
	go func() {
		// The echo and reply arrive after the stale prompt has been read.
		for {
			time.Sleep(30 * time.Millisecond)
			fc.mu.Lock()
			if pending != "" {
				fc.toRead.WriteString(pending)
				fc.mu.Unlock()
				return
			}
			fc.mu.Unlock()
		}
	}()
	out, err := newConsole(fc).Status(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "firmware : 3.6.0") {
		t.Fatalf("read ended at the stale prompt: %q", out)
	}
}

// A console that answers without echoing still works, echoGrace later.
func TestPromptWithoutEchoIsAcceptedAfterEchoGrace(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("device   : benchpod\r\n> ")
	}}
	start := time.Now()
	out, err := newConsole(fc).Status(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "benchpod") || time.Since(start) > echoGrace+400*time.Millisecond {
		t.Fatalf("out=%q after %v", out, time.Since(start))
	}
}

func TestParseSerialPodMACs(t *testing.T) {
	p := parseSerialPod("/dev/cu.usbmodem1103", "  device : benchpod\n  mac    : 02:35:c7:76:b7:9b\n  wifimac: 7c:e8:b1:c5:ca:6c\n  ip     : 0.0.0.0\n")
	if p.MAC != "02:35:c7:76:b7:9b" || p.WiFiMAC != "7c:e8:b1:c5:ca:6c" {
		t.Fatalf("MAC %q, WiFiMAC %q", p.MAC, p.WiFiMAC)
	}
	old := parseSerialPod("/dev/cu.usbmodem1103", "  device : benchpod\n  mac    : 02:35:c7:76:b7:9b\n")
	if old.WiFiMAC != "" {
		t.Fatalf("older firmware: WiFiMAC %q", old.WiFiMAC)
	}
}

func TestCandidatePortsRanksOwnIDFirst(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{
			{Name: "/dev/cu.usbmodem-a-other", IsUSB: true, VID: "1234", PID: "0001"},
			{Name: "/dev/cu.usbmodem-b-st", IsUSB: true, VID: "0483", PID: "5740"},
			{Name: "/dev/cu.usbmodem-c-pidcodes", IsUSB: true, VID: "1209", PID: "0002"},
			{Name: "/dev/cu.usbmodem-z-pod", IsUSB: true, VID: "1209", PID: "ec10"},
		}, nil
	})
	withGlobber(t)
	got, err := candidatePorts()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/dev/cu.usbmodem-z-pod", "/dev/cu.usbmodem-b-st", "/dev/cu.usbmodem-c-pidcodes", "/dev/cu.usbmodem-a-other"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestPortPrio(t *testing.T) {
	for _, c := range []struct {
		vid, pid string
		want     int
	}{{"1209", "EC10", 0}, {"1209", "ec10", 0}, {"1209", "0001", 1}, {"0483", "5740", 1}, {"2E8A", "000A", 1}, {"1234", "EC10", 2}, {"", "", 2}} {
		if got := portPrio(c.vid, c.pid); got != c.want {
			t.Errorf("portPrio(%q,%q) = %d, want %d", c.vid, c.pid, got, c.want)
		}
	}
}
