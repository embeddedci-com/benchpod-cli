package tcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"
)

// mockServer accepts a single connection, reads one request line, and writes
// back the provided response lines. It reports the request it received.
func mockServer(t *testing.T, responses []string) (addr string, gotReq <-chan map[string]any) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	reqCh := make(chan map[string]any, 1)

	go func() {
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		scanner := bufio.NewScanner(conn)
		var req map[string]any
		if scanner.Scan() {
			_ = json.Unmarshal(scanner.Bytes(), &req)
		}
		reqCh <- req

		for _, line := range responses {
			if _, err := conn.Write([]byte(line + "\n")); err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String(), reqCh
}

// silentServer accepts one connection, reads the request, and then never
// replies, holding the connection open — modelling a firmware that accepted the
// TCP connection but isn't ready to answer yet (e.g. WiFi still coming up).
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read the request then sit on the connection without writing back.
		_, _ = bufio.NewReader(conn).ReadString('\n')
		<-make(chan struct{}) // block forever; t.Cleanup closes the listener
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// A command blocked waiting for a reply must return promptly when its context
// is cancelled (Ctrl+C), instead of blocking until the socket deadline.
func TestCommandCancelUnblocksRead(t *testing.T) {
	c := &Client{Addr: silentServer(t)}
	// Long deadline so only the cancellation — not the timeout — can end the read.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)

	done := make(chan error, 1)
	go func() {
		_, err := c.Command(ctx, map[string]any{"cmd": "ping"})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Command returned nil, want a cancellation error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Command err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Command did not return within 2s of context cancellation")
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCommandOK(t *testing.T) {
	addr, gotReq := mockServer(t, []string{`{"status":"ok","data":"pong"}`})
	c := &Client{Addr: addr}

	data, err := c.Command(testContext(t), map[string]any{"cmd": "ping"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	var pong string
	if err := json.Unmarshal(data, &pong); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if pong != "pong" {
		t.Fatalf("data = %q, want pong", pong)
	}

	req := <-gotReq
	if req["cmd"] != "ping" {
		t.Fatalf("server received cmd = %v, want ping", req["cmd"])
	}
}

func TestCommandError(t *testing.T) {
	addr, _ := mockServer(t, []string{`{"status":"error","message":"unknown cmd"}`})
	c := &Client{Addr: addr}

	_, err := c.Command(testContext(t), map[string]any{"cmd": "bogus"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "unknown cmd" {
		t.Fatalf("error = %q, want %q", err.Error(), "unknown cmd")
	}
}

func TestSamplesMultiPacket(t *testing.T) {
	addr, gotReq := mockServer(t, []string{
		`{"status":"ok","data":[1,2,3],"more":true}`,
		`{"status":"chunk","data":[4,5,6],"more":true}`,
		`{"status":"chunk","data":[7,8,9],"more":false}`,
	})
	c := &Client{Addr: addr}

	got, err := c.Samples(testContext(t), map[string]any{"cmd": "capture", "samples": 9})
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	req := <-gotReq
	if req["cmd"] != "capture" {
		t.Fatalf("server received cmd = %v, want capture", req["cmd"])
	}
}

func TestSamplesSinglePacket(t *testing.T) {
	addr, _ := mockServer(t, []string{`{"status":"ok","data":[10,20,30],"more":false}`})
	c := &Client{Addr: addr}

	got, err := c.Samples(testContext(t), map[string]any{"cmd": "capture"})
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	want := []int{10, 20, 30}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSamplesMidStreamError(t *testing.T) {
	addr, _ := mockServer(t, []string{
		`{"status":"ok","data":[1,2,3],"more":true}`,
		`{"status":"error","message":"capture failed"}`,
	})
	c := &Client{Addr: addr}

	_, err := c.Samples(testContext(t), map[string]any{"cmd": "capture", "samples": 9})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "capture failed" {
		t.Fatalf("error = %q, want %q", err.Error(), "capture failed")
	}
}

// A single reply line larger than bufio.Scanner's default 64 KB token cap must
// still be read in full (the old scanner-based reader failed with
// bufio.ErrTooLong here).
func TestSamplesLargeReplyLineExceeds64K(t *testing.T) {
	// 70 000 samples → a JSON array well over 64 KB on one line.
	const n = 70000
	b := make([]byte, 0, n*2+32)
	b = append(b, []byte(`{"status":"ok","more":false,"data":[`)...)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '1')
	}
	b = append(b, []byte(`]}`)...)
	if len(b) <= bufio.MaxScanTokenSize {
		t.Fatalf("test reply is %d bytes, not over the 64 KB scanner cap", len(b))
	}

	addr, _ := mockServer(t, []string{string(b)})
	c := &Client{Addr: addr}

	got, err := c.Samples(testContext(t), map[string]any{"cmd": "capture", "samples": n})
	if err != nil {
		t.Fatalf("Samples: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d samples, want %d", len(got), n)
	}
}

func TestEmptyAddr(t *testing.T) {
	c := &Client{Addr: ""}
	if _, err := c.Command(testContext(t), map[string]any{"cmd": "ping"}); err == nil {
		t.Fatal("expected error for empty addr")
	}
}

// freePort reserves a loopback port then releases it, returning an address that
// is (momentarily) not being listened on so dials get "connection refused".
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestDialRetrySucceedsWhenServerAppears(t *testing.T) {
	addr := freePort(t)

	// Bring the server up only after a delay, so the client must retry past the
	// initial "connection refused" before it can connect.
	const delay = 300 * time.Millisecond
	go func() {
		time.Sleep(delay)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		defer ln.Close()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		sc := bufio.NewScanner(conn)
		if sc.Scan() {
			_, _ = conn.Write([]byte(`{"status":"ok","data":"pong"}` + "\n"))
		}
	}()

	c := &Client{Addr: addr} // default 10s budget is ample
	start := time.Now()
	data, err := c.Command(testContext(t), map[string]any{"cmd": "ping"})
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if elapsed := time.Since(start); elapsed < delay {
		t.Fatalf("connected in %v, before the server was up (%v) — retry not exercised", elapsed, delay)
	}
	var pong string
	if err := json.Unmarshal(data, &pong); err != nil || pong != "pong" {
		t.Fatalf("data = %s (err %v), want pong", data, err)
	}
}

func TestDialRetryGivesUp(t *testing.T) {
	addr := freePort(t) // nothing listening → dials get refused

	start := time.Now()
	conn, err := dialWithRetry(context.Background(), "tcp", addr, 200*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected error connecting to a dead port")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("gave up after %v, expected to bail within the ~200ms budget", elapsed)
	}
}
