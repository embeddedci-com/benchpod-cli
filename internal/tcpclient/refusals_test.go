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

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
)

// Every firmware refusal reaches the user word for word: the CLI's hints and retries match on
// these texts, and a user reads them.
func TestFirmwareRefusalsAreShownVerbatim(t *testing.T) {
	for _, r := range fwrefusals.All() {
		if r.Kind != "json" {
			continue
		}
		t.Run(r.ID, func(t *testing.T) {
			reply, _ := json.Marshal(map[string]string{"status": "error", "message": r.Example})
			addr, _ := mockServer(t, []string{string(reply)})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := (&Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "status"})
			if err == nil || err.Error() != r.Example {
				t.Fatalf("err = %v, want %q", err, r.Example)
			}
		})
	}
}

// Every error reply is a *PodError, classified by the firmware's own prefix.
func TestFirmwareRefusalsArePodErrorsOfTheirKind(t *testing.T) {
	want := map[string]RefusalKind{
		"lan_locked":           Locked,
		"tunnel_forbidden":     Forbidden,
		"lease_busy":           Busy,
		"heavy_busy":           Busy,
		"psram_bus_busy":       Busy,
		"ota_owner_busy":       Busy,
		"swd_busy":             Busy,
		"swd_busy_spi_session": Busy,
		"uart_busy":            Busy,
		"spi_busy":             Busy,
		"swd_or_spi_busy":      Busy,
	}
	for _, r := range fwrefusals.All() {
		if r.Kind != "json" {
			continue
		}
		t.Run(r.ID, func(t *testing.T) {
			reply, _ := json.Marshal(map[string]string{"status": "error", "message": r.Example})
			addr, _ := mockServer(t, []string{string(reply)})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := (&Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "status"})
			pe, ok := AsPodError(fmt.Errorf("wrapped: %w", err))
			if !ok {
				t.Fatalf("err = %#v, not a *PodError", err)
			}
			if pe.Cmd != "" || pe.Reason != r.Example {
				t.Fatalf("pe = %+v", pe)
			}
			if got := pe.Kind(); got != want[r.ID] {
				t.Fatalf("kind = %d, want %d", got, want[r.ID])
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
	for _, msg := range []string{"busybox missing", "unbusy: x", "ready: busy later", ""} {
		if k := ClassifyRefusal(msg); k != Refused {
			t.Errorf("%q: kind %d", msg, k)
		}
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
	if !errors.Is(err, ErrUnreachable) || !strings.HasPrefix(err.Error(), "connect to bench pod at "+addr+": ") {
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
