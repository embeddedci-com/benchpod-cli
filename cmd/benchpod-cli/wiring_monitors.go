package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/expansion"
)

// The wiring profile's `monitors` key: external INA current monitors (up to 8). The pod stores the
// profile opaquely, so the CLI, the SDK and the web app each check it with the same rules:
//
//	"monitors": [{"chip": "ina226", "addr": 64, "shunt_ohm": 10.0, "name": "dut", "channel": 1,
//	              "sda": 13, "scl": 14, "averaging": 16, "conversion_time": 0.0011, "max_current": 0.001}]

const maxProfileMonitors = 8

// profileMonitor is one `monitors` entry. Fields left out are zero (SDA, SCL and ShuntOhm are
// pointers: nil = not set).
type profileMonitor struct {
	Chip           string   `json:"chip"`
	Addr           int      `json:"addr"`
	ShuntOhm       *float64 `json:"shunt_ohm"`
	Name           string   `json:"name"`
	Channel        int      `json:"channel"`
	SDA            *int     `json:"sda"`
	SCL            *int     `json:"scl"`
	Averaging      int      `json:"averaging"`
	ConversionTime float64  `json:"conversion_time"`
	MaxCurrent     float64  `json:"max_current"`
}

var monitorNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.@-]{0,31}$`)

var monitorKeys = map[string]bool{"chip": true, "addr": true, "shunt_ohm": true, "name": true, "channel": true,
	"sda": true, "scl": true, "averaging": true, "conversion_time": true, "max_current": true}

// displayName is the entry's name, or the schema's default: chip@0xAA (ina3221: .channel appended).
func (m profileMonitor) displayName(chip expansion.Chip) string {
	if m.Name != "" {
		return m.Name
	}
	n := fmt.Sprintf("%s@0x%02x", chip.Name, m.Addr)
	if chip.Channels > 1 {
		ch := m.Channel
		if ch == 0 {
			ch = 1
		}
		n += fmt.Sprintf(".%d", ch)
	}
	return n
}

// monitorsFor returns the profile's monitors entries for chip, in profile order.
func (w *podWiring) monitorsFor(chip expansion.Chip) []profileMonitor {
	if w == nil {
		return nil
	}
	var out []profileMonitor
	for _, m := range w.Monitors {
		if c, err := expansion.Lookup(m.Chip); err == nil && c.Name == chip.Name {
			out = append(out, m)
		}
	}
	return out
}

func asInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.Abs(f) > 1e9 {
		return 0, false
	}
	return int(f), true
}

// validateMonitors checks the `monitors` key of a profile (decoded JSON) with the schema's rules.
// A profile without the key, or with [], is fine.
func validateMonitors(p map[string]any) error {
	raw, has := p["monitors"]
	if !has || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("monitors must be an array of objects")
	}
	if len(list) > maxProfileMonitors {
		return fmt.Errorf("monitors has %d entries; at most %d", len(list), maxProfileMonitors)
	}
	var profSDA, profSCL int
	if v, ok := asInt(p["i2c_sda"]); ok {
		profSDA = v
	}
	if v, ok := asInt(p["i2c_scl"]); ok {
		profSCL = v
	}
	names := map[string]bool{}
	slots := map[[2]int]bool{}
	var sda, scl int // the pins every monitor must share (0 = none resolved yet)
	for i, e := range list {
		at := fmt.Sprintf("monitors[%d]", i)
		m, ok := e.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", at)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			if !monitorKeys[k] {
				keys = append(keys, k)
			}
		}
		if len(keys) > 0 {
			sort.Strings(keys)
			return fmt.Errorf("%s: unknown key %s", at, strings.Join(keys, ", "))
		}
		cs, _ := m["chip"].(string)
		chip, err := expansion.Lookup(cs)
		if cs == "" || err != nil || chip.Kind != "current" {
			return fmt.Errorf("%s: chip must be an INA current monitor (ina226, ina228, ina238, ina219, ina260 or ina3221), got %v", at, m["chip"])
		}
		addr, ok := asInt(m["addr"])
		if !ok || !chip.HasAddr(addr) {
			return fmt.Errorf("%s: addr must be an integer 7-bit address, %s for the %s (64 is 0x40)", at, chip.AddrRange(), chip.Name)
		}
		if v, has := m["shunt_ohm"]; has && v != nil {
			s, ok := v.(float64)
			switch {
			case !ok || s <= 0 || s > 1e6:
				return fmt.Errorf("%s: shunt_ohm must be a number above 0 and at most 1e6", at)
			case chip.InternalOhm > 0 && s != chip.InternalOhm:
				return fmt.Errorf("%s: the %s has its own %g mohm shunt: leave shunt_ohm out", at, chip.Name, chip.InternalOhm*1000)
			}
		} else if chip.InternalOhm == 0 {
			return fmt.Errorf("%s: shunt_ohm is required for the %s", at, chip.Name)
		}
		ch := 1
		if v, has := m["channel"]; has && v != nil {
			n, ok := asInt(v)
			if chip.Channels <= 1 {
				return fmt.Errorf("%s: channel is for the ina3221 only", at)
			}
			if !ok || n < 1 || n > chip.Channels {
				return fmt.Errorf("%s: channel must be 1 to %d", at, chip.Channels)
			}
			ch = n
		}
		if slots[[2]int{addr, ch}] {
			return fmt.Errorf("%s: 0x%02x channel %d is listed twice", at, addr, ch)
		}
		slots[[2]int{addr, ch}] = true
		name := ""
		if v, has := m["name"]; has && v != nil {
			s, _ := v.(string)
			if !monitorNameRE.MatchString(s) || strings.EqualFold(s, "ext") {
				return fmt.Errorf(`%s: name must start with a letter, use letters, digits and _.@- (32 characters at most) and not be "ext", got %v`, at, v)
			}
			name = s
		}
		pm := profileMonitor{Chip: chip.Name, Addr: addr, Name: name, Channel: ch}
		key := strings.ToLower(pm.displayName(chip))
		if names[key] {
			return fmt.Errorf("%s: the name %q is used twice (names are unique, case does not matter)", at, pm.displayName(chip))
		}
		names[key] = true
		if v, has := m["averaging"]; has && v != nil {
			n, ok := asInt(v)
			valid := map[int]bool{1: true, 4: true, 16: true, 64: true, 128: true, 256: true, 512: true, 1024: true}
			list := "1, 4, 16, 64, 128, 256, 512 or 1024"
			if chip.Name == "ina219" {
				valid = map[int]bool{1: true, 2: true, 4: true, 8: true, 16: true, 32: true, 64: true, 128: true}
				list = "1, 2, 4, 8, 16, 32, 64 or 128"
			}
			if !ok || !valid[n] {
				return fmt.Errorf("%s: averaging for the %s is one of %s", at, chip.Name, list)
			}
		}
		if v, has := m["conversion_time"]; has && v != nil {
			if s, ok := v.(float64); !ok || s <= 0 || s > 0.01 {
				return fmt.Errorf("%s: conversion_time is in seconds, above 0 and at most 0.01", at)
			}
		}
		if v, has := m["max_current"]; has && v != nil {
			if s, ok := v.(float64); !ok || s <= 0 {
				return fmt.Errorf("%s: max_current is in amps and above 0", at)
			}
		}
		pins := [2]int{profSDA, profSCL}
		for j, k := range []string{"sda", "scl"} {
			if v, has := m[k]; has && v != nil {
				n, ok := asInt(v)
				if !ok || n < 1 || n > laPinCount {
					return fmt.Errorf("%s: %s must be an LA channel 1 to %d", at, k, laPinCount)
				}
				pins[j] = n
			}
		}
		if pins[0] != 0 && pins[0] == pins[1] {
			return fmt.Errorf("%s: sda and scl are both LA%d; each needs its own pin", at, pins[0])
		}
		if pins[0] != 0 && pins[1] != 0 {
			if sda == 0 {
				sda, scl = pins[0], pins[1]
			} else if sda != pins[0] || scl != pins[1] {
				return fmt.Errorf("%s: SDA LA%d, SCL LA%d differ from the other monitors' LA%d and LA%d (the pod has one I2C master)", at, pins[0], pins[1], sda, scl)
			}
		}
	}
	return nil
}
