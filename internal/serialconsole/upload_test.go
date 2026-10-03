package serialconsole

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	busyChunkAt    int // offset of a chunk refused once with "busy" (-1 = none)
	committed      map[string][]byte
	versions       map[string]int
}

func newPodUploadSim() *podUploadSim {
	return &podUploadSim{corruptChunkAt: -1, busyChunkAt: -1,
		committed: map[string][]byte{}, versions: map[string]int{}}
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
	case "upload-abort":
		p.state = "idle"
		p.reply("upload-abort ok")
	case "blobs":
		p.out.WriteString("blob gw0 ok 104090 45 cd410deff3d6ce5d08d591daf70adbb46a0b3ae87e7e7f6ca2a545bb48e99192\r\n")
		p.out.WriteString("[wifi] some log line\r\n")
		p.out.WriteString("blob gw1 outdated 104090 44 0898c1f0d3693d2c39f7d67497a152edb5d0c27b4e4cb15654964886a430b20f\r\n")
		p.reply("blob esp missing 0 0 -")
	default:
		p.out.WriteString("> ")
	}
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
