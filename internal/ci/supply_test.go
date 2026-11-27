package ci

import "testing"

func TestNormalizeSupply(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   string
		legacy bool
		ok     bool
	}{
		{"5v", "5v", false, true}, {"External", "external", false, true}, {"3v3", "3v3", false, true},
		{"1v8", "1v8", false, true}, {"1", "5v", true, true}, {"internal", "5v", true, true},
		{"2", "external", true, true}, {"3", "", false, false}, {"j1_3v3", "", false, false},
	} {
		got, legacy, ok := NormalizeSupply(tc.in)
		if got != tc.want || legacy != tc.legacy || ok != tc.ok {
			t.Errorf("%q: %q %v %v", tc.in, got, legacy, ok)
		}
	}
}

func TestFlashTargetPowerSupply(t *testing.T) {
	for in, want := range map[string]string{"5v": "5v", "2": "external", "3v3": "3v3"} {
		cfg, err := Parse([]byte("version: 1\nsuite: s\ndut: d\nfirmware:\n  file: fw.bin\n  flash: {target_power: "+in+"}\nsteps:\n  - power_on: {}\n"), "x.yml", nil)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := cfg.Firmware.Flash.TargetPower; got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
	if _, err := Parse([]byte("version: 1\nsuite: s\ndut: d\nfirmware:\n  file: fw.bin\n  flash: {target_power: 3}\nsteps:\n  - power_on: {}\n"), "x.yml", nil); err == nil {
		t.Error("target_power 3 accepted")
	}
}
