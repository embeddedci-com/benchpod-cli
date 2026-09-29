package spiflash

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakePod models the firmware's SPI commands over an in-memory 16 MB NOR part:
// erase sets whole 4 KB sectors to 0xFF, write can only clear bits, and every
// request is logged.
type fakePod struct {
	mu     sync.Mutex
	mem    []byte
	armed  bool
	nrst   bool
	log    []string
	reqs   []map[string]any
	failOn string // "op" or "cmd" whose Nth occurrence fails
	failAt int
	seen   map[string]int
	caps   []string
	busy   bool
}

func newFakePod() *fakePod {
	m := make([]byte, AddrSpace)
	for i := range m {
		m[i] = 0x5A // not erased
	}
	return &fakePod{mem: m, seen: map[string]int{}, caps: []string{"swd", Cap}}
}

func (f *fakePod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Round-trip through JSON like the wire does.
	raw, _ := json.Marshal(req)
	var r map[string]any
	_ = json.Unmarshal(raw, &r)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	name := r["cmd"].(string)
	if op, ok := r["op"].(string); ok {
		name = op
	}
	f.log = append(f.log, name)
	f.seen[name]++
	if f.failOn == name && f.seen[name] == f.failAt {
		return nil, fmt.Errorf("injected %s failure", name)
	}
	num := func(k string) int { v, _ := r[k].(float64); return int(v) }
	ok := func(v any) (json.RawMessage, error) { b, _ := json.Marshal(v); return b, nil }

	switch r["cmd"] {
	case "status":
		return ok(map[string]any{"caps": f.caps})
	case "nrst":
		f.nrst = r["assert"].(bool)
		return ok(map[string]any{"asserted": f.nrst})
	case "spi_start":
		if f.busy || f.armed {
			return nil, errors.New("spi busy: send spi_stop first")
		}
		f.armed = true
		return ok(map[string]any{"sck": num("sck"), "mosi": num("mosi"), "miso": num("miso"), "cs": num("cs"), "hz": 1000000, "mode": num("mode")})
	case "spi_stop":
		f.armed = false
		return ok("spi stopped")
	case "spi_flash":
		if !f.armed {
			return nil, errors.New("no SPI session: send spi_start first")
		}
		addr, n := num("addr"), num("len")
		switch r["op"] {
		case "id":
			return ok(map[string]any{"id": "ef4017", "present": true, "size": 8 << 20, "status": 0})
		case "read":
			if n < 1 || n > MaxRead {
				return nil, errors.New("read needs addr and len 1..1024")
			}
			return ok(map[string]any{"addr": addr, "len": n, "data": EncodeB64(f.mem[addr : addr+n])})
		case "erase":
			if n < 1 || n > MaxErase {
				return nil, errors.New("erase needs addr and len 1..1048576")
			}
			s := addr &^ 0xFFF
			e := (addr + n + 0xFFF) &^ 0xFFF
			for i := s; i < e; i++ {
				f.mem[i] = 0xFF
			}
			return ok(map[string]any{"addr": s, "len": e - s, "ms": 1})
		case "chip_erase":
			for i := range f.mem {
				f.mem[i] = 0xFF
			}
			return ok(map[string]any{"ms": 5})
		case "write":
			s, _ := r["data"].(string)
			if strings.ContainsAny(s, "+/=") {
				return nil, errors.New("data must be 1..768 bytes of base64url")
			}
			d, err := DecodeB64(s)
			if err != nil || len(d) == 0 || len(d) > MaxWrite {
				return nil, errors.New("data must be 1..768 bytes of base64url")
			}
			for i, b := range d {
				f.mem[addr+i] &= b
			}
			verify := true
			if v, ok := r["verify"].(bool); ok {
				verify = v
			}
			if verify && !bytes.Equal(f.mem[addr:addr+len(d)], d) {
				return nil, fmt.Errorf("verify failed at 0x%06x: not erased, write-protected, or a bad wire", addr)
			}
			return ok(map[string]any{"addr": addr, "len": len(d), "verified": verify, "ms": 1})
		}
	}
	return nil, errors.New("unknown cmd")
}

func (f *fakePod) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

var cfg = Config{SCK: 3, MOSI: 4, MISO: 5, CS: 6, Hz: 2000000}

func TestB64URLUnpadded(t *testing.T) {
	// 0xfb 0xff encodes to "-_8" in base64url; std base64 would be "+/8=".
	if got := EncodeB64([]byte{0xfb, 0xff}); got != "-_8" {
		t.Fatalf("EncodeB64 = %q, want -_8", got)
	}
	for _, in := range []string{"-_8", "-_8="} {
		b, err := DecodeB64(in)
		if err != nil || !bytes.Equal(b, []byte{0xfb, 0xff}) {
			t.Fatalf("DecodeB64(%q) = %x, %v", in, b, err)
		}
	}
}

func TestWriteChunksAndVerify(t *testing.T) {
	f := newFakePod()
	p := &Pod{C: f}
	ctx := context.Background()
	data := randBytes(3*MaxWrite + 100)
	const addr = 0x1000
	if _, err := p.Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := p.Erase(ctx, addr, len(data), nil); err != nil {
		t.Fatal(err)
	}
	var last int
	if err := p.Write(ctx, addr, data, true, func(d, total int) { last = d }); err != nil {
		t.Fatal(err)
	}
	if last != len(data) {
		t.Fatalf("progress ended at %d, want %d", last, len(data))
	}
	if !bytes.Equal(f.mem[addr:addr+len(data)], data) {
		t.Fatal("flash content differs")
	}
	var writes []int
	var sizes []int
	for _, r := range f.reqs {
		if r["op"] == "write" {
			writes = append(writes, int(r["addr"].(float64)))
			d, _ := DecodeB64(r["data"].(string))
			sizes = append(sizes, len(d))
			if r["verify"] != true {
				t.Fatalf("verify not sent: %v", r)
			}
		}
	}
	wantAddr := []int{addr, addr + 768, addr + 1536, addr + 2304}
	if !reflect.DeepEqual(writes, wantAddr) || !reflect.DeepEqual(sizes, []int{768, 768, 768, 100}) {
		t.Fatalf("write chunks addr=%v sizes=%v", writes, sizes)
	}
	// Read back through the read path, which splits at 1024.
	got, err := p.Read(ctx, addr, len(data), nil)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back: %v", err)
	}
}

func TestWriteVerifyFailureSurfaces(t *testing.T) {
	f := newFakePod()
	p := &Pod{C: f}
	ctx := context.Background()
	_, _ = p.Start(ctx, cfg)
	// Not erased: the fake flash can only clear bits, so verify fails.
	err := p.Write(ctx, 0, randBytes(10), true, nil)
	if err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("got %v, want verify failure", err)
	}
}

func TestEraseStepsOneMBAndSkipsErased(t *testing.T) {
	f := newFakePod()
	p := &Pod{C: f}
	ctx := context.Background()
	_, _ = p.Start(ctx, cfg)
	// Unaligned 2.5 MB range: the first step's reply ends past addr+1MB (sector
	// rounding), so the next step must start there.
	const addr = 0x800
	n := 2*MaxErase + MaxErase/2
	if err := p.Erase(ctx, addr, n, nil); err != nil {
		t.Fatal(err)
	}
	var got [][2]int
	for _, r := range f.reqs {
		if r["op"] == "erase" {
			got = append(got, [2]int{int(r["addr"].(float64)), int(r["len"].(float64))})
		}
	}
	want := [][2]int{
		{0x800, MaxErase},
		{0x101000, MaxErase},
		{0x201000, addr + n - 0x201000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("erase steps %x, want %x", got, want)
	}
	for i := addr; i < addr+n; i++ {
		if f.mem[i] != 0xFF {
			t.Fatalf("0x%x not erased", i)
		}
	}
}

func TestRangeLimits(t *testing.T) {
	p := &Pod{C: newFakePod()}
	ctx := context.Background()
	if err := p.Erase(ctx, AddrSpace-10, 20, nil); err == nil {
		t.Fatal("range past 16 MB accepted")
	}
	if _, err := p.Read(ctx, 0, 0, nil); err == nil {
		t.Fatal("zero-length read accepted")
	}
}

func TestSessionOrderAndCleanup(t *testing.T) {
	f := newFakePod()
	err := Session(context.Background(), f, f, cfg, true, func(ctx context.Context, p *Pod, s Started) error {
		if s.Hz != 1000000 {
			t.Fatalf("started = %+v", s)
		}
		_, err := p.ReadID(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"nrst", "spi_start", "id", "spi_stop", "nrst"}
	if got := f.ops(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ops %v, want %v", got, want)
	}
	if f.nrst || f.armed {
		t.Fatalf("left nrst=%v armed=%v", f.nrst, f.armed)
	}
}

func TestSessionCleanupOnError(t *testing.T) {
	f := newFakePod()
	f.failOn, f.failAt = "write", 2
	err := Session(context.Background(), f, f, cfg, true, func(ctx context.Context, p *Pod, _ Started) error {
		if err := p.Erase(ctx, 0, 4096, nil); err != nil {
			return err
		}
		return p.Write(ctx, 0, randBytes(2000), true, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "injected write failure") {
		t.Fatalf("got %v", err)
	}
	ops := f.ops()
	if tail := ops[len(ops)-2:]; !reflect.DeepEqual(tail, []string{"spi_stop", "nrst"}) {
		t.Fatalf("ops end %v", ops)
	}
	if f.nrst || f.armed {
		t.Fatalf("left nrst=%v armed=%v", f.nrst, f.armed)
	}
}

// A cancelled main context (Ctrl-C) must not stop cleanup, which runs through
// its own commander and deadline.
func TestSessionCleanupAfterCancel(t *testing.T) {
	f := newFakePod()
	ctx, cancel := context.WithCancel(context.Background())
	err := Session(ctx, f, f, cfg, true, func(ctx context.Context, p *Pod, _ Started) error {
		cancel()
		return p.Write(ctx, 0, randBytes(10), true, nil)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled", err)
	}
	if f.nrst || f.armed {
		t.Fatalf("left nrst=%v armed=%v", f.nrst, f.armed)
	}
}

func TestSessionStartFailureReleasesReset(t *testing.T) {
	f := newFakePod()
	f.busy = true
	err := Session(context.Background(), f, f, cfg, true, func(context.Context, *Pod, Started) error {
		t.Fatal("fn ran without a session")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "spi busy") {
		t.Fatalf("got %v", err)
	}
	// Busy means another session owns the engine: no spi_stop, but reset released.
	if got := f.ops(); !reflect.DeepEqual(got, []string{"nrst", "spi_start", "nrst"}) {
		t.Fatalf("ops %v", got)
	}
	if f.nrst {
		t.Fatal("reset left held")
	}
}

func TestSessionWithoutReset(t *testing.T) {
	f := newFakePod()
	if err := Session(context.Background(), f, f, cfg, false, func(context.Context, *Pod, Started) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := f.ops(); !reflect.DeepEqual(got, []string{"spi_start", "spi_stop"}) {
		t.Fatalf("ops %v", got)
	}
}

func TestHasCap(t *testing.T) {
	ok, err := HasCap(json.RawMessage(`{"fw":"3.2.0","caps":["swd","spi_master"]}`), Cap)
	if err != nil || !ok {
		t.Fatalf("HasCap = %v, %v", ok, err)
	}
	ok, _ = HasCap(json.RawMessage(`{"caps":["swd"]}`), Cap)
	if ok {
		t.Fatal("HasCap true without the cap")
	}
}
