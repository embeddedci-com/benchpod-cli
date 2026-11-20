package features

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const newPod = `{"version":"3.10.11","analog":true,"nrst_pin":true,"gateware":56,"sensors_digital":true,
"features_rev":1,"fpga_image":"loop","hw":["wifi","usb_cc","pod_current","adc_low_range"]}`

func TestHasFromTheTable(t *testing.T) {
	for _, tc := range []struct {
		status string
		name   string
		want   bool
	}{
		{newPod, "spi_master", true},
		{newPod, "generate16", true},
		{newPod, "sensors", true},
		{newPod, "uart_frame", true},
		{newPod, "dac_control_loop", true},
		{newPod, "dac_deep_replay", false}, // the loop image is running
		{newPod, "la_dir", true},           // no flag of its own
		{newPod, "no_such_feature", false},
		// gateware 44: no SPI master; a digital board: no analog features
		{`{"gateware":44,"features_rev":1}`, "spi_master", false},
		{`{"gateware":56,"features_rev":1,"analog":false}`, "generate16", false},
		{`{"gateware":56,"features_rev":1,"analog":false,"hw":[]}`, "wifi_scan", false},
		// no features_rev (firmware up to 3.10.10): CLI 0.10 reads no caps list
		{`{"caps":["sensors","spi_master"]}`, "spi_master", false},
	} {
		got, err := Has(json.RawMessage(tc.status), tc.name)
		if err != nil || got != tc.want {
			t.Errorf("Has(%s, %q) = %v, %v; want %v", tc.status, tc.name, got, err, tc.want)
		}
	}
	if _, err := Has(json.RawMessage(`not json`), "x"); err == nil {
		t.Error("bad JSON: no error")
	}
}

func TestStatusRequestAsksWithoutTheList(t *testing.T) {
	r := StatusRequest()
	if r["cmd"] != "status" || r["caps"] != false {
		t.Fatalf("StatusRequest() = %v", r)
	}
}

func TestPodIsNewer(t *testing.T) {
	if PodIsNewer(json.RawMessage(newPod)) {
		t.Error("revision 1 is not newer")
	}
	if !PodIsNewer(json.RawMessage(`{"features_rev":999}`)) {
		t.Error("revision 999 is newer")
	}
}

// The embedded copy is benchpod-firmware's features/features.json, byte for byte
// (scripts/sync-feature-map.sh), when that checkout is next to this repository.
func TestEmbeddedCopyMatchesTheFirmware(t *testing.T) {
	dir := os.Getenv("BENCHPOD_FIRMWARE_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "benchpod-firmware")
	}
	src, err := os.ReadFile(filepath.Join(dir, "features", "features.json"))
	if err != nil {
		t.Skipf("no firmware checkout: %v", err)
	}
	if !bytes.Equal(src, raw) {
		t.Fatal("internal/features/features.json differs from benchpod-firmware's: run scripts/sync-feature-map.sh")
	}
}
