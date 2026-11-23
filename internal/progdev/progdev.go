// Package progdev is the table of chips the pod's prog_flash command can program: for each
// device its protocol, ID bytes, flash size, write page and protocol flags. The table is
// prog_devices.json, copied from the embeddedci-python SDK by scripts/sync-prog-devices.sh.
package progdev

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed prog_devices.json
var raw []byte

// Device is one programmable chip.
type Device struct {
	Name      string   `json:"name"`
	Proto     string   `json:"proto"` // updi, pic, c2, tpi, pdi
	ID        string   `json:"id"`    // ID bytes in hex, as the chip sends them
	FlashSize int      `json:"flash_size"`
	Page      int      `json:"page"`
	Flags     int      `json:"flags"`
	Aliases   []string `json:"aliases"`
}

var devices []Device

func init() {
	var t struct {
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		panic("progdev: prog_devices.json: " + err.Error())
	}
	devices = t.Devices
}

// Targets maps a --target name or alias to the prog_flash protocol.
var Targets = map[string]string{
	"avr-updi": "updi", "updi": "updi", "pic": "pic", "efm8": "c2", "c2": "c2",
	"attiny-tpi": "tpi", "tpi": "tpi", "xmega": "pdi", "pdi": "pdi",
}

// Proto is the protocol a --target value names ("" if it is not one).
func Proto(target string) string { return Targets[strings.ToLower(strings.TrimSpace(target))] }

func norm(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Find looks a device up by name or alias within a protocol.
func Find(proto, name string) (Device, error) {
	n := norm(name)
	for _, d := range devices {
		if d.Proto != proto {
			continue
		}
		if norm(d.Name) == n {
			return d, nil
		}
		for _, a := range d.Aliases {
			if norm(a) == n {
				return d, nil
			}
		}
	}
	return Device{}, fmt.Errorf("unknown %s device %q (known: %s)", proto, name, strings.Join(Names(proto), ", "))
}

// ByID finds the devices of a protocol whose ID starts with the bytes the chip answered.
func ByID(proto, id string) []Device {
	var out []Device
	id = norm(id)
	for _, d := range devices {
		if d.Proto == proto && id != "" && strings.HasPrefix(id, norm(d.ID)) {
			out = append(out, d)
		}
	}
	return out
}

// Names lists the device names of a protocol, sorted.
func Names(proto string) []string {
	var out []string
	for _, d := range devices {
		if d.Proto == proto {
			out = append(out, d.Name)
		}
	}
	sort.Strings(out)
	return out
}
