// Package spiflash drives the pod's SPI master to read and program SPI NOR flash
// (25-series parts: W25Q, MX25, GD25, IS25, ...) wired to four LA pins.
//
// It speaks the firmware's JSON commands (see benchpod-firmware docs/API.md,
// "SPI master"): spi_start, spi_stop, spi_flash (id/read/erase/write/chip_erase)
// and nrst. Every request and reply fits one line, so any transport that can run
// a single JSON command works; the package only needs a Commander.
package spiflash

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Firmware limits per command.
const (
	MaxWrite = 768     // bytes of data in one write
	MaxRead  = 1024    // bytes in one read
	MaxErase = 1 << 20 // bytes in one erase
	// AddrSpace is what 3-byte addressing reaches: the first 16 MB.
	AddrSpace = 1 << 24
	// Cap is the status capability a pod with the SPI master reports.
	Cap = "spi_master"
)

// Per-command deadlines. A 1 MB erase of 4 KB sectors can take many seconds on a
// slow part; the rest answer in milliseconds.
const (
	cmdTimeout   = 30 * time.Second
	eraseTimeout = 3 * time.Minute
)

// Commander runs one JSON command and returns its "data".
type Commander interface {
	Command(ctx context.Context, req map[string]any) (json.RawMessage, error)
}

// Config is the spi_start request.
type Config struct {
	SCK, MOSI, MISO, CS int // LA pins 1..14, all different
	Hz                  int // 0 = pod default (1 MHz)
	Mode                int // 0 or 3
}

// Started is the spi_start reply: the pins and the rate actually used.
type Started struct {
	SCK  int `json:"sck"`
	MOSI int `json:"mosi"`
	MISO int `json:"miso"`
	CS   int `json:"cs"`
	Hz   int `json:"hz"`
	Mode int `json:"mode"`
}

// ID is the op "id" reply.
type ID struct {
	ID      string `json:"id"`
	Present bool   `json:"present"`
	Size    int    `json:"size"` // bytes, 0 when the part is unknown
	Status  int    `json:"status"`
}

// Progress reports done of total bytes.
type Progress func(done, total int)

// Pod issues SPI commands through C.
type Pod struct {
	C Commander
}

func (p *Pod) call(ctx context.Context, timeout time.Duration, req map[string]any, out any) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	data, err := p.C.Command(ctx, req)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %v reply: %w", req["cmd"], err)
	}
	return nil
}

// Start claims the four pins for SPI.
func (p *Pod) Start(ctx context.Context, c Config) (Started, error) {
	req := map[string]any{"cmd": "spi_start", "sck": c.SCK, "mosi": c.MOSI, "miso": c.MISO, "cs": c.CS, "mode": c.Mode}
	if c.Hz > 0 {
		req["hz"] = c.Hz
	}
	var s Started
	err := p.call(ctx, cmdTimeout, req, &s)
	return s, err
}

// Stop releases the pins.
func (p *Pod) Stop(ctx context.Context) error {
	return p.call(ctx, cmdTimeout, map[string]any{"cmd": "spi_stop"}, nil)
}

// Reset holds (assert) or releases the DUT's reset line.
func (p *Pod) Reset(ctx context.Context, assert bool) error {
	return p.call(ctx, cmdTimeout, map[string]any{"cmd": "nrst", "assert": assert}, nil)
}

// ReadID reads the JEDEC ID and status register.
func (p *Pod) ReadID(ctx context.Context) (ID, error) {
	var id ID
	err := p.call(ctx, cmdTimeout, map[string]any{"cmd": "spi_flash", "op": "id"}, &id)
	return id, err
}

// Read reads n bytes from addr in MaxRead pieces.
func (p *Pod) Read(ctx context.Context, addr, n int, prog Progress) ([]byte, error) {
	if err := checkRange(addr, n); err != nil {
		return nil, err
	}
	out := make([]byte, 0, n)
	for off := 0; off < n; off += MaxRead {
		l := min(MaxRead, n-off)
		var r struct {
			Addr int    `json:"addr"`
			Len  int    `json:"len"`
			Data string `json:"data"`
		}
		req := map[string]any{"cmd": "spi_flash", "op": "read", "addr": addr + off, "len": l}
		if err := p.call(ctx, cmdTimeout, req, &r); err != nil {
			return nil, fmt.Errorf("read at 0x%06x: %w", addr+off, err)
		}
		b, err := DecodeB64(r.Data)
		if err != nil {
			return nil, fmt.Errorf("read at 0x%06x: bad data: %w", addr+off, err)
		}
		if len(b) != l {
			return nil, fmt.Errorf("read at 0x%06x: got %d bytes, want %d", addr+off, len(b), l)
		}
		out = append(out, b...)
		if prog != nil {
			prog(len(out), n)
		}
	}
	return out, nil
}

// Erase erases every sector [addr, addr+n) touches, in MaxErase steps. The pod
// erases whole 4 KB sectors, so bytes around an unaligned range go too. Each
// step starts where the previous reply says erasing ended, so a sector is never
// erased twice.
func (p *Pod) Erase(ctx context.Context, addr, n int, prog Progress) error {
	if err := checkRange(addr, n); err != nil {
		return err
	}
	end := addr + n
	for at := addr; at < end; {
		l := min(MaxErase, end-at)
		var r struct {
			Addr int `json:"addr"`
			Len  int `json:"len"`
		}
		req := map[string]any{"cmd": "spi_flash", "op": "erase", "addr": at, "len": l}
		if err := p.call(ctx, eraseTimeout, req, &r); err != nil {
			return fmt.Errorf("erase at 0x%06x: %w", at, err)
		}
		next := at + l
		if e := r.Addr + r.Len; r.Len > 0 && e > next {
			next = e
		}
		at = next
		if prog != nil {
			prog(min(at, end)-addr, n)
		}
	}
	return nil
}

// ChipErase erases the whole part; it can take minutes, so only ctx bounds it.
// It returns the time the pod reported.
func (p *Pod) ChipErase(ctx context.Context) (time.Duration, error) {
	var r struct {
		MS int `json:"ms"`
	}
	if err := p.call(ctx, 0, map[string]any{"cmd": "spi_flash", "op": "chip_erase"}, &r); err != nil {
		return 0, err
	}
	return time.Duration(r.MS) * time.Millisecond, nil
}

// Write programs data at addr in MaxWrite chunks on an erased range. With verify
// the pod reads every chunk back and fails on the first mismatch.
func (p *Pod) Write(ctx context.Context, addr int, data []byte, verify bool, prog Progress) error {
	if err := checkRange(addr, len(data)); err != nil {
		return err
	}
	for off := 0; off < len(data); off += MaxWrite {
		chunk := data[off:min(off+MaxWrite, len(data))]
		var r struct {
			Len      int  `json:"len"`
			Verified bool `json:"verified"`
		}
		req := map[string]any{"cmd": "spi_flash", "op": "write", "addr": addr + off, "data": EncodeB64(chunk), "verify": verify}
		if err := p.call(ctx, cmdTimeout, req, &r); err != nil {
			return fmt.Errorf("write at 0x%06x: %w", addr+off, err)
		}
		if r.Len != len(chunk) {
			return fmt.Errorf("write at 0x%06x: pod wrote %d bytes, want %d", addr+off, r.Len, len(chunk))
		}
		if verify && !r.Verified {
			return fmt.Errorf("write at 0x%06x: pod did not verify the chunk", addr+off)
		}
		if prog != nil {
			prog(off+len(chunk), len(data))
		}
	}
	return nil
}

func checkRange(addr, n int) error {
	if addr < 0 || n <= 0 {
		return fmt.Errorf("invalid range: addr 0x%x, length %d", addr, n)
	}
	if addr+n > AddrSpace {
		return fmt.Errorf("range 0x%06x..0x%06x is past 16 MB, the most 3-byte addressing reaches", addr, addr+n)
	}
	return nil
}

// Session runs fn inside an SPI session: hold the DUT in reset (when nreset),
// spi_start, fn, then always spi_stop and release reset, in that order, so the
// pod's pins are high-Z before the DUT's own controller comes out of reset.
//
// Cleanup runs through cleanup (a Commander that does not share main's
// connection, which a cancel may have broken) with its own short deadline, so it
// still happens after Ctrl-C or a timeout. A cleanup failure is reported only
// when fn succeeded.
func Session(ctx context.Context, main, cleanup Commander, cfg Config, nreset bool,
	fn func(ctx context.Context, p *Pod, s Started) error) (err error) {
	p := &Pod{C: main}
	c := &Pod{C: cleanup}
	started, resetHeld := false, false
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var errs []error
		if started {
			if e := c.Stop(cctx); e != nil {
				errs = append(errs, fmt.Errorf("spi_stop: %w", e))
			}
		}
		if resetHeld {
			if e := c.Reset(cctx, false); e != nil {
				errs = append(errs, fmt.Errorf("release reset: %w", e))
			}
		}
		if err == nil && len(errs) > 0 {
			err = fmt.Errorf("cleanup: %w", errors.Join(errs...))
		}
	}()

	if nreset {
		// Mark it held before sending: if the reply is lost, releasing is still right.
		resetHeld = true
		if err := p.Reset(ctx, true); err != nil {
			return fmt.Errorf("hold reset: %w", err)
		}
	}
	started = true
	s, err := p.Start(ctx, cfg)
	if err != nil {
		if isSPIBusy(err) {
			// Someone else's session: do not stop it on the way out.
			started = false
		}
		return fmt.Errorf("spi_start: %w", err)
	}
	return fn(ctx, p, s)
}

func isSPIBusy(err error) bool {
	m := err.Error()
	return strings.Contains(m, "spi busy") || strings.Contains(m, "swd or spi busy")
}

// HasCap reports whether a status reply lists capability name.
func HasCap(status json.RawMessage, name string) (bool, error) {
	var st struct {
		Caps []string `json:"caps"`
	}
	if err := json.Unmarshal(status, &st); err != nil {
		return false, fmt.Errorf("decode status: %w", err)
	}
	for _, c := range st.Caps {
		if c == name {
			return true, nil
		}
	}
	return false, nil
}

// EncodeB64 is unpadded base64url (RFC 4648 section 5), what the firmware takes.
func EncodeB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DecodeB64 decodes base64url, padded or not.
func DecodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}
