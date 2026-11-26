package currentmonitor

import (
	"math"
	"strings"
	"testing"
)

// ina is a register file: reads continue into the next register, writes take one 16-bit value.
type ina struct {
	widths map[int]int
	regs   map[int]int
	ptr    int
	writes [][2]int
}

func (d *ina) xfer(addr int, w []byte, n int) ([]byte, error) {
	if len(w) > 0 {
		d.ptr = int(w[0])
	}
	if len(w) == 3 {
		d.regs[d.ptr] = int(w[1])<<8 | int(w[2])
		d.writes = append(d.writes, [2]int{d.ptr, d.regs[d.ptr]})
	}
	var out []byte
	for len(out) < n {
		wd := d.widths[d.ptr]
		if wd == 0 {
			wd = 2
		}
		v := d.regs[d.ptr]
		for i := wd - 1; i >= 0; i-- {
			out = append(out, byte(v>>(8*i)))
		}
		d.ptr++
	}
	return out[:n], nil
}

func u16(v int) int { return v & 0xFFFF }

func near(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > math.Abs(want)*1e-9+1e-15 {
		t.Fatalf("%s = %g, want %g", what, got, want)
	}
}

func mon(t *testing.T, name string, d *ina, shunt float64, ch int) *Monitor {
	t.Helper()
	c, err := Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return New(c, 0x40, ch, shunt, d.xfer)
}

func TestChipTable(t *testing.T) {
	if _, err := Lookup("ina999"); err == nil || !strings.Contains(err.Error(), "known: ina226") {
		t.Fatalf("%v", err)
	}
	for _, c := range Chips() {
		for _, v := range c.Voltages {
			if v < 3 {
				t.Fatalf("%s runs at %g V?", c.Name, v)
			}
		}
	}
}

func TestINA226(t *testing.T) {
	d := &ina{regs: map[int]int{0xFE: 0x5449, 0xFF: 0x2260, 1: 4, 2: 2640}}
	m := mon(t, "ina226", d, 10, 1)
	if err := m.Probe(); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(true, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if d.writes[0] != [2]int{0, 0x8000} || d.writes[1] != [2]int{0, 0x4000 | 2<<9 | 4<<6 | 4<<3 | 7} {
		t.Fatalf("writes %x", d.writes)
	}
	r, err := m.Read()
	if err != nil {
		t.Fatal(err)
	}
	near(t, "current", r.Current, 1e-6)
	near(t, "bus", r.BusVoltage, 3.3)
	d.regs[1] = u16(-8)
	r, _ = m.Read()
	near(t, "signed", r.Current, -2e-6)
	d.regs[0xFF] = 0x2270
	if err := m.Probe(); err == nil || !strings.Contains(err.Error(), "0x2260") {
		t.Fatalf("wrong die ID: %v", err)
	}
}

func TestINA219(t *testing.T) {
	d := &ina{regs: map[int]int{1: 10, 2: 825<<3 | 1}}
	m := mon(t, "ina219", d, 10, 1)
	if err := m.Configure(false, 0, 0, 0.01); err != nil { // 10 mV -> PGA /1
		t.Fatal(err)
	}
	if got := d.regs[0]; got>>11&3 != 0 || got>>7&0xF != 0b1100 {
		t.Fatalf("config %04x", got)
	}
	r, _ := m.Read()
	near(t, "current", r.Current, 10e-6)
	near(t, "bus", r.BusVoltage, 3.3)
	if !r.Overflow {
		t.Fatal("OVF bit not reported")
	}
	if err := m.Configure(false, 3, 0, 0); err == nil {
		t.Fatal("averaging 3 accepted")
	}
}

func TestINA228And238(t *testing.T) {
	d := &ina{widths: map[int]int{4: 3, 5: 3}, regs: map[int]int{0x3E: 0x5449, 0x3F: 0x2281, 4: 128 << 4, 5: 16896 << 4}}
	m := mon(t, "ina228", d, 10, 1)
	if err := m.Probe(); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(true, 0, 0, 0.01); err != nil { // fine range
		t.Fatal(err)
	}
	if d.regs[0] != 0x10 {
		t.Fatalf("config %x", d.regs[0])
	}
	r, _ := m.Read()
	near(t, "ina228 current", r.Current, 1e-6)
	near(t, "ina228 bus", r.BusVoltage, 3.3)
	if err := m.Configure(false, 0, 0, 0.105); err != nil {
		t.Fatal(err)
	}
	d.regs[4] = (-16 << 4) & 0xFFFFFF
	r, _ = m.Read()
	near(t, "wide range negative", r.Current, -16*312.5e-9/10)

	e := &ina{regs: map[int]int{0x3E: 0x5449, 0x3F: 0x2381, 4: 8, 5: 1056}}
	n := mon(t, "ina238", e, 10, 1)
	if err := n.Probe(); err != nil {
		t.Fatal(err)
	}
	_ = n.Configure(true, 0, 0, 0.01)
	r, _ = n.Read()
	near(t, "ina238 current", r.Current, 1e-6)
	near(t, "ina238 bus", r.BusVoltage, 3.3)
	if e.regs[1]>>12 != 0xB || e.regs[1]&7 != 2 {
		t.Fatalf("adc config %04x", e.regs[1])
	}
	if err := n.Configure(false, 0, 0, 1.0); err == nil || !strings.Contains(err.Error(), "163.84 mV") {
		t.Fatalf("range check: %v", err)
	}
}

func TestINA260AndINA3221(t *testing.T) {
	d := &ina{regs: map[int]int{0xFE: 0x5449, 0xFF: 0x2270, 1: 8, 2: 2640}}
	m := mon(t, "ina260", d, 0.002, 1)
	if err := m.Probe(); err != nil {
		t.Fatal(err)
	}
	r, _ := m.Read()
	near(t, "ina260 current", r.Current, 0.01)

	e := &ina{regs: map[int]int{0xFE: 0x5449, 0xFF: 0x3220, 3: 5 << 3, 4: 412 << 3}}
	n := mon(t, "ina3221", e, 10, 2)
	if err := n.Probe(); err != nil {
		t.Fatal(err)
	}
	r, _ = n.Read()
	near(t, "ina3221 shunt", r.ShuntVoltage, 200e-6)
	near(t, "ina3221 bus", r.BusVoltage, 3.296)
	near(t, "cycle", n.Cycle(), 2*1.1e-3*16*3)
}
