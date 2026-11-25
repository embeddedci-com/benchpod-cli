package progdev

import (
	"strings"
	"testing"
)

func TestTargets(t *testing.T) {
	for k, v := range map[string]string{"avr-updi": "updi", "UPDI": "updi", "pic": "pic", "efm8": "c2", "c2": "c2",
		"attiny-tpi": "tpi", "tpi": "tpi", "xmega": "pdi", "pdi": "pdi", "esp32": ""} {
		if Proto(k) != v {
			t.Errorf("Proto(%q) = %q, want %q", k, Proto(k), v)
		}
	}
	for _, p := range []string{"updi", "pic", "c2", "tpi", "pdi"} {
		if Fam(p) == nil || len(Fam(p).Devices) == 0 {
			t.Errorf("no devices for %s", p)
		}
	}
}

func TestFindDevice(t *testing.T) {
	d, err := FindDevice("updi", "1e9422", "")
	if err != nil || d.Name != "ATtiny1614" {
		t.Fatalf("%v %v", d, err)
	}
	if d, err = FindDevice("updi", "", "attiny-1614"); err != nil || d.Name != "ATtiny1614" {
		t.Fatalf("by name: %v %v", d, err)
	}
	if _, err = FindDevice("updi", "1e9422", "AVR64DA28"); err == nil {
		t.Fatal("a name with another ID must fail")
	}
	// the XMEGA alias shares the ID
	if d, err = FindDevice("pdi", "1e9441", "ATxmega16A4"); err != nil || d.Name != "ATxmega16A4U" {
		t.Fatalf("alias: %v %v", d, err)
	}
	// a PIC whose ID is longer than the table's still matches as a prefix
	if d, err = FindDevice("pic", "7030aabb", ""); err != nil || d.Name != "PIC16F18856" || d.Legacy["cp_addr"] != 65558 {
		t.Fatalf("pic: %v %v", d, err)
	}
}

func TestCheckVoltage(t *testing.T) {
	tpi, _ := FindDevice("tpi", "1e9003", "")
	for _, mv := range []int{1800, 3300} {
		if err := CheckVoltage(tpi, mv, "none"); err == nil || !strings.Contains(err.Error(), "level shifter") {
			t.Errorf("%d mV: %v", mv, err)
		}
	}
	if CheckVoltage(tpi, 0, "chip") != nil || CheckVoltage(tpi, 5000, "chip") != nil {
		t.Error("unknown voltage and 5 V are fine")
	}
	pic, _ := FindDevice("pic", "7030", "")
	if err := CheckVoltage(pic, 1800, "none"); err == nil {
		t.Error("PIC16F18856 needs 2.3 V")
	}
	if err := CheckVoltage(pic, 3300, "chip"); err != nil {
		t.Error(err)
	}
}

func TestSplitRegions(t *testing.T) {
	d, _ := FindDevice("updi", "1e9422", "")
	segs := []Segment{{0, []byte{1, 2, 3}}, {0x810000, []byte{9}}, {0x820003, []byte{7}}}
	r, _, err := SplitRegions(segs, d, Fam("updi").Ignore)
	if err != nil || len(r) != 3 || r[0].Mem != "flash" || len(r[0].Data) != 64 || r[0].Data[3] != 0xFF ||
		r[1].Mem != "eeprom" || r[1].Addr != 0 || r[2].Mem != "config" || r[2].Addr != 3 || len(r[2].Data) != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	// the lock bits an avr-gcc ELF carries are left out with a note
	r, notes, err := SplitRegions([]Segment{{0, []byte{1}}, {0x830000, []byte{0xFF}}}, d, Fam("updi").Ignore)
	if err != nil || len(r) != 1 || len(notes) != 1 {
		t.Fatalf("ignore: %v %v %v", err, r, notes)
	}
	if _, _, err = SplitRegions([]Segment{{0x900000, []byte{1}}}, d, nil); err == nil {
		t.Fatal("data outside every memory must fail")
	}
}

func TestParseIHex(t *testing.T) {
	s, err := ParseIHex([]byte(":0400100001020304E2\n:00000001FF\n"))
	if err != nil || len(s) != 1 || s[0].Addr != 0x10 || len(s[0].Data) != 4 {
		t.Fatalf("%v %+v", err, s)
	}
	if _, err = ParseIHex([]byte(":0400100001020304E3\n")); err == nil {
		t.Fatal("bad checksum")
	}
	if _, err = LoadImage("x.bin", nil, 0); err == nil {
		t.Fatal("empty file")
	}
	if s, err = LoadImage("x.bin", []byte{1, 2}, 0x100); err != nil || s[0].Addr != 0x100 {
		t.Fatalf("bin: %v %v", s, err)
	}
}
