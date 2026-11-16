package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// espPod answers an esp upload like firmware that keeps no copy of the C3's image: the commit
// answers "applying", ota_status keeps answering it for `polls` calls, then "installed" (or
// "error" with failWith). dropOnApply closes the connection the commit came in on, as a link over
// the pod's Wi-Fi does while the C3 is written.
type espPod struct {
	mu          sync.Mutex
	polls       int
	failWith    string
	dropOnApply bool
	oldFirmware bool // commit answers "installed" at once (the W25Q slot)
	applying    bool
	statusCalls int
	conns       int
}

func (p *espPod) start(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.conns++
			p.mu.Unlock()
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
					reply, drop := p.handle(req)
					fmt.Fprintln(conn, reply)
					if drop {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func (p *espPod) handle(req map[string]any) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch req["cmd"] {
	case "ota_begin", "ota_data":
		return okReply(map[string]any{"state": "receiving"}), false
	case "ota_end":
		return okReply(map[string]any{"state": "verified"}), false
	case "ota_commit":
		if p.oldFirmware {
			return okReply(map[string]any{"state": "installed"}), false
		}
		p.applying = true
		return okReply(map[string]any{"state": "applying"}), p.dropOnApply
	case "ota_status":
		p.statusCalls++
		if p.applying && p.statusCalls > p.polls {
			p.applying = false
			if p.failWith != "" {
				return okReply(map[string]any{"state": "error", "error": p.failWith}), false
			}
			return okReply(map[string]any{"state": "installed"}), false
		}
		if p.applying {
			return okReply(map[string]any{"state": "applying"}), false
		}
		return okReply(map[string]any{"state": "installed"}), false
	}
	return okReply(map[string]any{}), false
}

func fastEspWait(t *testing.T, wait time.Duration) {
	w, p, n := espApplyWait, espApplyPoll, espApplyNote
	espApplyWait, espApplyPoll, espApplyNote = wait, 5*time.Millisecond, time.Hour
	t.Cleanup(func() { espApplyWait, espApplyPoll, espApplyNote = w, p, n })
}

func uploadEsp(t *testing.T, addr string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lan, err := openLAN(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer lan.s.Close()
	_, err = lanBlobs{lan: lan, addr: addr}.UploadSigned(ctx, "esp", []byte("c3 image"), 0, nil, nil)
	return err
}

func TestEspUploadWaitsForTheC3Write(t *testing.T) {
	fastEspWait(t, 5*time.Second)
	pod := &espPod{polls: 3}
	if err := uploadEsp(t, pod.start(t)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if pod.statusCalls != 4 {
		t.Fatalf("ota_status asked %d times, want 4 (3 applying + installed)", pod.statusCalls)
	}
}

func TestEspUploadSurvivesTheWiFiDrop(t *testing.T) {
	fastEspWait(t, 5*time.Second)
	pod := &espPod{polls: 2, dropOnApply: true}
	if err := uploadEsp(t, pod.start(t)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if pod.conns < 2 {
		t.Fatalf("%d connection(s): the wait did not reconnect", pod.conns)
	}
}

func TestEspUploadReportsAFailedC3Write(t *testing.T) {
	fastEspWait(t, 5*time.Second)
	pod := &espPod{polls: 1, failWith: "ESP32-C3 flash failed: no SYNC"}
	err := uploadEsp(t, pod.start(t))
	if err == nil || !strings.Contains(err.Error(), "no SYNC") {
		t.Fatalf("err = %v, want the pod's reason", err)
	}
}

func TestEspUploadGivesUp(t *testing.T) {
	fastEspWait(t, 100*time.Millisecond)
	pod := &espPod{polls: 1 << 30}
	err := uploadEsp(t, pod.start(t))
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

func TestEspUploadOnOlderFirmwareDoesNotWait(t *testing.T) {
	fastEspWait(t, 5*time.Second)
	pod := &espPod{oldFirmware: true}
	if err := uploadEsp(t, pod.start(t)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if pod.statusCalls != 0 {
		t.Fatalf("ota_status asked %d times after an installed commit", pod.statusCalls)
	}
}

func TestEspIsInstalledLast(t *testing.T) {
	got := espLast([]blobEntry{{Name: "gw0"}, {Name: "esp"}, {Name: "gw2"}, {Name: "bridge"}})
	var names []string
	for _, e := range got {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "gw0,gw2,bridge,esp" {
		t.Fatalf("order %v", names)
	}
}
