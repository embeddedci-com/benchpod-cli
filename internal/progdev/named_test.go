package progdev

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// fwFlags is what the firmware's prog_desc.c turns the named fields into (ctx->flags), written
// again here independently of the generator.
func fwFlags(proto string, f map[string]any) int {
	b := func(k string) bool { v, _ := f[k].(bool); return v }
	n := func(k string) int { v, _ := f[k].(int); return v }
	s := func(k string) string { v, _ := f[k].(string); return v }
	flags := 0
	switch proto {
	case "updi":
		flags = n("flash_base")
	case "pic":
		for bit, on := range map[int]bool{1: s("cmd_set") == "8bit", 2: s("pc") == "byte", 4: b("ee_erase"),
			8: b("cp_high"), 16: b("q10"), 32: b("id_split"), 64: b("key_flip")} {
			if on {
				flags |= bit
			}
		}
		flags |= (n("cp_bit") & 15) << 8
	case "tpi":
		if w := n("write_words"); w > 1 {
			flags |= w
		}
		if b("page_erase") {
			flags |= 8
		}
	case "c2":
		flags = n("fpdat")
		if b("no_setup") {
			flags |= 0x100
		}
	}
	return flags
}

// The packed fields every chip got from SDK 4.0.5 and CLI 0.10.6 (prog_chip_fields_golden.json,
// captured from the schema 1 table) must still be what the legacy path sends.
func TestLegacyFieldsAreUnchangedForEveryChip(t *testing.T) {
	data, err := os.ReadFile("testdata/prog_chip_fields_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]map[string]int
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, proto := range []string{"updi", "pic", "c2", "tpi", "pdi"} {
		for _, d := range families[proto].Devices {
			n++
			want := golden[proto+"/"+d.Name]
			got := map[string]int{}
			for k, v := range d.Fields(false) {
				got[k] = v.(int)
			}
			if !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
				t.Errorf("%s/%s: legacy fields %v, golden %v", proto, d.Name, got, want)
			}
		}
	}
	if n != len(golden) || n != 288 {
		t.Errorf("%d chips, golden has %d", n, len(golden))
	}
}

// The named fields mean the same flags to the firmware, and cp_addr keeps its value.
func TestNamedFieldsMeanTheLegacyFlagsForEveryChip(t *testing.T) {
	for proto, fam := range families {
		for _, d := range fam.Devices {
			named := d.Fields(true)
			if _, has := named["flags"]; has {
				t.Errorf("%s: named fields carry flags", d.Name)
			}
			if got, want := fwFlags(proto, named), d.Legacy["flags"]; got != want {
				t.Errorf("%s/%s: named fields %v mean flags %#x, legacy says %#x", proto, d.Name, named, got, want)
			}
			if named["cp_addr"] != nil && named["cp_addr"].(int) != d.Legacy["cp_addr"] {
				t.Errorf("%s: cp_addr %v vs %v", d.Name, named["cp_addr"], d.Legacy["cp_addr"])
			}
		}
	}
}

func TestPICEntryStyles(t *testing.T) {
	st := PICEntryStyles()
	var flags []int
	for _, s := range st {
		flags = append(flags, s.Flags)
		if fwFlags("pic", s.Prog) != s.Flags {
			t.Errorf("style %v: flags %d", s.Prog, s.Flags)
		}
	}
	if !reflect.DeepEqual(flags, []int{0, 1, 3, 19, 32}) || st[0].Prog["cmd_set"] != "6bit" {
		t.Fatalf("styles %v", st)
	}
	d, _ := FindDevice("pic", "d430", "")
	if !st[1].SameEntry(*d) || st[0].SameEntry(*d) {
		t.Errorf("PIC16F18446 is the 8-bit style: %v", d.EntryOf())
	}
	if f := st[0].Fields(false); len(f) != 0 {
		t.Errorf("plain style sends no flags, got %v", f)
	}
	if f := st[2].Fields(false); f["flags"] != 3 {
		t.Errorf("byte style legacy: %v", f)
	}
}
