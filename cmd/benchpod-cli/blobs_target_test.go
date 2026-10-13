package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// textConsole answers `status` with the given lines and a prompt, like the firmware's console.
type textConsole struct {
	status string
	out    bytes.Buffer
}

func (c *textConsole) Read(p []byte) (int, error) {
	if c.out.Len() == 0 {
		time.Sleep(5 * time.Millisecond)
		return 0, nil
	}
	return c.out.Read(p)
}

func (c *textConsole) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(bytes.ReplaceAll(p, []byte{0x08}, nil)))
	if line != "" {
		c.out.WriteString(line + "\r\n")
		if line == "status" {
			c.out.WriteString(c.status)
		}
		c.out.WriteString("> ")
	}
	return len(p), nil
}

func (c *textConsole) Close() error { return nil }

var _ io.ReadWriteCloser = (*textConsole)(nil)

// New CLI + firmware without a usable USB network: install-blobs keeps today's console path.
func TestBlobTargetFallsBackToTheConsole(t *testing.T) {
	cases := map[string]string{
		"firmware before 3.9.0": "  device : benchpod\r\n  ip     : 192.168.1.220\r\n",
		"Wi-Fi configured":      "  device : benchpod\r\n  usb    : net off (Wi-Fi configured) pod 10.237.8.85 host 10.237.8.86 link down rx 0 (drop 0) tx 0 (full 0)\r\n",
	}
	for name, status := range cases {
		c := serialconsole.NewConsole(&textConsole{status: status})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		target, closeT, err := blobTargetFor(ctx, c, "/dev/cu.usbmodem1")
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		closeT()
		if _, ok := target.(consoleBlobs); !ok || target.Via() != "/dev/cu.usbmodem1" {
			t.Fatalf("%s: target %T via %s, want the console", name, target, target.Via())
		}
	}
}
