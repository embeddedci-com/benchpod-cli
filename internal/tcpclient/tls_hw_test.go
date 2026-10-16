package tcpclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTLSResumesOnThePod needs a pod with LAN TLS (firmware 3.9.3+): BENCHPOD_TLS_HW_ADDR=<ip>.
// The first command does a full handshake; later ones resume with the pod's session ticket.
func TestTLSResumesOnThePod(t *testing.T) {
	addr := os.Getenv("BENCHPOD_TLS_HW_ADDR")
	if addr == "" {
		t.Skip("BENCHPOD_TLS_HW_ADDR not set")
	}
	PinStorePath = filepath.Join(t.TempDir(), "known_pods.json")
	defer func() { PinStorePath = "" }()
	c := &Client{Addr: addr + ":8080"}
	var times []time.Duration
	for i := 0; i < 4; i++ {
		t0 := time.Now()
		if _, err := c.Command(context.Background(), map[string]any{"cmd": "ping"}); err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(t0))
		if !c.LastDialTLS() {
			t.Fatal("not TLS")
		}
	}
	t.Logf("ping round trips: %v", times)
	if times[3] > 200*time.Millisecond {
		t.Errorf("no resumption: the 4th ping took %v", times[3])
	}
}
