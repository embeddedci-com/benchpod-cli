package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// imagePod fakes the pod commands ensureBusMasterImage uses.
type imagePod struct {
	image    string
	version  int
	features int
	sensors  string // the instances array of sensor_status
	switches []string
}

func (p *imagePod) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	switch req["cmd"] {
	case "fpga_image":
		if v, ok := req["image"].(string); ok {
			p.switches = append(p.switches, v)
			p.image = v
		}
		b, _ := json.Marshal(map[string]any{"name": p.image, "version": p.version, "features": p.features})
		return b, nil
	case "sensor_status":
		return json.RawMessage(`{"instances":` + p.sensors + `}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func TestEnsureBusMasterImage(t *testing.T) {
	running := `[{"instance":"env","kind":"i2c","running":true},{"instance":"t","kind":"analog","running":true}]`
	stopped := `[{"instance":"env","kind":"i2c","running":false},{"instance":"t","kind":"analog","running":true}]`
	cases := []struct {
		name    string
		pod     imagePod
		wantErr string
		wantSw  int
	}{
		{"emu with sensors", imagePod{image: "emulation", version: 65, features: 4, sensors: running}, "benchpod sensor stop", 0},
		{"emu, no sensors", imagePod{image: "emulation", version: 65, features: 4, sensors: stopped}, "", 1},
		{"emu named emu", imagePod{image: "emu", version: 65, features: 4, sensors: `[]`}, "", 1},
		{"loop", imagePod{image: "loop", version: 65, features: 0x29, sensors: running}, "", 0},
		{"deep", imagePod{image: "deep", version: 65, features: 0x2A, sensors: running}, "", 0},
		{"gateware 64 emu", imagePod{image: "emulation", version: 64, features: 0x24, sensors: running}, "", 0},
		{"emu with bus masters bit", imagePod{image: "emulation", version: 65, features: 0x24, sensors: running}, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.pod
			err := ensureBusMasterImage(context.Background(), &p, "SWD")
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if len(p.switches) != tc.wantSw || tc.wantSw == 1 && p.switches[0] != "loop" {
				t.Fatalf("switches = %v, want %d to loop", p.switches, tc.wantSw)
			}
		})
	}
}
