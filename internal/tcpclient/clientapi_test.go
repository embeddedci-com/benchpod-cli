package tcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/features"
)

// ClientAPI is the api of the features.json this CLI embeds: a table bump that raises api needs a
// CLI that handles it.
func TestClientAPIIsTheEmbeddedTablesAPI(t *testing.T) {
	if ClientAPI != features.API() {
		t.Fatalf("ClientAPI = %d, the embedded features.json has api %d", ClientAPI, features.API())
	}
}

func TestTagRequest(t *testing.T) {
	SetClientName("cli 1.2.3")
	defer SetClientName("cli dev")
	req := map[string]any{"cmd": "ping"}
	got := tagRequest(req, false)
	if got["client_api"] != ClientAPI || got["client"] != "cli 1.2.3" || got["cmd"] != "ping" {
		t.Fatalf("got %v", got)
	}
	if len(req) != 1 {
		t.Fatalf("req changed: %v", req)
	}
	// Fields the request has are kept.
	if got := tagRequest(map[string]any{"cmd": "x", "client_api": 0, "client": "me"}, false); got["client_api"] != 0 || got["client"] != "me" {
		t.Fatalf("got %v", got)
	}
	// The lease: added unless skipped; the client fields either way.
	SetLeaseJoin(&LeaseJoin{ID: "L-1"})
	defer SetLeaseJoin(nil)
	if got := tagRequest(map[string]any{"cmd": "x"}, false); got["lease"] != "L-1" || got["client_api"] != ClientAPI {
		t.Fatalf("got %v", got)
	}
	if got := TagRequest(map[string]any{"cmd": "x"}); got["lease"] != nil || got["client_api"] != ClientAPI {
		t.Fatalf("TagRequest: got %v", got)
	}
	SetClientFields(false)
	if got := tagRequest(map[string]any{"cmd": "x"}, true); len(got) != 1 {
		t.Fatalf("fields off: got %v", got)
	}
	SetClientFields(true)
}

// The firmware takes at most 32 printable ASCII characters.
func TestSetClientName(t *testing.T) {
	defer SetClientName("cli dev")
	SetClientName("cli 0.9.5-rc.1+build.abcdefghijklmnopqrstuvwxyz")
	if n := ClientName(); len(n) != 32 || !strings.HasPrefix(n, "cli 0.9.5") {
		t.Fatalf("%q", n)
	}
	SetClientName("cli é\n1")
	if n := ClientName(); n != "cli 1" {
		t.Fatalf("%q", n)
	}
}

// requestServer answers one reply per connection (several requests on one connection for a
// session) and hands each request it read to the test.
func requestServer(t *testing.T, reply string) (string, <-chan map[string]any) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan map[string]any, 8)
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
					_ = json.Unmarshal(line, &req)
					got <- req
					if _, err := conn.Write([]byte(reply + "\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String(), got
}

func wantClientFields(t *testing.T, what string, req map[string]any) {
	t.Helper()
	if req["client_api"] != float64(ClientAPI) || req["client"] != ClientName() || ClientName() == "" {
		t.Errorf("%s: request %v lacks client_api %d / client %q", what, req, ClientAPI, ClientName())
	}
}

// Every way this package sends a JSON command carries client_api and client.
func TestEveryRequestBuilderCarriesTheClientFields(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, got := requestServer(t, `{"status":"ok","data":{},"more":false}`)
	c := &Client{Addr: addr, TLS: TLSOff}

	if _, err := c.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
		t.Fatal(err)
	}
	wantClientFields(t, "Command", <-got)

	if _, err := c.Chunks(ctx, map[string]any{"cmd": "capture_read"}); err != nil {
		t.Fatal(err)
	}
	wantClientFields(t, "Chunks", <-got)

	s, err := c.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
		t.Fatal(err)
	}
	wantClientFields(t, "Session.Command", <-got)
	s.Close()

	conn, err := c.StartRaw(ctx, map[string]any{"cmd": "uart_proxy_start", "rx": 1, "tx": 2})
	if err != nil {
		t.Fatal(err)
	}
	wantClientFields(t, "StartRaw", <-got)
	conn.Close()
}

func TestCheckPodAPI(t *testing.T) {
	for _, c := range []struct {
		status string
		bad    bool
	}{
		{`{"version":"3.10.13"}`, false}, // no api: level 1
		{`{"version":"3.10.14","api":1,"min_client_api":0}`, false},
		{`{"version":"3.11.0","api":2,"min_client_api":0}`, true},
		{`{"version":"3.11.0","api":2,"min_client_api":2}`, true},
		{`not json`, false},
	} {
		err := CheckPodAPI(json.RawMessage(c.status))
		var upd *UpdateRequiredError
		if got := errors.As(err, &upd); got != c.bad {
			t.Errorf("%s: err = %v", c.status, err)
		}
	}
	err := CheckPodAPI(json.RawMessage(`{"version":"3.11.0","api":2}`))
	want := "this pod's firmware 3.11.0 needs a newer benchpod CLI (pod API 2, this CLI handles 1): " + UpdatePath
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Reading status checks the pod's API level, on a connection and in a session; other commands
// do not.
func TestStatusOfANewerPodStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, _ := requestServer(t, `{"status":"ok","data":{"version":"3.11.0","api":2,"min_client_api":0}}`)
	c := &Client{Addr: addr, TLS: TLSOff}
	var upd *UpdateRequiredError
	if _, err := c.Command(ctx, map[string]any{"cmd": "status", "caps": false}); !errors.As(err, &upd) || upd.PodAPI != 2 || upd.Firmware != "3.11.0" {
		t.Fatalf("status: err = %v", err)
	}
	if _, err := c.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
		t.Fatalf("ping: %v", err)
	}
	s, err := c.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Command(ctx, map[string]any{"cmd": "status"}); !errors.As(err, &upd) {
		t.Fatalf("session status: err = %v", err)
	}

	for _, ok := range []string{`{"status":"ok","data":{"version":"3.10.13"}}`, `{"status":"ok","data":{"api":1}}`} {
		addr, _ := requestServer(t, ok)
		if _, err := (&Client{Addr: addr, TLS: TLSOff}).Command(ctx, map[string]any{"cmd": "status"}); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
}

// BP0030 is the same error as the status check, not a *PodError.
func TestClientTooOldRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, _ := requestServer(t, `{"status":"error","code":"BP0030","args":{"client_api":1,"min_client_api":2,"api":2},`+
		`"message":"update the BenchPod SDK, MCP or CLI: this firmware needs client API 2 or newer (this client sent 1)"}`)
	_, err := (&Client{Addr: addr, TLS: TLSOff}).Command(ctx, map[string]any{"cmd": "la"})
	var upd *UpdateRequiredError
	if !errors.As(err, &upd) || upd.PodAPI != 2 || upd.MinClientAPI != 2 || upd.Refusal == nil || upd.Refusal.Code != "BP0030" {
		t.Fatalf("err = %#v", err)
	}
	if _, ok := AsPodError(err); ok {
		t.Fatal("BP0030 is not a *PodError")
	}
	want := "this pod's firmware needs a newer benchpod CLI (pod API 2, this CLI handles 1): " + UpdatePath
	if err.Error() != want {
		t.Fatalf("err = %q, want %q", err, want)
	}
}
