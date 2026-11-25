package detect

import (
	"reflect"
	"testing"
)

func s(v string) *string { return &v }

func TestMapSWDSTM32F446(t *testing.T) {
	r := SWDReply{Port: "swd", Found: true, DPIDR: "0x2ba01477", APIDR: s("0x24770011"), CPUID: s("0x410fc241"),
		Reads: []SWDRead{{Addr: "0xe0042000", Value: s("0x10006421")}}}
	f := MapSWD(r, map[string]any{"swclk": 11, "swdio": 12})
	if f == nil || f.Chip != "STM32F446" || f.Vendor != "STMicroelectronics" || f.Core != "Cortex-M4" ||
		f.ID != "0x10006421" || f.Confidence != High || f.Method != "swd" {
		t.Fatalf("got %+v", f)
	}
}

func TestMapSWDTable(t *testing.T) {
	cases := []struct {
		name  string
		reply SWDReply
		chip  string
		conf  string
		core  string
	}{
		{"H7 at its own address", SWDReply{Found: true, DPIDR: "0x6ba02477", CPUID: s("0x411fc271"),
			Reads: []SWDRead{{Addr: "0xe0042000", Value: s("0x20000450")}, {Addr: "0x5c001000", Value: s("0x10000450")}}},
			"STM32H74x/H75x", High, "Cortex-M7"},
		{"nRF52840", SWDReply{Found: true, DPIDR: "0x2ba01477", CPUID: s("0x410fc241"),
			Reads: []SWDRead{{Addr: "0xe0042000", Value: nil}, {Addr: "0x10000100", Value: s("0x00052840")}}},
			"nRF52840", High, "Cortex-M4"},
		{"faulted reads: debug port only", SWDReply{Found: true, DPIDR: "0x2ba01477", CPUID: s("0x410fc241"),
			Reads: []SWDRead{{Addr: "0xe0042000", Value: nil}}}, "unknown", Low, "Cortex-M4"},
		{"ST part not in the table", SWDReply{Found: true, DPIDR: "0x2ba01477", CPUID: s("0x410fc241"),
			Reads: []SWDRead{{Addr: "0xe0042000", Value: s("0x10006fff")}}}, "unknown STMicroelectronics part 0xfff", Medium, "Cortex-M4"},
		{"TARGETID of an ST part (DPv2)", SWDReply{Found: true, DPIDR: "0x6ba02477", TargetID: s("0x10484041"), CPUID: s("0x410fd214")},
			"STM32H56x/H573", High, "Cortex-M33"},
		{"no CPUID", SWDReply{Found: true, DPIDR: "0x0bc11477"}, "unknown", Low, ""},
	}
	for _, c := range cases {
		f := MapSWD(c.reply, nil)
		if f == nil || f.Chip != c.chip || f.Confidence != c.conf || f.Core != c.core {
			t.Errorf("%s: got %+v, want %s %s %s", c.name, f, c.chip, c.conf, c.core)
		}
	}
	if MapSWD(SWDReply{Found: false}, nil) != nil {
		t.Error("nothing answered must map to no finding")
	}
}

func TestSWDReads(t *testing.T) {
	if got := SWDReads("Cortex-M4"); !reflect.DeepEqual(got, []string{"0xe0042000", "0x10000100"}) {
		t.Errorf("M4: %v", got)
	}
	if got := SWDReads("Cortex-M7"); !reflect.DeepEqual(got, []string{"0xe0042000", "0x5c001000"}) {
		t.Errorf("M7: %v", got)
	}
	if got := SWDReads(""); len(got) != 0 {
		t.Errorf("unknown core reads %v", got)
	}
	for _, c := range []string{"Cortex-M0", "Cortex-M33", "Cortex-M3"} {
		if len(SWDReads(c)) > MaxReads {
			t.Errorf("%s: more than %d reads", c, MaxReads)
		}
	}
}

func TestCore(t *testing.T) {
	for in, want := range map[string]string{"0x410fc241": "Cortex-M4", "0x411fc271": "Cortex-M7", "0x410cc601": "Cortex-M0+", "0x510fc241": "", "": ""} {
		if got := Core(in); got != want {
			t.Errorf("Core(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapJTAGChain(t *testing.T) {
	fs := MapJTAG(JTAGReply{Found: true, IDCodes: []string{"0x4ba00477", "0x06421041"}}, map[string]any{"tck": 11})
	if len(fs) != 2 {
		t.Fatalf("findings: %+v", fs)
	}
	if fs[0].Chip != "Arm JTAG-DP (Cortex-M debug port)" || fs[0].Vendor != "Arm" || fs[0].Confidence != Low || fs[0].ID != "0x4ba00477" {
		t.Errorf("DP: %+v", fs[0])
	}
	// the ST boundary-scan TAP: DEV_ID 0x421 in bits 12-23
	if fs[1].Chip != "STM32F446" || fs[1].Vendor != "STMicroelectronics" || fs[1].Confidence != High || fs[1].ID != "0x06421041" {
		t.Errorf("ST TAP: %+v", fs[1])
	}
}

func TestMapJTAGOther(t *testing.T) {
	fs := MapJTAG(JTAGReply{Found: true, IDCodes: []string{"0x00005c25", "0x1ba02477", "0x12345679"}}, nil)
	if fs[0].Chip != "ESP32-C3" || fs[0].Confidence != High {
		t.Errorf("ESP32-C3: %+v", fs[0])
	}
	if fs[1].Vendor != "Arm" || fs[1].Confidence != Low { // the version nibble does not matter
		t.Errorf("version nibble: %+v", fs[1])
	}
	if fs[2].Chip != "unknown" || fs[2].Confidence != Low {
		t.Errorf("unknown: %+v", fs[2])
	}
	if MapJTAG(JTAGReply{Found: false}, nil) != nil {
		t.Error("no chain, no findings")
	}
}

func TestUSBName(t *testing.T) {
	if USBName(0x16c0, 0x0478) != "Teensy (HalfKay bootloader)" || USBName(0x2e8a, 0x000f) != "RP2350 (BOOTSEL)" || USBName(1, 2) != "" {
		t.Error("USB table")
	}
	if USBID(0x0483, 0xdf11) != "0483:df11" {
		t.Error("USBID")
	}
}

func TestTableIsConsistent(t *testing.T) {
	for _, r := range ids.Registers {
		if _, ok := parseHex(r.Addr); !ok {
			t.Errorf("bad addr %q", r.Addr)
		}
		m, ok := parseHex(r.Mask)
		if !ok {
			t.Fatalf("bad mask %q", r.Mask)
		}
		for k := range r.Parts {
			v, ok := parseHex(k)
			if !ok || partKey(v, m) != k {
				t.Errorf("%s: part key %q is not written the way partKey writes it", r.Addr, k)
			}
		}
	}
}
