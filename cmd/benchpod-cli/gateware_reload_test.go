package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// gwPod is a pod's network API as install-blobs uses it, with gateware: each slot holds an
// image of some version, the iCE40 runs one image kind, and fpga_image reflashes that kind from
// its slot (as the firmware does, also for the kind already running).
type gwPod struct {
	t         *testing.T
	mu        sync.Mutex
	running   int            // gateware version the iCE40 runs
	kind      int            // running image kind (0 loop, 1 deep, 2 emulation)
	embedded  int            // gateware_embedded
	slots     map[string]int // slot -> version of the image in it
	hookFixes bool           // the firmware's own post-install check reloads the running kind
	failImage string         // error reply to fpga_image with an image
	target    string
	version   int
	reqs      []string
}

func (p *gwPod) start() string {
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
					fmt.Fprintln(conn, p.handle(req))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

var kindSlot = []string{"gw0", "gw1", "gw2"}

func (p *gwPod) handle(req map[string]any) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd, _ := req["cmd"].(string)
	if img, ok := req["image"]; ok {
		cmd += fmt.Sprintf(":%v", img)
	}
	p.reqs = append(p.reqs, cmd)
	switch req["cmd"] {
	case "status":
		return okReply(map[string]any{"version": "3.10.3", "gateware": p.running, "gateware_embedded": p.embedded})
	case "blob_status":
		var blobs []any
		for _, n := range kindSlot {
			blobs = append(blobs, map[string]any{"name": n, "state": "ok", "present": true, "size": 4,
				"version": p.slots[n], "sha256": fmt.Sprintf("old-%d", p.slots[n])})
		}
		return okReply(map[string]any{"blobs": blobs})
	case "ota_begin":
		p.target, _ = req["target"].(string)
		v, _ := req["version"].(float64)
		p.version = int(v)
		return okReply(map[string]any{"state": "receiving"})
	case "ota_data":
		return okReply(map[string]any{"state": "receiving"})
	case "ota_end":
		return okReply(map[string]any{"state": "verified"})
	case "ota_commit":
		p.slots[p.target] = p.version
		if p.hookFixes && p.target == kindSlot[p.kind] && p.version == p.embedded {
			p.running = p.version
		}
		return okReply(map[string]any{"state": "installed"})
	case "fpga_image":
		img, ok := req["image"].(float64)
		if !ok {
			return okReply(map[string]any{"image": p.kind, "name": []string{"loop", "deep", "emulation"}[p.kind], "version": p.running})
		}
		if p.failImage != "" {
			return errReply(p.failImage)
		}
		p.kind, p.running = int(img), p.slots[kindSlot[int(img)]]
		return okReply(map[string]any{"image": p.kind, "name": []string{"loop", "deep", "emulation"}[p.kind], "version": p.running})
	}
	return errReply("unknown cmd")
}

func (p *gwPod) cmds() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.reqs, ",")
}

// gatewareBlobsDir writes a manifest with real gw0..gw2 blobs of one gateware version.
func gatewareBlobsDir(t *testing.T, version int) string {
	dir := t.TempDir()
	var entries []string
	for _, n := range kindSlot {
		data := []byte(n + "-v" + fmt.Sprint(version))
		sum := sha256.Sum256(data)
		file := "blob-" + n + ".bin"
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, fmt.Sprintf(`{"name":%q,"file":%q,"size":%d,"sha256":%q,"version":%d}`,
			n, file, len(data), hex.EncodeToString(sum[:]), version))
	}
	m := fmt.Sprintf(`{"format":1,"gateware_version":%d,"blobs":[%s]}`, version, strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(dir, blobsManifestName), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func installOnGWPod(t *testing.T, pod *gwPod, version int) string {
	t.Helper()
	addr := pod.start()
	ctx := context.Background()
	lan, err := openLAN(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer lan.s.Close()
	var ierr error
	out := captureStderr(t, func() {
		_, ierr = installBlobsList(ctx, lanBlobs{lan: lan, addr: addr}, blobSource{base: gatewareBlobsDir(t, version)}, false, nil)
	})
	if ierr != nil {
		t.Fatalf("install: %v\n%s", ierr, out)
	}
	return out
}

// The bench case: back from a v55 dev build to a release that carries v54, with the emulation
// image running (firmware 3.10.3 does not check after a gw2 install). The CLI reloads image 2.
func TestInstallBlobsReloadsStaleGateware(t *testing.T) {
	pod := &gwPod{t: t, running: 55, kind: 2, embedded: 54, slots: map[string]int{"gw0": 55, "gw1": 55, "gw2": 55}}
	out := installOnGWPod(t, pod, 54)
	if pod.running != 54 || pod.kind != 2 {
		t.Fatalf("pod runs v%d image %d, want v54 image 2\n%s", pod.running, pod.kind, out)
	}
	if !strings.Contains(pod.cmds(), "fpga_image,fpga_image:2,status") {
		t.Fatalf("commands %s", pod.cmds())
	}
	for _, want := range []string{"still runs gateware v55 and its firmware carries v54: reloading the emulation image",
		"now runs gateware v54"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, out)
		}
	}
}

// When the firmware's own check already reloaded the gateware, the CLI only looks.
func TestInstallBlobsLeavesMatchingGatewareAlone(t *testing.T) {
	pod := &gwPod{t: t, running: 55, kind: 0, embedded: 54, slots: map[string]int{"gw0": 55, "gw1": 55, "gw2": 55}, hookFixes: true}
	out := installOnGWPod(t, pod, 54)
	if strings.Contains(pod.cmds(), "fpga_image") || strings.Contains(out, "reloading") {
		t.Fatalf("reloaded matching gateware: %s\n%s", pod.cmds(), out)
	}
}

func TestInstallBlobsReportsAFailedReload(t *testing.T) {
	pod := &gwPod{t: t, running: 55, kind: 1, embedded: 54, slots: map[string]int{"gw0": 55, "gw1": 55, "gw2": 55}, failImage: "busy"}
	out := installOnGWPod(t, pod, 54)
	if !strings.Contains(out, "the reload failed (busy)") || !strings.Contains(out, "restart the pod (tap RESET) to load gateware v54") {
		t.Fatalf("stderr:\n%s", out)
	}
}

// reloadStaleGateware on its own: no reload without a mismatch, or with a version unknown.
func TestReloadStaleGatewareOnlyOnAMismatch(t *testing.T) {
	for _, st := range []map[string]any{
		{"gateware": 54, "gateware_embedded": 54},
		{"gateware": 54},
		{"gateware": 0, "gateware_embedded": 54},
		{"gateware": map[string]any{"version": 54}, "gateware_embedded": 54},
	} {
		var sent []string
		cmd := func(_ context.Context, req map[string]any, out any) error {
			sent = append(sent, req["cmd"].(string))
			b, _ := json.Marshal(st)
			return json.Unmarshal(b, out)
		}
		var w strings.Builder
		reloadStaleGateware(context.Background(), cmd, &w)
		if strings.Join(sent, ",") != "status" || w.Len() != 0 {
			t.Fatalf("status %v: sent %v, wrote %q", st, sent, w.String())
		}
	}
}

func TestInstalledGateware(t *testing.T) {
	if installedGateware([]blobEntry{{Name: "esp"}}) || !installedGateware([]blobEntry{{Name: "esp"}, {Name: "gw2"}}) {
		t.Fatal("installedGateware")
	}
}
