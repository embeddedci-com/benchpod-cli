package tcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// linePod serves every connection: each request line gets handle's reply line. It records the
// commands per connection.
type linePod struct {
	mu    sync.Mutex
	conns [][]string
}

func startLinePod(t *testing.T, handle func(req map[string]any) string) (string, *linePod) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &linePod{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			idx := len(p.conns)
			p.conns = append(p.conns, nil)
			p.mu.Unlock()
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					var req map[string]any
					_ = json.Unmarshal(sc.Bytes(), &req)
					cmd, _ := req["cmd"].(string)
					p.mu.Lock()
					p.conns[idx] = append(p.conns[idx], cmd)
					p.mu.Unlock()
					if _, err := conn.Write([]byte(handle(req) + "\n")); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), p
}

func (p *linePod) seen() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]string, len(p.conns))
	for i, c := range p.conns {
		out[i] = append([]string(nil), c...)
	}
	return out
}

func TestLeaseTagOnEveryCommand(t *testing.T) {
	var mu sync.Mutex
	var got []map[string]any
	addr, _ := startLinePod(t, func(req map[string]any) string {
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		return `{"status":"ok","data":{}}`
	})
	SetLeaseJoin(&LeaseJoin{ID: "L-1", TTL: 300})
	t.Cleanup(func() { SetLeaseJoin(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &Client{Addr: addr, TLS: TLSOff}
	req := map[string]any{"cmd": "target_power", "efuse": 1}
	if _, err := c.Command(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, has := req["lease"]; has {
		t.Error("the caller's request was changed")
	}
	s, err := c.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Command(ctx, map[string]any{"cmd": "spi_xfer"}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := (&Client{Addr: addr, TLS: TLSOff, NoLeaseJoin: true}).Command(ctx, map[string]any{"cmd": "lease_renew", "id": "L-1"}); err != nil {
		t.Fatal(err)
	}
	SetLeaseJoin(nil)
	if _, err := c.Command(ctx, map[string]any{"cmd": "status"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 4 || got[0]["lease"] != "L-1" || got[0]["efuse"].(float64) != 1 || got[1]["lease"] != "L-1" {
		t.Fatalf("tagged %v", got)
	}
	if _, has := got[2]["lease"]; has {
		t.Errorf("NoLeaseJoin client tagged: %v", got[2])
	}
	if _, has := got[3]["lease"]; has {
		t.Errorf("tagged after the lease ended: %v", got[3])
	}
}

func TestChunks(t *testing.T) {
	addr, _ := mockServer(t, []string{
		`{"status":"ok","t_us":[0],"current_ua":[1],"more":true}`,
		`{"status":"chunk","stats":{"avg_ua":1200},"more":false}`,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := (&Client{Addr: addr, TLS: TLSOff}).Chunks(ctx, map[string]any{"cmd": "power_profile"})
	if err != nil || len(lines) != 2 {
		t.Fatalf("%d %v", len(lines), err)
	}
	var last struct {
		Stats map[string]float64 `json:"stats"`
	}
	json.Unmarshal(lines[1], &last)
	if last.Stats["avg_ua"] != 1200 {
		t.Errorf("%s", lines[1])
	}
	addr2, _ := mockServer(t, []string{`{"status":"error","message":"no power profile to stop"}`})
	if _, err := (&Client{Addr: addr2, TLS: TLSOff}).Chunks(ctx, map[string]any{"cmd": "power_profile"}); err == nil {
		t.Error("error line accepted")
	}
}

func TestStartRaw(t *testing.T) {
	SetLeaseJoin(&LeaseJoin{ID: "L-raw"})
	t.Cleanup(func() { SetLeaseJoin(nil) })
	rawReq := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		line, _ := r.ReadBytes('\n')
		rawReq <- string(line)
		conn.Write([]byte(`{"status":"ok","data":"uart ready"}` + "\n" + "APP_OK\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&Client{Addr: ln.Addr().String(), TLS: TLSOff}).StartRaw(ctx, map[string]any{"cmd": "uart_proxy_start", "rx": 5, "tx": 4})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 8)
	n, _ := conn.Read(buf)
	if string(buf[:n]) != "APP_OK\r\n" {
		t.Errorf("raw bytes %q", buf[:n])
	}
	if req := <-rawReq; !strings.Contains(req, `"lease":"L-raw"`) {
		t.Errorf("uart_proxy_start not tagged: %s", req)
	}
}
