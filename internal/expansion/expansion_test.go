package expansion

import "testing"

func TestLookupAndLayouts(t *testing.T) {
	if c, err := Lookup("TCA9535"); err != nil || c.Name != "tca9555" {
		t.Fatalf("alias: %v %v", c, err)
	}
	for _, c := range Chips() {
		if c.Bus != "i2c" || c.Kind == "current" {
			continue
		}
		if _, err := layout(c); err != nil {
			t.Fatalf("%s has no read layout: %v", c.Name, err)
		}
	}
}

func TestReadPCFQuasi(t *testing.T) {
	c, _ := Lookup("pcf8574")
	pins, err := ReadI2C(c, []int{0x20}, func(addr int, w []byte, n int) ([]byte, error) {
		if len(w) != 0 || n != 1 {
			t.Fatalf("pcf read: write %v read %d", w, n)
		}
		return []byte{0x81}, nil
	})
	if err != nil || len(pins) != 8 || pins[0].Dir != "quasi" || pins[0].Level != 1 || pins[1].Level != 0 || pins[7].ChipPin != "P7" {
		t.Fatalf("%+v %v", pins, err)
	}
}

func TestReadMCP23017TwoChips(t *testing.T) {
	c, _ := Lookup("mcp23017")
	regs := map[int][]byte{0x12: {0x01, 0x00}, 0x14: {0x01, 0x00}, 0x00: {0xFE, 0xFF}}
	pins, err := ReadI2C(c, []int{0x20, 0x21}, func(addr int, w []byte, n int) ([]byte, error) {
		return regs[int(w[0])], nil
	})
	if err != nil || len(pins) != 32 || pins[16].Pin != "EXT17" || pins[16].Addr != 0x21 || pins[8].ChipPin != "GPB0" || pins[0].Dir != "output" {
		t.Fatalf("%+v %v", pins, err)
	}
}
