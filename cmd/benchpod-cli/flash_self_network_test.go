package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fwNetPod is a pod's network API as flash-self --network uses it: status, identity, ota_* for
// the firmware (no target), and a restart on commit that brings it back with the new version.
type fwNetPod struct {
	t       *testing.T
	mu      sync.Mutex
	version string
	flashKB int
	refuse  string // error reply to ota_begin
	corrupt bool   // ota_end reports a hash mismatch
	staged  []byte
	sha     string
	state   string
	reqs    []string
	written []byte
}

func (p *fwNetPod) start() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { ln.Close() })
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
					reply, restart := p.handle(req)
					fmt.Fprintln(conn, reply)
					if restart {
						return // the pod resets: the connection drops
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func (p *fwNetPod) handle(req map[string]any) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd, _ := req["cmd"].(string)
	p.reqs = append(p.reqs, cmd)
	switch cmd {
	case "status":
		return okReply(map[string]any{"version": p.version, "flash_kb": p.flashKB, "gateware": 48}), false
	case "identity_public":
		return okReply(map[string]any{"public": "pubkey-1"}), false
	case "blob_status":
		return errReply("unknown cmd"), false
	case "ota_begin":
		if p.refuse != "" {
			return errReply(p.refuse), false
		}
		if _, ok := req["target"]; ok {
			return errReply("unknown target"), false
		}
		p.staged = make([]byte, int(req["size"].(float64)))
		p.sha, p.state = req["sha256"].(string), "receiving"
	case "ota_data":
		raw, _ := base64.RawURLEncoding.DecodeString(req["data"].(string))
		copy(p.staged[int(req["offset"].(float64)):], raw)
	case "ota_end":
		sum := sha256.Sum256(p.staged)
		p.state = "verified"
		if p.corrupt || hex.EncodeToString(sum[:]) != p.sha {
			p.state, p.staged = "error", nil
			return okReply(map[string]any{"state": "error", "error": "sha256 mismatch"}), false
		}
	case "ota_commit":
		if p.state != "verified" {
			return errReply("no verified image staged"), false
		}
		p.written, p.version = p.staged, "3.9.1"
		return okReply(map[string]any{"committing": true}), true
	case "ota_abort":
		p.state = "idle"
	default:
		return errReply("unknown cmd"), false
	}
	return okReply(map[string]any{"state": p.state}), false
}

func (p *fwNetPod) cmds() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.reqs, ",")
}

// fwImage writes a firmware image with a fw_info block asking for needKB of flash.
func fwImage(t *testing.T, size, needKB int) (string, []byte) {
	img := make([]byte, size)
	for i := range img {
		img[i] = byte(i * 7)
	}
	copy(img[fwInfoOffset:], "BPFW")
	img[fwInfoOffset+6], img[fwInfoOffset+7] = byte(needKB), byte(needKB>>8)
	path := filepath.Join(t.TempDir(), "bench_pod_stm32.bin")
	if err := os.WriteFile(path, img, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, img
}

func fastNetworkFlash(t *testing.T) {
	w, r, s := networkFlashReturnWait, networkFlashReturnRetry, networkFlashSettle
	networkFlashReturnWait, networkFlashReturnRetry, networkFlashSettle = 3*time.Second, 50*time.Millisecond, 0
	t.Cleanup(func() { networkFlashReturnWait, networkFlashReturnRetry, networkFlashSettle = w, r, s })
}

func noFlagChanged(string) bool { return false }

func TestFlashSelfNetworkUpdates(t *testing.T) {
	fastNetworkFlash(t)
	pod := &fwNetPod{t: t, version: "3.9.0", flashKB: 2048}
	addr := pod.start()
	file, img := fwImage(t, 3*lanBlobChunk+100, 1024)
	g := &globalFlags{connection: addr, noFirmwareReport: true}
	if err := runFlashSelfNetwork(g, &flashSelfFlags{network: true}, noFlagChanged, []string{file}); err != nil {
		t.Fatalf("flash-self --network: %v", err)
	}
	if string(pod.written) != string(img) {
		t.Fatalf("the pod did not get the image")
	}
	want := "ota_begin,ota_data,ota_data,ota_data,ota_data,ota_end,ota_commit"
	if got := pod.cmds(); !strings.Contains(got, want) {
		t.Fatalf("requests = %s, want them to contain %s", got, want)
	}
	if !strings.HasSuffix(pod.cmds(), "blob_status") {
		t.Fatalf("after the restart it should read the blob slots, got %s", pod.cmds())
	}
}

func TestFlashSelfNetworkVerifyFailureWritesNothing(t *testing.T) {
	fastNetworkFlash(t)
	pod := &fwNetPod{t: t, version: "3.9.0", flashKB: 2048, corrupt: true}
	addr := pod.start()
	file, _ := fwImage(t, 2000, 1024)
	err := runFlashSelfNetwork(&globalFlags{connection: addr, noFirmwareReport: true}, &flashSelfFlags{network: true}, noFlagChanged, []string{file})
	if err == nil || !strings.Contains(err.Error(), "nothing was written") || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if pod.written != nil || strings.Contains(pod.cmds(), "ota_commit") {
		t.Fatalf("committed after a failed verify: %s", pod.cmds())
	}
	if !strings.HasSuffix(pod.cmds(), "ota_abort") {
		t.Fatalf("a failed verify should abort the session, got %s", pod.cmds())
	}
}

func TestFlashSelfNetworkPodRefusal(t *testing.T) {
	fastNetworkFlash(t)
	pod := &fwNetPod{t: t, version: "3.9.0", flashKB: 2048, refuse: "locked: ota_begin needs the cloud or USB"}
	addr := pod.start()
	file, _ := fwImage(t, 2000, 1024)
	err := runFlashSelfNetwork(&globalFlags{connection: addr, noFirmwareReport: true}, &flashSelfFlags{network: true}, noFlagChanged, []string{file})
	if err == nil || !strings.Contains(err.Error(), "locked: ota_begin needs the cloud or USB") {
		t.Fatalf("err = %v", err)
	}
}

func TestFlashSelfNetworkImageTooBig(t *testing.T) {
	pod := &fwNetPod{t: t, version: "3.9.0", flashKB: 1024}
	addr := pod.start()
	file, _ := fwImage(t, 2000, 2048)
	err := runFlashSelfNetwork(&globalFlags{connection: addr, noFirmwareReport: true}, &flashSelfFlags{network: true}, noFlagChanged, []string{file})
	if err == nil || !strings.Contains(err.Error(), "needs 2048 KB of flash and the pod has 1024 KB") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(pod.cmds(), "ota_begin") {
		t.Fatalf("sent an image that does not fit: %s", pod.cmds())
	}
}

func TestFlashSelfNetworkRefusesCloudAndDFUFlags(t *testing.T) {
	err := runFlashSelfNetwork(&globalFlags{connection: "embeddedci:bench-1", noFirmwareReport: true},
		&flashSelfFlags{network: true}, noFlagChanged, nil)
	if err == nil || !strings.Contains(err.Error(), "update a cloud pod from the web app") {
		t.Fatalf("cloud err = %v", err)
	}
	err = runFlashSelfNetwork(&globalFlags{connection: "127.0.0.1:1"}, &flashSelfFlags{network: true},
		func(name string) bool { return name == "enter-dfu" }, nil)
	if err == nil || !strings.Contains(err.Error(), "--enter-dfu is for USB DFU") {
		t.Fatalf("enter-dfu err = %v", err)
	}
}
