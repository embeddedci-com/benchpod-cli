package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Firmware before 3.9.0 has no USB network adapter: its console upload is gone, so a clear error
// that names flash-self.
func TestBlobTargetBefore39IsAnError(t *testing.T) {
	c := serialconsole.NewConsole(&textConsole{status: "  device : benchpod\r\n  ip     : 192.168.1.220\r\n"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := blobTargetFor(ctx, c, "/dev/cu.usbmodem1"); err == nil || !strings.Contains(err.Error(), "benchpod flash-self") {
		t.Fatalf("err = %v", err)
	}
}

// Firmware 3.9.0 with Wi-Fi configured and no network address answering: no console upload to fall
// back to, so a clear error.
func TestBlobTargetOn39WithoutANetworkIsAnError(t *testing.T) {
	oldPing := pingLANPod
	pingLANPod = func(string) error { return errors.New("refused") }
	t.Cleanup(func() { pingLANPod = oldPing })
	c := serialconsole.NewConsole(&textConsole{status: "  device : benchpod\r\n  ip     : 192.168.1.221\r\n" +
		"  usb    : net off (Wi-Fi configured) pod 10.237.8.85 host 10.237.8.86 link down rx 0 (drop 0) tx 0 (full 0)\r\n"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := blobTargetFor(ctx, c, "/dev/cu.usbmodem1"); err == nil || !strings.Contains(err.Error(), "usb-network on") {
		t.Fatalf("err = %v", err)
	}
}

// podKey is an identity public key whose short id is the given 6 hex digits.
func podKey(t *testing.T, short string) string {
	t.Helper()
	b, err := hex.DecodeString(short)
	if err != nil || len(b) != 3 {
		t.Fatalf("bad short id %q", short)
	}
	return base64.RawURLEncoding.EncodeToString(append(b, make([]byte, 29)...))
}

// lanBlobPod is a pod's network API as install-blobs uses it: identity, status, blob_status with
// one empty gw0 slot, and the ota_* upload. It records the targets it got blobs for.
type lanBlobPod struct {
	mu       sync.Mutex
	short    string
	received []string
}

func (p *lanBlobPod) start(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	key := podKey(t, p.short)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					_ = json.Unmarshal([]byte(line), &req)
					p.mu.Lock()
					var reply any
					switch req["cmd"] {
					case "identity_public":
						reply = map[string]any{"public": key}
					case "status":
						reply = map[string]any{"version": "4.0.5"}
					case "blob_status":
						reply = map[string]any{"blobs": []map[string]any{{"name": "gw0", "state": "empty", "present": false}}}
					case "ota_begin":
						p.received = append(p.received, fmt.Sprint(req["target"]))
						reply = map[string]any{"state": "receiving"}
					case "ota_data":
						reply = map[string]any{"state": "receiving"}
					case "ota_end":
						reply = map[string]any{"state": "verified"}
					case "ota_commit":
						reply = map[string]any{"state": "installed"}
					default:
						reply = map[string]any{}
					}
					p.mu.Unlock()
					fmt.Fprintln(conn, okReply(reply))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// blobDir writes a manifest with one gw0 blob and returns its directory.
func blobDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	data := []byte("gateware image")
	sum := sha256.Sum256(data)
	m := blobsManifest{Format: 1, Blobs: []blobEntry{{Name: "gw0", File: "blob-gw0.bin", Size: len(data),
		SHA256: hex.EncodeToString(sum[:]), Version: 68}}}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, blobsManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "blob-gw0.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// noUSB fails the test when install-blobs opens a USB console.
func noUSB(t *testing.T) {
	t.Helper()
	old := openBlobConsole
	openBlobConsole = func(*globalFlags, string) (blobTarget, string, context.Context, func(), error) {
		t.Error("install-blobs opened a USB console for a network connection")
		return nil, "", nil, nil, errors.New("no USB in this test")
	}
	t.Cleanup(func() { openBlobConsole = old })
}

func runInstallBlobs(t *testing.T, conn string, args ...string) error {
	t.Helper()
	g := &globalFlags{connection: conn, configFile: filepath.Join(t.TempDir(), "config.json"), noFirmwareReport: true}
	cmd := newInstallBlobsCmd(g)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	return cmd.Execute()
}

// The 2026-11-25 bug: --connection <address> with another pod on this computer's USB sent the blobs
// to the USB pod. A network address goes to that address only.
func TestInstallBlobsNetworkAddressGoesOverTheNetwork(t *testing.T) {
	noUSB(t)
	pod := &lanBlobPod{short: "c2f662"}
	addr := pod.start(t)
	if err := runInstallBlobs(t, addr, "--dir", blobDir(t)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pod.received, ","); got != "gw0" {
		t.Fatalf("blobs sent = %q, want gw0", got)
	}
}

// A pod name found on the LAN goes to its address, and the pod there must be that pod.
func TestInstallBlobsPodNameGoesOverTheNetwork(t *testing.T) {
	noUSB(t)
	pod := &lanBlobPod{short: "c2f662"}
	addr := pod.start(t)
	withPodNames(t, map[string]*namedPod{"c2f662": {addr: addr}})
	if err := runInstallBlobs(t, "benchpod-c2f662", "--dir", blobDir(t)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pod.received, ","); got != "gw0" {
		t.Fatalf("blobs sent = %q, want gw0", got)
	}
}

// A different pod answering where the named one was expected gets nothing.
func TestInstallBlobsRefusesAnotherPod(t *testing.T) {
	noUSB(t)
	pod := &lanBlobPod{short: "30a9f1"}
	addr := pod.start(t)
	withPodNames(t, map[string]*namedPod{"c2f662": {addr: addr}})
	err := runInstallBlobs(t, "benchpod-c2f662", "--dir", blobDir(t))
	if err == nil || !strings.Contains(err.Error(), "benchpod-c2f662") || !strings.Contains(err.Error(), "benchpod-30a9f1") {
		t.Fatalf("err = %v", err)
	}
	if len(pod.received) != 0 {
		t.Fatalf("blobs sent to the wrong pod: %v", pod.received)
	}
}

// usb:<name> reached through the console path: the pod that answers must be that pod too.
func TestInstallBlobsUSBRefusesAnotherPod(t *testing.T) {
	pod := &lanBlobPod{short: "30a9f1"}
	addr := pod.start(t)
	old := openBlobConsole
	openBlobConsole = func(*globalFlags, string) (blobTarget, string, context.Context, func(), error) {
		lan, err := openLAN(context.Background(), addr)
		if err != nil {
			return nil, "", nil, nil, err
		}
		return lanBlobs{lan: lan, addr: addr}, "/dev/cu.usbmodem1", context.Background(), func() { lan.s.Close() }, nil
	}
	t.Cleanup(func() { openBlobConsole = old })
	err := runInstallBlobs(t, "usb:benchpod-c2f662", "--dir", blobDir(t))
	if err == nil || !strings.Contains(err.Error(), "benchpod-30a9f1") {
		t.Fatalf("err = %v", err)
	}
	if len(pod.received) != 0 {
		t.Fatalf("blobs sent to the wrong pod: %v", pod.received)
	}
}

func TestBlobConnection(t *testing.T) {
	withPodNames(t, map[string]*namedPod{"c2f662": {addr: "192.168.1.164:8080"}})
	for _, tc := range []struct {
		conn, addr, pod, want string
		usb                   bool
	}{
		{conn: "192.168.1.164", addr: "192.168.1.164:8080"},
		{conn: "benchpod-c2f662", addr: "192.168.1.164:8080", want: "c2f662"},
		{conn: "usb:benchpod-30a9f1", usb: true, pod: "30a9f1", want: "30a9f1"},
		{conn: "benchpod-30a9f1", usb: true, pod: "30a9f1", want: "30a9f1"}, // not on the LAN: that pod on USB
		{conn: "usb", usb: true},
		{conn: "", usb: true},
	} {
		g := &globalFlags{connection: tc.conn, configFile: filepath.Join(t.TempDir(), "config.json")}
		c, err := g.blobConnection()
		if err != nil {
			t.Fatalf("%q: %v", tc.conn, err)
		}
		if c.spec.IsSerial() != tc.usb || c.spec.Addr != tc.addr || c.spec.Pod != tc.pod || c.want != tc.want {
			t.Errorf("%q: got %+v want=%q", tc.conn, c.spec, c.want)
		}
	}
	g := &globalFlags{connection: "embeddedci:benchpod-c2f662"}
	if _, err := g.blobConnection(); err == nil {
		t.Error("an explicit embeddedci: connection should be refused")
	}
}
