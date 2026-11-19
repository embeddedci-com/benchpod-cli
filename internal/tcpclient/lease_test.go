package tcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
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

func TestLeaseJoinOnEveryConnection(t *testing.T) {
	var gotID string
	addr, pod := startLinePod(t, func(req map[string]any) string {
		if req["cmd"] == "lease_acquire" {
			gotID, _ = req["id"].(string)
			return `{"status":"ok","data":{"ok":true,"lease":{"id":"L-1","left_s":300}}}`
		}
		return `{"status":"ok","data":{"version":"3.10.12"}}`
	})
	SetLeaseJoin(&LeaseJoin{ID: "L-1", Holder: "benchpod ci: me", Purpose: "suite", TTL: 300})
	t.Cleanup(func() { SetLeaseJoin(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &Client{Addr: addr, TLS: TLSOff}
	for i := 0; i < 2; i++ {
		data, err := c.Command(ctx, map[string]any{"cmd": "status"})
		if err != nil || string(data) != `{"version":"3.10.12"}` {
			t.Fatalf("command %d: %s %v", i, data, err)
		}
	}
	if gotID != "L-1" {
		t.Errorf("joined %q", gotID)
	}
	seen := pod.seen()
	if len(seen) != 2 || len(seen[0]) != 2 || seen[0][0] != "lease_acquire" || seen[0][1] != "status" {
		t.Errorf("connections %v", seen)
	}
	// The lease commands' own client does not join.
	own := &Client{Addr: addr, TLS: TLSOff, NoLeaseJoin: true}
	if _, err := own.Command(ctx, map[string]any{"cmd": "lease_renew"}); err != nil {
		t.Fatal(err)
	}
	if s := pod.seen(); len(s[2]) != 1 || s[2][0] != "lease_renew" {
		t.Errorf("own client %v", s[2])
	}
}

func TestLeaseJoinUnknownCommandIsRemembered(t *testing.T) {
	addr, pod := startLinePod(t, func(req map[string]any) string {
		if req["cmd"] == "lease_acquire" {
			return `{"status":"error","code":"BP0002","args":{"cmd":"lease_acquire"},"message":"unknown cmd"}`
		}
		return `{"status":"ok","data":{}}`
	})
	SetLeaseJoin(&LeaseJoin{ID: "L-2", TTL: 300})
	t.Cleanup(func() { SetLeaseJoin(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &Client{Addr: addr, TLS: TLSOff}
	for i := 0; i < 2; i++ {
		if _, err := c.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
			t.Fatal(err)
		}
	}
	seen := pod.seen()
	if len(seen[0]) != 2 || len(seen[1]) != 1 || seen[1][0] != "ping" {
		t.Errorf("connections %v", seen)
	}
}

func TestLeaseJoinBusyFailsTheCommand(t *testing.T) {
	addr, _ := startLinePod(t, func(req map[string]any) string {
		if req["cmd"] == "lease_acquire" {
			return `{"status":"error","code":"BP0050","message":"busy: web holds this pod (demo, 40 s left)","busy":true}`
		}
		return `{"status":"ok","data":{}}`
	})
	SetLeaseJoin(&LeaseJoin{ID: "L-3", TTL: 300})
	t.Cleanup(func() { SetLeaseJoin(nil) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := (&Client{Addr: addr, TLS: TLSOff}).Command(ctx, map[string]any{"cmd": "target_power"})
	pe, ok := AsPodError(err)
	if !ok || pe.Kind() != Busy {
		t.Fatalf("want a busy refusal, got %v", err)
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
		r.ReadBytes('\n')
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
}
