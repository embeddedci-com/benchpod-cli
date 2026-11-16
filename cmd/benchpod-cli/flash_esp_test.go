package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsESPTarget(t *testing.T) {
	for _, s := range []string{"esp32", "ESP32-S3", "esp32c3", "esp32-c6", " esp32h2 "} {
		if !isESPTarget(s) {
			t.Errorf("%q should be an ESP target", s)
		}
	}
	for _, s := range []string{"", "target/stm32f4x.cfg", "esp8266", "nrf52"} {
		if isESPTarget(s) {
			t.Errorf("%q should not be an ESP target", s)
		}
	}
}

func TestParseFlashSize(t *testing.T) {
	cases := map[string]int{"": 16 << 20, "8MB": 8 << 20, "4mb": 4 << 20, "2097152": 2 << 20}
	for in, want := range cases {
		got, err := parseFlashSize(in)
		if err != nil || got != want {
			t.Errorf("parseFlashSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := parseFlashSize("lots"); err == nil {
		t.Error("want an error for lots")
	}
}

func TestESPImages(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, n int) string {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	app := write("app.bin", 10)
	write("bootloader/bootloader.bin", 8)
	write("pt.bin", 3)
	args := filepath.Join(dir, "flasher_args.json")
	_ = os.WriteFile(args, []byte(`{"flash_files":{"0x1000":"bootloader/bootloader.bin","0x8000":"pt.bin","0x10000":"app.bin"},`+
		`"flash_settings":{"flash_size":"8MB"}}`), 0o644)

	r, size, err := espImages(args, "")
	if err != nil || size != "8MB" || len(r) != 3 || r[0].offset != 0x1000 || r[2].offset != 0x10000 {
		t.Fatalf("flasher_args: %v %q %+v", err, size, r)
	}
	r, _, err = espImages(app, "")
	if err != nil || len(r) != 1 || r[0].offset != 0 {
		t.Fatalf(".bin at 0: %v %+v", err, r)
	}
	r, _, err = espImages(app, "0x20000")
	if err != nil || len(r) != 1 || r[0].offset != 0x20000 {
		t.Fatalf(".bin at --load-address: %v %+v", err, r)
	}
	for _, bad := range []string{"0x100", "nope"} {
		if _, _, err := espImages(app, bad); err == nil {
			t.Errorf("want an error for load address %q", bad)
		}
	}
	if _, _, err := espImages("", ""); err == nil || !strings.Contains(err.Error(), "--file") {
		t.Errorf("want the needs --file error, got %v", err)
	}
}

func TestESPPins(t *testing.T) {
	rx, tx := 5, 4
	w := &podWiring{UartRx: &rx, UartTx: &tx}
	w.Signals = append(w.Signals, struct {
		Name string `json:"name"`
		LA   int    `json:"la"`
	}{"IO0", 6})
	f := &flashFlags{}
	pins, err := f.espPins(w)
	if err != nil || pins["rx"] != 5 || pins["tx"] != 4 || pins["boot"] != 6 || pins["en"] != "nrst" {
		t.Fatalf("from the profile: %v %v", pins, err)
	}
	w.Signals = append(w.Signals, struct {
		Name string `json:"name"`
		LA   int    `json:"la"`
	}{"EN", 8})
	if pins, _ = (&flashFlags{}).espPins(w); pins["en"] != 8 {
		t.Fatalf("EN from a profile signal: %v", pins)
	}
	if pins, _ = (&flashFlags{nreset: true, nresetSet: true}).espPins(w); pins["en"] != "nrst" {
		t.Fatalf("--nreset wins over the EN signal: %v", pins)
	}
	f = &flashFlags{espRx: "la1", espTx: "2", espBoot: "3"}
	pins, err = f.espPins(nil)
	if err != nil || pins["rx"] != 1 || pins["boot"] != 3 || pins["en"] != "nrst" {
		t.Fatalf("from flags: %v %v", pins, err)
	}
	if _, err := (&flashFlags{espRx: "1", espTx: "2", espBoot: "3", nresetSet: true}).espPins(nil); err == nil {
		t.Fatal("--nreset=false with no EN signal must be refused")
	}
	if _, err := (&flashFlags{espRx: "1", espTx: "2"}).espPins(nil); err == nil || !strings.Contains(err.Error(), "--boot") {
		t.Fatalf("want the --boot error, got %v", err)
	}
}
