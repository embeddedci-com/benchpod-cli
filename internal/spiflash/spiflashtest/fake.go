// Package spiflashtest is a fake pod for SPI flash tests: an in-memory NOR part
// behind the firmware's JSON commands, usable directly or over TCP.
package spiflashtest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/spiflash"
)

// FakePod models the firmware's SPI commands over an in-memory 16 MB NOR part:
// erase sets whole 4 KB sectors to 0xFF, write can only clear bits, and every
// request is logged.
type FakePod struct {
	mu     sync.Mutex
	Mem    []byte
	Armed  bool
	NRST   bool
	Log    []string         // cmd, or op for spi_flash
	Reqs   []map[string]any // every request as decoded JSON
	FailOn string           // cmd or op whose FailAt-th occurrence fails
	FailAt int
	seen   map[string]int
	Caps   []string
	Busy   bool // another client owns the SPI engine
}

// NewFakePod returns a pod with a 16 MB part full of non-erased bytes (0x5A)
// and the spi_master capability.
func NewFakePod() *FakePod {
	m := make([]byte, spiflash.AddrSpace)
	for i := range m {
		m[i] = 0x5A // not erased
	}
	return &FakePod{Mem: m, seen: map[string]int{}, Caps: []string{"swd", spiflash.Cap}}
}

func (f *FakePod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Round-trip through JSON like the wire does.
	raw, _ := json.Marshal(req)
	var r map[string]any
	_ = json.Unmarshal(raw, &r)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reqs = append(f.Reqs, r)
	name := r["cmd"].(string)
	if op, ok := r["op"].(string); ok {
		name = op
	}
	f.Log = append(f.Log, name)
	f.seen[name]++
	if f.FailOn == name && f.seen[name] == f.FailAt {
		return nil, fmt.Errorf("injected %s failure", name)
	}
	num := func(k string) int { v, _ := r[k].(float64); return int(v) }
	ok := func(v any) (json.RawMessage, error) { b, _ := json.Marshal(v); return b, nil }

	switch r["cmd"] {
	case "status":
		return ok(map[string]any{"caps": f.Caps})
	case "nrst":
		f.NRST = r["assert"].(bool)
		return ok(map[string]any{"asserted": f.NRST})
	case "spi_start":
		if f.Busy || f.Armed {
			return nil, errors.New("spi busy: send spi_stop first")
		}
		f.Armed = true
		return ok(map[string]any{"sck": num("sck"), "mosi": num("mosi"), "miso": num("miso"), "cs": num("cs"), "hz": 1000000, "mode": num("mode")})
	case "spi_stop":
		f.Armed = false
		return ok("spi stopped")
	case "spi_flash":
		if !f.Armed {
			return nil, errors.New("no SPI session: send spi_start first")
		}
		addr, n := num("addr"), num("len")
		switch r["op"] {
		case "id":
			return ok(map[string]any{"id": "ef4017", "present": true, "size": 8 << 20, "status": 0})
		case "read":
			if n < 1 || n > spiflash.MaxRead {
				return nil, errors.New("read needs addr and len 1..1024")
			}
			return ok(map[string]any{"addr": addr, "len": n, "data": spiflash.EncodeB64(f.Mem[addr : addr+n])})
		case "erase":
			if n < 1 || n > spiflash.MaxErase {
				return nil, errors.New("erase needs addr and len 1..1048576")
			}
			s := addr &^ 0xFFF
			e := (addr + n + 0xFFF) &^ 0xFFF
			for i := s; i < e; i++ {
				f.Mem[i] = 0xFF
			}
			return ok(map[string]any{"addr": s, "len": e - s, "ms": 1})
		case "chip_erase":
			for i := range f.Mem {
				f.Mem[i] = 0xFF
			}
			return ok(map[string]any{"ms": 5})
		case "write":
			s, _ := r["data"].(string)
			if strings.ContainsAny(s, "+/=") {
				return nil, errors.New("data must be 1..768 bytes of base64url")
			}
			d, err := spiflash.DecodeB64(s)
			if err != nil || len(d) == 0 || len(d) > spiflash.MaxWrite {
				return nil, errors.New("data must be 1..768 bytes of base64url")
			}
			for i, b := range d {
				f.Mem[addr+i] &= b
			}
			verify := true
			if v, ok := r["verify"].(bool); ok {
				verify = v
			}
			if verify && !bytes.Equal(f.Mem[addr:addr+len(d)], d) {
				return nil, fmt.Errorf("verify failed at 0x%06x: not erased, write-protected, or a bad wire", addr)
			}
			return ok(map[string]any{"addr": addr, "len": len(d), "verified": verify, "ms": 1})
		}
	}
	return nil, errors.New("unknown cmd")
}

// Ops returns the command log.
func (f *FakePod) Ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Log...)
}

// Serve answers the firmware's line protocol for f on a local TCP port, any
// number of connections and commands per connection, and returns host:port.
func Serve(t *testing.T, f *FakePod) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					var req map[string]any
					if err := json.Unmarshal(line, &req); err != nil {
						return
					}
					var reply map[string]any
					data, err := f.Command(context.Background(), req)
					if err != nil {
						reply = map[string]any{"status": "error", "message": err.Error()}
					} else {
						reply = map[string]any{"status": "ok", "data": data}
					}
					out, _ := json.Marshal(reply)
					if _, err := conn.Write(append(out, '\n')); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}
