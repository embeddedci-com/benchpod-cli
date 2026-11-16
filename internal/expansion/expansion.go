// Package expansion knows the GPIO expander chips the embeddedci SDK drives (chips.json, a
// snapshot of its EXPANSION_CHIPS) and reads an I2C expander's pins: direction and live level.
//
// The SDK (bp.expansion) and the MCP server attach and drive expanders; the CLI only reads one
// (`benchpod expansion`), so a check on the bench needs no Python.
package expansion

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed chips.json
var chipsJSON []byte

// Chip is one entry of the SDK's EXPANSION_CHIPS.
type Chip struct {
	Name       string    `json:"name"`
	Bus        string    `json:"bus"`
	Pins       int       `json:"pins"`
	LAPins     int       `json:"la_pins"`
	Addrs      []int     `json:"addrs"`
	Voltages   []float64 `json:"voltages"`
	Directions string    `json:"directions"`
	MaxHz      int       `json:"max_hz"`
	Since      string    `json:"since"`
	Aliases    []string  `json:"aliases"`
	Note       string    `json:"note"`
}

// Chips is every chip the SDK drives, in its order.
func Chips() []Chip {
	var f struct {
		Chips []Chip `json:"chips"`
	}
	if err := json.Unmarshal(chipsJSON, &f); err != nil {
		panic("expansion: chips.json: " + err.Error())
	}
	return f.Chips
}

// Lookup finds a chip by name or alias, case-insensitive.
func Lookup(name string) (Chip, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	var names []string
	for _, c := range Chips() {
		if key == c.Name {
			return c, nil
		}
		for _, a := range c.Aliases {
			if key == a {
				return c, nil
			}
		}
		names = append(names, c.Name)
	}
	return Chip{}, fmt.Errorf("unknown expander chip %q; known: %s", name, strings.Join(names, ", "))
}

// HasAddr reports whether addr is one of the chip's addresses.
func (c Chip) HasAddr(addr int) bool {
	for _, a := range c.Addrs {
		if a == addr {
			return true
		}
	}
	return false
}

// AddrRange is "0x20-0x27".
func (c Chip) AddrRange() string {
	if len(c.Addrs) == 0 {
		return ""
	}
	a := append([]int(nil), c.Addrs...)
	sort.Ints(a)
	return fmt.Sprintf("0x%02x-0x%02x", a[0], a[len(a)-1])
}

// Pin is one expander pin as read from the chip.
type Pin struct {
	Pin     string `json:"pin"`      // EXT1
	ChipPin string `json:"chip_pin"` // P00, GPA0
	Addr    int    `json:"addr"`
	// Dir is input or output (in/out chips), or quasi (PCF857x: a pin reads what it is pulled to).
	Dir   string `json:"dir"`
	Level int    `json:"level"`
	// Out is the output latch, for an output pin.
	Out *int `json:"out,omitempty"`
}

// Xfer writes then reads one I2C device (a repeated START between), like the pod's i2c_xfer.
type Xfer func(addr int, write []byte, read int) ([]byte, error)

type regs struct {
	ports        int
	in, out, cfg int // register addresses; cfg < 0: no direction register (1 = input)
	label        func(i int) string
}

func portLabel(i int) string { return fmt.Sprintf("P%d%d", i/8, i%8) }

func layout(c Chip) (regs, error) {
	switch c.Name {
	case "tca9555", "pca9555", "tca6416a":
		return regs{ports: 2, in: 0x00, out: 0x02, cfg: 0x06, label: portLabel}, nil
	case "tca6424a":
		return regs{ports: 3, in: 0x80, out: 0x84, cfg: 0x8C, label: portLabel}, nil
	case "mcp23017":
		return regs{ports: 2, in: 0x12, out: 0x14, cfg: 0x00,
			label: func(i int) string { return fmt.Sprintf("GP%c%d", "AB"[i/8], i%8) }}, nil
	case "pcf8574", "pcf8574a":
		return regs{ports: 1, cfg: -1, label: func(i int) string { return fmt.Sprintf("P%d", i) }}, nil
	case "pcf8575":
		return regs{ports: 2, cfg: -1, label: portLabel}, nil
	}
	return regs{}, fmt.Errorf("the CLI reads I2C expanders only; drive a %s (%s) from the SDK (bp.expansion) or the MCP server's expansion tool", c.Name, c.Bus)
}

func le(b []byte) int {
	v := 0
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | int(b[i])
	}
	return v
}

// ReadI2C reads every pin of the chips at addrs (EXT pins numbered in that order). It only reads:
// nothing on the chip changes. An MCP23017 is read as IOCON.BANK = 0 (its power-on state).
func ReadI2C(c Chip, addrs []int, xfer Xfer) ([]Pin, error) {
	r, err := layout(c)
	if err != nil {
		return nil, err
	}
	var out []Pin
	n := 1
	for _, addr := range addrs {
		read := func(reg int) (int, error) {
			var w []byte
			if reg >= 0 {
				w = []byte{byte(reg)}
			}
			b, err := xfer(addr, w, r.ports)
			if err != nil {
				return 0, err
			}
			if len(b) != r.ports {
				return 0, fmt.Errorf("0x%02x answered %d bytes, not %d", addr, len(b), r.ports)
			}
			return le(b), nil
		}
		var level, latch, cfg int
		if r.cfg < 0 {
			if level, err = read(-1); err != nil {
				return nil, err
			}
		} else {
			if level, err = read(r.in); err != nil {
				return nil, err
			}
			if latch, err = read(r.out); err != nil {
				return nil, err
			}
			if cfg, err = read(r.cfg); err != nil {
				return nil, err
			}
		}
		for i := 0; i < 8*r.ports; i++ {
			p := Pin{Pin: fmt.Sprintf("EXT%d", n), ChipPin: r.label(i), Addr: addr, Level: level >> i & 1}
			switch {
			case r.cfg < 0:
				p.Dir = "quasi"
			case cfg>>i&1 == 1:
				p.Dir = "input"
			default:
				p.Dir = "output"
				o := latch >> i & 1
				p.Out = &o
			}
			out = append(out, p)
			n++
		}
	}
	return out, nil
}
