package serialconsole

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// podUploadSim models the firmware's side of an upload byte by byte, as console.c and
// upload_rx.c do: a line editor that echoes, upload-data lines followed by raw bytes that never
// reach the editor, and the staged image checked against its SHA-256. It can corrupt or refuse
// a chunk once to exercise the retry paths.
type podUploadSim struct {
	mu      sync.Mutex
	out     bytes.Buffer
	line    []byte
	raw     int // raw bytes still expected for the current chunk
	rawOff  int
	rawCRC  uint32
	rawBuf  []byte
	staged  []byte
	size    int
	sha     string
	target  string
	version int
	state   string

	corruptChunkAt int // offset of a chunk whose first arrival is corrupted (-1 = none)
	muteReplyAt    int // offset of a chunk staged fine but whose reply is lost (-1 = none)
	received       int // staged high-water mark, as upload-status reports it
	busyChunkAt    int // offset of a chunk refused once with "busy" (-1 = none)
	committed      map[string][]byte
	versions       map[string]int

	// Signed manifests (upload-sig), as firmware that checks signatures takes them. Without
	// sigAware the sim answers upload-sig like older firmware: unknown command.
	sigAware   bool
	sigB64     string            // halves collected for the next upload-begin
	sigAtBegin map[string][]byte // the manifest each target's upload-begin used
	sigResult  string            // the last begin's check
	commands   []string          // every command line the pod executed
}

func newPodUploadSim() *podUploadSim {
	return &podUploadSim{corruptChunkAt: -1, busyChunkAt: -1, muteReplyAt: -1,
		committed: map[string][]byte{}, versions: map[string]int{},
		sigAtBegin: map[string][]byte{}, sigResult: "none"}
}

func (p *podUploadSim) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.out.Len() == 0 {
		return 0, nil
	}
	return p.out.Read(b)
}

func (p *podUploadSim) Close() error                       { return nil }
func (p *podUploadSim) SetReadTimeout(time.Duration) error { return nil }

func (p *podUploadSim) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range b {
		p.feed(c)
	}
	return len(b), nil
}

func (p *podUploadSim) feed(c byte) {
	if p.raw > 0 {
		p.rawBuf = append(p.rawBuf, c)
		p.raw--
		if p.raw == 0 {
			p.chunkDone()
		}
		return
	}
	switch {
	case c == 0x08:
		if len(p.line) > 0 {
			p.line = p.line[:len(p.line)-1]
		}
	case c == '\n' || c == '\r':
		ln := string(p.line)
		p.line = p.line[:0]
		p.out.WriteString("\r\n")
		p.exec(ln)
	default:
		p.line = append(p.line, c)
		p.out.WriteByte(c) // echo
	}
}

func (p *podUploadSim) reply(format string, a ...any) {
	fmt.Fprintf(&p.out, format+"\r\n> ", a...)
}

func (p *podUploadSim) exec(ln string) {
	f := strings.Fields(ln)
	if len(f) == 0 {
		p.out.WriteString("> ")
		return
	}
	if f[0] != "upload-data" {
		p.commands = append(p.commands, ln)
	}
	switch f[0] {
	case "upload-data":
		off, _ := strconv.Atoi(f[1])
		n, _ := strconv.Atoi(f[2])
		crc, _ := strconv.ParseUint(f[3], 16, 32)
		p.raw, p.rawOff, p.rawCRC, p.rawBuf = n, off, uint32(crc), nil
	case "upload-begin":
		p.target = f[1]
		p.size, _ = strconv.Atoi(f[2])
		p.sha = f[3]
		p.version, _ = strconv.Atoi(f[4])
		p.staged = make([]byte, p.size)
		if p.sigAware {
			p.sigResult = "none"
			if p.sigB64 != "" {
				sig, err := base64.RawURLEncoding.DecodeString(p.sigB64)
				p.sigResult = "format"
				if err == nil && len(sig) == 128 {
					p.sigAtBegin[p.target] = sig
					p.sigResult = "ok"
				}
			}
			p.sigB64 = ""
		}
		p.received = 0
		p.state = "receiving"
		p.out.WriteString("[ota] begin\r\n") // log noise between echo and reply
		p.reply("upload-begin ok")
	case "upload-end":
		sum := sha256.Sum256(p.staged)
		if hex.EncodeToString(sum[:]) != p.sha {
			p.state = "error"
			p.reply("upload-end error sha256 mismatch")
			return
		}
		p.state = "verified"
		p.reply("upload-end ok")
	case "upload-commit":
		if p.state != "verified" {
			p.reply("upload-commit error no verified image staged")
			return
		}
		p.committed[p.target] = append([]byte(nil), p.staged...)
		p.versions[p.target] = p.version
		if p.target == "firmware" {
			p.reply("upload-commit resetting")
			return
		}
		p.reply("upload-commit ok")
	case "upload-status":
		p.reply("upload-status %s %s %d/%d -", p.state, p.target, p.received, p.size)
	case "upload-abort":
		p.state = "idle"
		p.reply("upload-abort ok")
	case "blobs":
		p.out.WriteString("blob gw0 ok 104090 45 cd410deff3d6ce5d08d591daf70adbb46a0b3ae87e7e7f6ca2a545bb48e99192\r\n")
		p.out.WriteString("[wifi] some log line\r\n")
		p.out.WriteString("blob gw1 outdated 104090 44 0898c1f0d3693d2c39f7d67497a152edb5d0c27b4e4cb15654964886a430b20f\r\n")
		p.reply("blob esp missing 0 0 -")
	case "upload-sig":
		if !p.sigAware {
			p.unknown(f[0])
			return
		}
		switch {
		case len(f) == 1:
			kid := "-"
			if p.sigResult == "ok" {
				kid = "ed4242ead4ac6948"
			}
			p.reply("upload-sig result %s %s", p.sigResult, kid)
		case f[1] == "clear":
			p.sigB64 = ""
			p.reply("upload-sig ok")
		case len(f) == 3 && (f[1] == "0" || f[1] == "1"):
			if f[1] == "0" {
				p.sigB64 = ""
			}
			p.sigB64 += f[2]
			p.reply("upload-sig ok")
		default:
			p.reply("upload-sig error usage: upload-sig <0|1> <base64url> | clear")
		}
	default:
		p.unknown(f[0])
	}
}

// unknown answers a command the firmware does not have, as console.c does.
func (p *podUploadSim) unknown(cmd string) {
	p.reply("  unknown command '%s' (try 'help')", cmd)
}

func (p *podUploadSim) chunkDone() {
	buf := p.rawBuf
	if p.rawOff == p.corruptChunkAt {
		p.corruptChunkAt = -1
		buf = append([]byte(nil), buf...)
		buf[0] ^= 0xFF
	}
	if crc32.ChecksumIEEE(buf) != p.rawCRC {
		p.reply("upload-data retry crc")
		return
	}
	if p.rawOff == p.busyChunkAt {
		p.busyChunkAt = -1
		p.reply("upload-data busy")
		return
	}
	copy(p.staged[p.rawOff:], buf)
	if end := p.rawOff + len(buf); end > p.received {
		p.received = end
	}
	if p.rawOff == p.muteReplyAt {
		p.muteReplyAt = -1
		return // staged, but the reply never reaches the host
	}
	p.reply("upload-data ok %d", p.rawOff)
}

func testImage(n int) []byte {
	b := make([]byte, n)
	x := uint32(7)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x) // includes '\r', '\n' and 0x08, which must travel as data
	}
	return b
}

func TestUploadBlobWithRetries(t *testing.T) {
	sim := newPodUploadSim()
	sim.corruptChunkAt = 3 * UploadChunk
	sim.busyChunkAt = 7 * UploadChunk
	c := newConsole(sim)
	img := testImage(10*UploadChunk + 123)

	var calls int
	err := c.Upload(context.Background(), "gw1", img, 46, func(done, total int) { calls++ })
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !bytes.Equal(sim.committed["gw1"], img) {
		t.Fatalf("the pod committed different bytes than were sent")
	}
	if sim.versions["gw1"] != 46 {
		t.Fatalf("version %d, want 46", sim.versions["gw1"])
	}
	if calls != 11 {
		t.Fatalf("progress called %d times, want 11", calls)
	}
}

func TestUploadFirmwareResets(t *testing.T) {
	sim := newPodUploadSim()
	c := newConsole(sim)
	img := testImage(2000)
	if err := c.Upload(context.Background(), "firmware", img, 0, nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !bytes.Equal(sim.committed["firmware"], img) {
		t.Fatalf("firmware not committed")
	}
}

// A chunk that never arrives intact is an error after a few attempts, not a hang.
func TestUploadGivesUpOnPersistentCorruption(t *testing.T) {
	sim := newPodUploadSim()
	c := newConsole(&alwaysCorrupt{podUploadSim: sim})
	err := c.Upload(context.Background(), "esp", testImage(1500), 0, nil)
	if err == nil || !strings.Contains(err.Error(), "retry crc") {
		t.Fatalf("expected a retry-crc failure, got %v", err)
	}
	if _, ok := sim.committed["esp"]; ok {
		t.Fatalf("a failed upload was committed")
	}
}

// alwaysCorrupt corrupts the first chunk on every attempt.
type alwaysCorrupt struct{ *podUploadSim }

func (a *alwaysCorrupt) Write(b []byte) (int, error) {
	a.mu.Lock()
	a.corruptChunkAt = 0
	a.mu.Unlock()
	return a.podUploadSim.Write(b)
}

func TestBlobsParse(t *testing.T) {
	sim := newPodUploadSim()
	c := newConsole(sim)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	slots, err := c.Blobs(ctx)
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	if len(slots) != 3 {
		t.Fatalf("got %d slots: %+v", len(slots), slots)
	}
	if slots[0].Name != "gw0" || slots[0].State != "ok" || slots[0].Size != 104090 || slots[0].Version != 45 || slots[0].NeedsInstall() {
		t.Fatalf("gw0: %+v", slots[0])
	}
	if !slots[1].NeedsInstall() || !slots[2].NeedsInstall() || slots[2].SHA256 != "" {
		t.Fatalf("gw1/esp: %+v %+v", slots[1], slots[2])
	}
}

// A chunk that landed but whose reply was lost (a console busy right after boot) is confirmed
// with upload-status rather than failing the upload.
func TestUploadSurvivesALostReply(t *testing.T) {
	orig := uploadReplyTimeout
	uploadReplyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { uploadReplyTimeout = orig })
	sim := newPodUploadSim()
	sim.muteReplyAt = 2 * UploadChunk
	c := newConsole(sim)
	img := testImage(4*UploadChunk + 9)
	if err := c.Upload(context.Background(), "gw0", img, 45, nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !bytes.Equal(sim.committed["gw0"], img) {
		t.Fatal("the pod committed different bytes than were sent")
	}
}

func testManifest() []byte {
	m := make([]byte, ManifestLen)
	for i := range m {
		m[i] = byte(i * 13)
	}
	copy(m, "BPSG")
	return m
}

// sigLines are the upload-sig commands the pod executed.
func (p *podUploadSim) sigLines() []string {
	var out []string
	for _, ln := range p.commands {
		if strings.HasPrefix(ln, "upload-sig") {
			out = append(out, ln)
		}
	}
	return out
}

// Firmware that checks signatures gets the manifest in two halves before upload-begin, and its
// check of the upload is read back. The probe runs once per console session.
func TestUploadSignedNewFirmware(t *testing.T) {
	sim := newPodUploadSim()
	sim.sigAware = true
	c := newConsole(sim)
	img := testImage(3*UploadChunk + 5)
	man := testManifest()

	rep, err := c.UploadSigned(context.Background(), "gw1", img, 46, man, nil)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !rep.Supported || !rep.Sent || rep.Result != "ok" || rep.KeyID != "ed4242ead4ac6948" {
		t.Fatalf("report %+v", rep)
	}
	if !bytes.Equal(sim.sigAtBegin["gw1"], man) {
		t.Fatal("upload-begin did not get the manifest that was sent")
	}
	if !bytes.Equal(sim.committed["gw1"], img) {
		t.Fatal("the pod committed different bytes than were sent")
	}
	lines := sim.sigLines()
	b64 := base64.RawURLEncoding.EncodeToString(man)
	want := []string{"upload-sig", "upload-sig 0 " + b64[:86], "upload-sig 1 " + b64[86:], "upload-sig"}
	if len(b64) != 171 || strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("upload-sig lines %q, want %q", lines, want)
	}
	for _, ln := range lines {
		if len(ln) >= 128 {
			t.Fatalf("line of %d chars does not fit the console: %q", len(ln), ln)
		}
	}
	// The order on the wire: probe, both halves, then upload-begin, then the query.
	var order []string
	for _, ln := range sim.commands {
		order = append(order, strings.Fields(ln)[0])
	}
	if got := strings.Join(order[:5], " "); got != "upload-sig upload-sig upload-sig upload-begin upload-sig" {
		t.Fatalf("command order %q", got)
	}

	// A second upload on the same console does not probe again.
	sim.commands = nil
	if _, err := c.UploadSigned(context.Background(), "esp", testImage(700), 0, man, nil); err != nil {
		t.Fatalf("second upload: %v", err)
	}
	if n := len(sim.sigLines()); n != 3 {
		t.Fatalf("second upload sent %d upload-sig lines, want 3 (two halves + query): %q", n, sim.sigLines())
	}
}

// Older firmware answers the probe with "unknown command": nothing else about signatures is
// sent and the upload goes ahead as before.
func TestUploadSignedOldFirmware(t *testing.T) {
	sim := newPodUploadSim()
	c := newConsole(sim)
	img := testImage(2*UploadChunk + 77)
	man := testManifest()
	for i := 0; i < 2; i++ {
		rep, err := c.UploadSigned(context.Background(), "gw0", img, 45, man, nil)
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
		if rep.Supported || rep.Sent || rep.Result != "" {
			t.Fatalf("report %+v", rep)
		}
	}
	if lines := sim.sigLines(); len(lines) != 1 || lines[0] != "upload-sig" {
		t.Fatalf("upload-sig lines %q, want only the one probe", lines)
	}
	if !bytes.Equal(sim.committed["gw0"], img) {
		t.Fatal("the pod committed different bytes than were sent")
	}
}

// Without a manifest nothing about signatures goes to the pod, new firmware or not.
func TestUploadWithoutManifest(t *testing.T) {
	sim := newPodUploadSim()
	sim.sigAware = true
	c := newConsole(sim)
	img := testImage(900)
	rep, err := c.UploadSigned(context.Background(), "esp", img, 0, nil, nil)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if rep != (SigReport{}) {
		t.Fatalf("report %+v", rep)
	}
	if err := c.Upload(context.Background(), "esp", img, 0, nil); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if lines := sim.sigLines(); len(lines) != 0 {
		t.Fatalf("upload-sig lines %q, want none", lines)
	}
	if _, ok := sim.sigAtBegin["esp"]; ok {
		t.Fatal("a manifest reached upload-begin")
	}
}

// A refused half clears what the pod collected; the upload then goes ahead without a manifest.
func TestUploadSignedRefusedHalf(t *testing.T) {
	sim := newPodUploadSim()
	sim.sigAware = true
	c := newConsole(&refuseSigHalf{podUploadSim: sim})
	img := testImage(600)
	rep, err := c.UploadSigned(context.Background(), "gw1", img, 46, testManifest(), nil)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !rep.Supported || rep.Sent || rep.Result != "none" {
		t.Fatalf("report %+v", rep)
	}
	if _, ok := sim.sigAtBegin["gw1"]; ok {
		t.Fatal("a half manifest reached upload-begin")
	}
	if !bytes.Equal(sim.committed["gw1"], img) {
		t.Fatal("not committed")
	}
}

// refuseSigHalf turns the second half into a malformed command, so the pod answers with an error.
type refuseSigHalf struct{ *podUploadSim }

func (r *refuseSigHalf) Write(b []byte) (int, error) {
	if i := bytes.Index(b, []byte("upload-sig 1 ")); i >= 0 {
		b = append(append([]byte(nil), b[:i]...), []byte("upload-sig 7 x\n")...)
	}
	return r.podUploadSim.Write(b)
}
