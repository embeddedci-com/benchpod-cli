package main

import (
	"testing"

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
		{Name: "gw9", File: "blob-gw9.bin", Size: 5, SHA256: "dd"}, // a slot this pod does not have
	}}
	slots := []serialconsole.BlobSlot{
		{Name: "gw0", State: "ok", Size: 10, SHA256: "AA"},
		{Name: "gw1", State: "outdated", Size: 10, SHA256: "b0"},
		{Name: "esp", State: "missing"},
	}
	got := blobsToInstall(m, slots, false)
	if len(got) != 2 || got[0].Name != "gw1" || got[1].Name != "esp" {
		t.Fatalf("to install: %+v", got)
	}
	if all := blobsToInstall(m, slots, true); len(all) != 3 {
		t.Fatalf("force installs %d, want 3", len(all))
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
