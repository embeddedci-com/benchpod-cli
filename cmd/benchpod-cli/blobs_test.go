package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

func TestImageNeedsKB(t *testing.T) {
	img := make([]byte, 0x500)
	if got := imageNeedsKB(img); got != 2048 {
		t.Fatalf("an image without fw_info needs %d KB, want 2048 (built for the 2 MB part)", got)
	}
	copy(img[0x400:], []byte{'B', 'P', 'F', 'W', 2, 0, 0x00, 0x04})
	if got := imageNeedsKB(img); got != 1024 {
		t.Fatalf("fw_info min 1024 read as %d", got)
	}
	if got := imageNeedsKB(img[:0x404]); got != 2048 {
		t.Fatalf("a truncated image needs %d KB, want 2048", got)
	}
}

func TestDfuFlashKB(t *testing.T) {
	cases := map[string]int{
		`Found DFU: [0483:df11] ver=0200, devnum=9, cfg=1, intf=0, path="1-1", alt=0, name="@Internal Flash   /0x08000000/256*08Kg", serial="357D377F3133"`: 2048,
		`name="@Internal Flash   /0x08000000/128*08Kg", serial="X"`:                                                                                         1024,
		`name="@Internal Flash  /0x08000000/04*016Kg,01*064Kg,07*128Kg"`:                                                                                    1024,
		`no device here`: 0,
	}
	for in, want := range cases {
		if got := dfuFlashKB(in); got != want {
			t.Errorf("dfuFlashKB(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBlobsToInstall(t *testing.T) {
	m := blobsManifest{Blobs: []blobEntry{
		{Name: "gw0", File: "blob-gw0.bin", Size: 10, SHA256: "aa"},
		{Name: "gw1", File: "blob-gw1.bin", Size: 10, SHA256: "bb"},
		{Name: "esp", File: "blob-esp.bin", Size: 20, SHA256: "cc"},
		{Name: "gw2", File: "blob-gw2.bin", Size: 30, SHA256: "ee"}, // the emulation image (v54+)
		{Name: "gw9", File: "blob-gw9.bin", Size: 5, SHA256: "dd"},  // a slot this pod does not have
	}}
	slots := []serialconsole.BlobSlot{
		{Name: "gw0", State: "ok", Size: 10, SHA256: "AA"},
		{Name: "gw1", State: "outdated", Size: 10, SHA256: "b0"},
		{Name: "esp", State: "missing"},
		{Name: "gw2", State: "missing"},
	}
	got := blobsToInstall(m, slots, false)
	if len(got) != 3 || got[0].Name != "gw1" || got[1].Name != "esp" || got[2].Name != "gw2" {
		t.Fatalf("to install: %+v", got)
	}
	if all := blobsToInstall(m, slots, true); len(all) != 4 {
		t.Fatalf("force installs %d, want 4", len(all))
	}
}

func TestBlobSourceForFirmware(t *testing.T) {
	if s := blobSourceForFirmware("stm32h563/build/bench_pod_stm32.bin", "", ""); s.base != "stm32h563/build/blobs" {
		t.Fatalf("local build: %s", s.base)
	}
	if s := blobSourceForFirmware("https://example.com/rel/v1/bench_pod_stm32.bin", "", ""); s.base != "https://example.com/rel/v1" {
		t.Fatalf("url: %s", s.base)
	}
	if s := blobSourceForFirmware("", "", "v3.5.0"); s.base != "https://github.com/embeddedci-com/benchpod-firmware/releases/download/v3.5.0" {
		t.Fatalf("release: %s", s.base)
	}
	if s := blobSourceForFirmware("", "", ""); s.base != "https://github.com/embeddedci-com/benchpod-firmware/releases/latest/download" {
		t.Fatalf("latest: %s", s.base)
	}
}

// fakePod answers the pod commands updateBridge sends: wifi_status walks through statuses (the
// last one repeats), wifi_bridge update answers updateErr.
type fakePod struct {
	statuses  []string
	polls     int
	updateErr error
	sent      []string
}

func (f *fakePod) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	switch req["cmd"] {
	case "wifi_bridge":
		f.sent = append(f.sent, fmt.Sprintf("%v/%v", req["cmd"], req["action"]))
		return json.RawMessage(`{"update":"requested"}`), f.updateErr
	case "wifi_status":
		i := min(f.polls, len(f.statuses)-1)
		f.polls++
		return json.RawMessage(fmt.Sprintf(`{"via":"wifi-bridge","bridge":{"fw":"1.2.3","update":%q}}`, f.statuses[i])), nil
	}
	return nil, errors.New("unexpected command")
}

func fastBridgePoll(t *testing.T) {
	t.Helper()
	w, p := bridgeUpdateWait, bridgeUpdatePoll
	bridgeUpdateWait, bridgeUpdatePoll = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() { bridgeUpdateWait, bridgeUpdatePoll = w, p })
}

func TestUpdateBridgeSucceeds(t *testing.T) {
	fastBridgePoll(t)
	f := &fakePod{statuses: []string{"updated to 1.2.2", "updating 1.2.2 to 1.2.3: starting", "updated to 1.2.3"}}
	var out bytes.Buffer
	updateBridge(context.Background(), f, &out)
	if len(f.sent) != 1 || f.sent[0] != "wifi_bridge/update" {
		t.Fatalf("sent %v", f.sent)
	}
	if !strings.Contains(out.String(), "bridge: updated, the bridge reports firmware 1.2.3") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestUpdateBridgeNoBridge(t *testing.T) {
	f := &fakePod{statuses: []string{""}, updateErr: errors.New("wifi_bridge: no Wi-Fi bridge on the Ethernet port")}
	var out bytes.Buffer
	updateBridge(context.Background(), f, &out)
	if !strings.Contains(out.String(), "no Wi-Fi bridge on the pod's Ethernet port") || f.polls > 1 {
		t.Fatalf("output:\n%s (polls %d)", out.String(), f.polls)
	}
}

func TestUpdateBridgeFailureAndTimeout(t *testing.T) {
	fastBridgePoll(t)
	var out bytes.Buffer
	updateBridge(context.Background(), &fakePod{statuses: []string{"", "update to 1.2.3 failed: no answer"}}, &out)
	if !strings.Contains(out.String(), "the update did not finish") {
		t.Fatalf("failure output:\n%s", out.String())
	}
	out.Reset()
	updateBridge(context.Background(), &fakePod{statuses: []string{"updating 1.2.2 to 1.2.3: starting"}}, &out)
	if !strings.Contains(out.String(), "not finished after") {
		t.Fatalf("timeout output:\n%s", out.String())
	}
}

func TestBridgeUpdateDone(t *testing.T) {
	for s, want := range map[string]bool{
		"updated to 1.2.3": true, "update to 1.2.3 failed: x": true, "cannot update: no bridge": true,
		"update to 1.2.3 failed (will retry): x": false, "updating 1.2.2 to 1.2.3: starting": false, "": false,
	} {
		if got := bridgeUpdateDone(s); got != want {
			t.Errorf("bridgeUpdateDone(%q) = %v", s, got)
		}
	}
}
