package main

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/spiflash"
	"github.com/embeddedci-com/benchpod-cli/internal/spiflash/spiflashtest"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

func spiTestCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var testSPICfg = spiflash.Config{SCK: 3, MOSI: 4, MISO: 5, CS: 6, Hz: 1000000}

// The whole write flow over real TCP: status caps check, reset held, erase,
// chunked write with verify, then spi_stop and reset release.
func TestSPIWriteOverTCP(t *testing.T) {
	f := spiflashtest.NewFakePod()
	client := &tcpclient.Client{Addr: spiflashtest.Serve(t, f)}
	data := make([]byte, 5000)
	rand.New(rand.NewSource(1)).Read(data)

	var log bytes.Buffer
	err := runSPIWith(spiTestCtx(t), client, testSPICfg, true, "write", func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
		return spiWrite(ctx, p, 0x2000, data, true, true, &log)
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(f.Mem[0x2000:0x2000+len(data)], data) {
		t.Fatal("flash content differs")
	}
	want := []string{"status", "nrst", "spi_start", "id", "erase", "write", "write", "write", "write", "write", "write", "write", "spi_stop", "nrst"}
	if got := f.Ops(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ops\n got %v\nwant %v", got, want)
	}
	if f.NRST || f.Armed {
		t.Fatalf("left nrst=%v armed=%v", f.NRST, f.Armed)
	}
	if !strings.Contains(log.String(), "wrote and verified 4.9 KB at 0x002000") {
		t.Fatalf("summary missing: %q", log.String())
	}
}

func TestSPIWriteNoEraseNoVerify(t *testing.T) {
	f := spiflashtest.NewFakePod()
	client := &tcpclient.Client{Addr: spiflashtest.Serve(t, f)}
	err := runSPIWith(spiTestCtx(t), client, testSPICfg, false, "write", func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
		return spiWrite(ctx, p, 0, []byte{1, 2, 3}, false, false, io.Discard)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Reqs {
		if r["op"] == "erase" {
			t.Fatal("erased with --no-erase")
		}
		if r["op"] == "write" && r["verify"] != false {
			t.Fatalf("verify sent as %v with --no-verify", r["verify"])
		}
	}
}

// A failing chunk (not erased: verify fails) still ends the session.
func TestSPIWriteFailureCleansUp(t *testing.T) {
	f := spiflashtest.NewFakePod()
	client := &tcpclient.Client{Addr: spiflashtest.Serve(t, f)}
	err := runSPIWith(spiTestCtx(t), client, testSPICfg, true, "write", func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
		return spiWrite(ctx, p, 0, []byte{1, 2, 3}, false, true, io.Discard)
	})
	if err == nil || !strings.Contains(err.Error(), "verify failed at 0x000000") {
		t.Fatalf("got %v", err)
	}
	ops := f.Ops()
	if tail := ops[len(ops)-2:]; !reflect.DeepEqual(tail, []string{"spi_stop", "nrst"}) {
		t.Fatalf("ops %v", ops)
	}
	if f.NRST || f.Armed {
		t.Fatalf("left nrst=%v armed=%v", f.NRST, f.Armed)
	}
}

func TestSPIRefusesWithoutCap(t *testing.T) {
	f := spiflashtest.NewFakePod()
	f.Caps = []string{"swd"}
	client := &tcpclient.Client{Addr: spiflashtest.Serve(t, f)}
	err := runSPIWith(spiTestCtx(t), client, testSPICfg, true, "id", func(context.Context, *spiflash.Pod, spiflash.Started) error {
		t.Fatal("ran without the cap")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "no SPI master") {
		t.Fatalf("got %v", err)
	}
	if got := f.Ops(); !reflect.DeepEqual(got, []string{"status"}) {
		t.Fatalf("touched the pod beyond status: %v", got)
	}
}

func TestSPIWritePastPartEnd(t *testing.T) {
	f := spiflashtest.NewFakePod() // reports an 8 MB part
	client := &tcpclient.Client{Addr: spiflashtest.Serve(t, f)}
	err := runSPIWith(spiTestCtx(t), client, testSPICfg, false, "write", func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
		return spiWrite(ctx, p, 8<<20-10, make([]byte, 20), true, true, io.Discard)
	})
	if err == nil || !strings.Contains(err.Error(), "past the end") {
		t.Fatalf("got %v", err)
	}
	for _, op := range f.Ops() {
		if op == "erase" || op == "write" {
			t.Fatalf("touched flash: %v", f.Ops())
		}
	}
}

func TestSPIFlagsConfig(t *testing.T) {
	ok := spiFlags{sck: "la3", mosi: "4", miso: "5", cs: "LA6", hz: 2000000, mode: 3}
	c, err := ok.config()
	if err != nil || c != (spiflash.Config{SCK: 3, MOSI: 4, MISO: 5, CS: 6, Hz: 2000000, Mode: 3}) {
		t.Fatalf("config = %+v, %v", c, err)
	}
	for _, bad := range []spiFlags{
		{sck: "3", mosi: "4", miso: "5", hz: 1},           // cs missing
		{sck: "3", mosi: "4", miso: "4", cs: "6", hz: 1},  // duplicate
		{sck: "3", mosi: "4", miso: "5", cs: "15", hz: 1}, // out of range
		{sck: "3", mosi: "4", miso: "5", cs: "6", hz: 1, mode: 1},
		{sck: "3", mosi: "4", miso: "5", cs: "6", hz: 0},
	} {
		if _, err := bad.config(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "4096": 4096, "0x1000": 4096, "64K": 65536, "1M": 1 << 20, "0x10k": 16 << 10} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Fatalf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "abc", "4G"} {
		if _, err := parseSize(in); err == nil {
			t.Fatalf("parseSize(%q) accepted", in)
		}
	}
}

func TestProgressLine(t *testing.T) {
	got := progressLine("write", 1<<20, 4<<20, 10*time.Second)
	want := "write  25%  1 MB/4 MB  102.4 KB/s  ETA 30s"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := progressLine("erase", 4<<20, 4<<20, 2*time.Second); strings.Contains(got, "ETA") {
		t.Fatalf("finished line has an ETA: %q", got)
	}
}

// Ctrl-C mid-write: the main connection is abandoned, and cleanup still goes
// out over a fresh one.
func TestSPICancelCleansUp(t *testing.T) {
	f := spiflashtest.NewFakePod()
	client := &tcpclient.Client{Addr: spiflashtest.Serve(t, f)}
	ctx, cancel := context.WithCancel(spiTestCtx(t))
	err := runSPIWith(ctx, client, testSPICfg, true, "write", func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
		return p.Write(ctx, 0, make([]byte, 100*spiflash.MaxWrite), false, func(done, _ int) {
			if done >= 3*spiflash.MaxWrite {
				cancel()
			}
		})
	})
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("got %v, want canceled", err)
	}
	if f.NRST || f.Armed {
		t.Fatalf("left nrst=%v armed=%v", f.NRST, f.Armed)
	}
}
