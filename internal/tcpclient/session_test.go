package tcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

// echoServer accepts connections and answers each request line with
// {"status":"ok","data":{"n":<index>,"cmd":<cmd>}}; a request with cmd "fail"
// gets an error reply and "hang" gets none. It counts accepted connections.
func echoServer(t *testing.T) (addr string, conns func() int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := make(chan struct{}, 64)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func(conn net.Conn) {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				n := 0
				for sc.Scan() {
					var req map[string]any
					_ = json.Unmarshal(sc.Bytes(), &req)
					switch req["cmd"] {
					case "fail":
						_, _ = conn.Write([]byte(`{"status":"error","message":"nope"}` + "\n"))
					case "hang":
					default:
						out, _ := json.Marshal(map[string]any{"status": "ok", "data": map[string]any{"n": n, "cmd": req["cmd"]}})
						_, _ = conn.Write(append(out, '\n'))
					}
					n++
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), func() int { return len(accepted) }
}

func TestSessionManyCommandsOneConnection(t *testing.T) {
	addr, conns := echoServer(t)
	s, err := (&Client{Addr: addr}).Open(testContext(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for i := 0; i < 5; i++ {
		data, err := s.Command(testContext(t), map[string]any{"cmd": "ping"})
		if err != nil {
			t.Fatalf("Command %d: %v", i, err)
		}
		var got struct{ N int }
		_ = json.Unmarshal(data, &got)
		if got.N != i {
			t.Fatalf("reply %d carried n=%d", i, got.N)
		}
	}
	if _, err := s.Command(testContext(t), map[string]any{"cmd": "fail"}); err == nil || err.Error() != "nope" {
		t.Fatalf("error reply: got %v, want nope", err)
	}
	// A firmware error is a normal reply: the session stays usable.
	if _, err := s.Command(testContext(t), map[string]any{"cmd": "ping"}); err != nil {
		t.Fatalf("after error reply: %v", err)
	}
	if n := conns(); n != 1 {
		t.Fatalf("connections = %d, want 1", n)
	}
}

func TestSessionBrokenAfterTimeout(t *testing.T) {
	addr, _ := echoServer(t)
	s, err := (&Client{Addr: addr}).Open(testContext(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.Command(ctx, map[string]any{"cmd": "hang"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang: got %v, want deadline exceeded", err)
	}
	if !s.Broken() {
		t.Fatal("Broken() = false after a lost reply")
	}
	if _, err := s.Command(testContext(t), map[string]any{"cmd": "ping"}); err == nil {
		t.Fatal("a session that lost a reply must refuse further commands")
	}
}
