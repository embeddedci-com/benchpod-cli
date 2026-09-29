package spiflash_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/spiflash"
	"github.com/embeddedci-com/benchpod-cli/internal/spiflash/spiflashtest"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

var cfg = spiflash.Config{SCK: 3, MOSI: 4, MISO: 5, CS: 6, Hz: 2000000}

func TestB64URLUnpadded(t *testing.T) {
	// 0xfb 0xff encodes to "-_8" in base64url; std base64 would be "+/8=".
	if got := spiflash.EncodeB64([]byte{0xfb, 0xff}); got != "-_8" {
		t.Fatalf("EncodeB64 = %q, want -_8", got)
	}
	for _, in := range []string{"-_8", "-_8="} {
		b, err := spiflash.DecodeB64(in)
		if err != nil || !bytes.Equal(b, []byte{0xfb, 0xff}) {
			t.Fatalf("DecodeB64(%q) = %x, %v", in, b, err)
		}
	}
}

func TestWriteChunksAndVerify(t *testing.T) {
	f := spiflashtest.NewFakePod()
	p := &spiflash.Pod{C: f}
	ctx := context.Background()
	data := randBytes(3*spiflash.MaxWrite + 100)
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
	if !bytes.Equal(f.Mem[addr:addr+len(data)], data) {
		t.Fatal("flash content differs")
	}
	var writes []int
	var sizes []int
	for _, r := range f.Reqs {
		if r["op"] == "write" {
			writes = append(writes, int(r["addr"].(float64)))
			d, _ := spiflash.DecodeB64(r["data"].(string))
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
	f := spiflashtest.NewFakePod()
	p := &spiflash.Pod{C: f}
	ctx := context.Background()
	_, _ = p.Start(ctx, cfg)
	// Not erased: the fake flash can only clear bits, so verify fails.
	err := p.Write(ctx, 0, randBytes(10), true, nil)
	if err == nil || !strings.Contains(err.Error(), "verify failed") {
		t.Fatalf("got %v, want verify failure", err)
	}
}

func TestEraseStepsOneMBAndSkipsErased(t *testing.T) {
	f := spiflashtest.NewFakePod()
	p := &spiflash.Pod{C: f}
	ctx := context.Background()
	_, _ = p.Start(ctx, cfg)
	// Unaligned 2.5 MB range: the first step's reply ends past addr+1MB (sector
	// rounding), so the next step must start there.
	const addr = 0x800
	n := 2*spiflash.MaxErase + spiflash.MaxErase/2
	if err := p.Erase(ctx, addr, n, nil); err != nil {
		t.Fatal(err)
	}
	var got [][2]int
	for _, r := range f.Reqs {
		if r["op"] == "erase" {
			got = append(got, [2]int{int(r["addr"].(float64)), int(r["len"].(float64))})
		}
	}
	want := [][2]int{
		{0x800, spiflash.MaxErase},
		{0x101000, spiflash.MaxErase},
		{0x201000, addr + n - 0x201000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("erase steps %x, want %x", got, want)
	}
	for i := addr; i < addr+n; i++ {
		if f.Mem[i] != 0xFF {
			t.Fatalf("0x%x not erased", i)
		}
	}
}

func TestRangeLimits(t *testing.T) {
	p := &spiflash.Pod{C: spiflashtest.NewFakePod()}
	ctx := context.Background()
	if err := p.Erase(ctx, spiflash.AddrSpace-10, 20, nil); err == nil {
		t.Fatal("range past 16 MB accepted")
	}
	if _, err := p.Read(ctx, 0, 0, nil); err == nil {
		t.Fatal("zero-length read accepted")
	}
}

func TestSessionOrderAndCleanup(t *testing.T) {
	f := spiflashtest.NewFakePod()
	err := spiflash.Session(context.Background(), f, f, cfg, true, func(ctx context.Context, p *spiflash.Pod, s spiflash.Started) error {
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
	if got := f.Ops(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ops %v, want %v", got, want)
	}
	if f.NRST || f.Armed {
		t.Fatalf("left nrst=%v armed=%v", f.NRST, f.Armed)
	}
}

func TestSessionCleanupOnError(t *testing.T) {
	f := spiflashtest.NewFakePod()
	f.FailOn, f.FailAt = "write", 2
	err := spiflash.Session(context.Background(), f, f, cfg, true, func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
		if err := p.Erase(ctx, 0, 4096, nil); err != nil {
			return err
		}
		return p.Write(ctx, 0, randBytes(2000), true, nil)
	})
	if err == nil || !strings.Contains(err.Error(), "injected write failure") {
		t.Fatalf("got %v", err)
	}
	ops := f.Ops()
	if tail := ops[len(ops)-2:]; !reflect.DeepEqual(tail, []string{"spi_stop", "nrst"}) {
		t.Fatalf("ops end %v", ops)
	}
	if f.NRST || f.Armed {
		t.Fatalf("left nrst=%v armed=%v", f.NRST, f.Armed)
	}
}

// A cancelled main context (Ctrl-C) must not stop cleanup, which runs through
// its own commander and deadline.
func TestSessionCleanupAfterCancel(t *testing.T) {
	f := spiflashtest.NewFakePod()
	ctx, cancel := context.WithCancel(context.Background())
	err := spiflash.Session(ctx, f, f, cfg, true, func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
		cancel()
		return p.Write(ctx, 0, randBytes(10), true, nil)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want canceled", err)
	}
	if f.NRST || f.Armed {
		t.Fatalf("left nrst=%v armed=%v", f.NRST, f.Armed)
	}
}

func TestSessionStartFailureReleasesReset(t *testing.T) {
	f := spiflashtest.NewFakePod()
	f.Busy = true
	err := spiflash.Session(context.Background(), f, f, cfg, true, func(context.Context, *spiflash.Pod, spiflash.Started) error {
		t.Fatal("fn ran without a session")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "spi busy") {
		t.Fatalf("got %v", err)
	}
	// Busy means another session owns the engine: no spi_stop, but reset released.
	if got := f.Ops(); !reflect.DeepEqual(got, []string{"nrst", "spi_start", "nrst"}) {
		t.Fatalf("ops %v", got)
	}
	if f.NRST {
		t.Fatal("reset left held")
	}
}

func TestSessionWithoutReset(t *testing.T) {
	f := spiflashtest.NewFakePod()
	if err := spiflash.Session(context.Background(), f, f, cfg, false, func(context.Context, *spiflash.Pod, spiflash.Started) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := f.Ops(); !reflect.DeepEqual(got, []string{"spi_start", "spi_stop"}) {
		t.Fatalf("ops %v", got)
	}
}

func TestHasCap(t *testing.T) {
	ok, err := spiflash.HasCap(json.RawMessage(`{"fw":"3.2.0","caps":["swd","spi_master"]}`), spiflash.Cap)
	if err != nil || !ok {
		t.Fatalf("HasCap = %v, %v", ok, err)
	}
	ok, _ = spiflash.HasCap(json.RawMessage(`{"caps":["swd"]}`), spiflash.Cap)
	if ok {
		t.Fatal("HasCap true without the cap")
	}
}
