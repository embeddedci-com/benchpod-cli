package tcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// A coded error reply over the LAN is a *PodError of its code's kind, worded from the catalog.
func TestCodedRepliesArePodErrorsOfTheirKind(t *testing.T) {
	for code, want := range map[string]RefusalKind{"BP0040": Locked, "BP0045": Forbidden, "BP0025": Busy, "BP0049": Busy, "BP0006": Refused} {
		t.Run(code, func(t *testing.T) {
			reply, _ := json.Marshal(map[string]any{"status": "error", "code": code, "args": map[string]any{"cmd": "flash"}})
			addr, _ := mockServer(t, []string{string(reply)})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := (&Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "ping"})
			pe, ok := AsPodError(fmt.Errorf("wrapped: %w", err))
			if !ok {
				t.Fatalf("err = %#v, not a *PodError", err)
			}
			if pe.Cmd != "" || pe.Code != code || pe.Reason == "" || pe.Reason == "pod returned an error" {
				t.Fatalf("pe = %+v", pe)
			}
			if got := pe.Kind(); got != want {
				t.Fatalf("kind = %d, want %d", got, want)
			}
		})
	}
}

func TestPodErrorText(t *testing.T) {
	if got := (&PodError{Reason: "unknown cmd"}).Error(); got != "unknown cmd" {
		t.Fatalf("got %q", got)
	}
	if got := (&PodError{Cmd: "proxy-set", Reason: "w25q write failed"}).Error(); got != "proxy-set: w25q write failed" {
		t.Fatalf("got %q", got)
	}
}

func TestAFailedConnectIsUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = (&Client{Addr: addr, DialTimeout: 300 * time.Millisecond}).Command(context.Background(), map[string]any{"cmd": "ping"})
	if !errors.Is(err, ErrUnreachable) || !strings.HasPrefix(err.Error(), "connect to pod at "+addr+": ") {
		t.Fatalf("err = %v", err)
	}
	// Ctrl+C while connecting is not an unreachable pod.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = (&Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "ping"})
	if err == nil || errors.Is(err, ErrUnreachable) {
		t.Fatalf("canceled: err = %v", err)
	}
}
