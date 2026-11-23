package main

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/features"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// jtagOnlyTargets are the OpenOCD target names (the config's base name) that have no SWD: flash
// picks JTAG for them even when SWD pins are known. Names starting with "riscv" are JTAG-only too.
// This is the one list.
var jtagOnlyTargets = map[string]bool{"esp32": true, "esp32s2": true, "gd32vf103": true, "fe310": true, "k210": true}

// jtagOnlyTarget reports whether --target (target/esp32s2.cfg, "riscv", ...) is JTAG-only.
func jtagOnlyTarget(target string) bool {
	n := strings.ToLower(strings.TrimSpace(target))
	n = strings.TrimSuffix(path.Base(n), ".cfg")
	return jtagOnlyTargets[n] || strings.HasPrefix(n, "riscv")
}

// useJTAG is the automatic transport choice: JTAG when all four JTAG pins are known and either no
// SWD pins are or the target is JTAG-only; otherwise SWD.
func useJTAG(haveJTAGPins, haveSWDPins, jtagOnly bool) bool {
	return haveJTAGPins && (!haveSWDPins || jtagOnly)
}

// dapPins is what flash arms the pod's probe with: SWD pins or JTAG pins, never both.
type dapPins struct {
	jtag         bool
	swclk, swdio int
	jtagPins     tcpclient.JTAGPins
}

func (p dapPins) transport() string {
	if p.jtag {
		return "jtag"
	}
	return "swd"
}

// jtagFlagPins are the JTAG pin flags in the order TCK, TMS, TDI, TDO, TRST.
func (f *flashFlags) jtagFlagPins() [5]*string {
	return [5]*string{&f.tck, &f.tms, &f.tdi, &f.tdo, &f.trst}
}

var jtagFlagNames = [5]string{"--tck", "--tms", "--tdi", "--tdo", "--trst"}

// resolveDAPPins decides SWD or JTAG from the flags (already filled from the wiring profile) and
// the target, and parses and checks the pins of the chosen one.
func (f *flashFlags) resolveDAPPins() (dapPins, error) {
	haveSWD := strings.TrimSpace(f.swclk) != "" && strings.TrimSpace(f.swdio) != ""
	flags := f.jtagFlagPins()
	var missing []string
	for i := 0; i < 4; i++ {
		if strings.TrimSpace(*flags[i]) == "" {
			missing = append(missing, jtagFlagNames[i])
		}
	}
	haveJTAG := len(missing) == 0
	jtagOnly := jtagOnlyTarget(f.target)
	jtag := useJTAG(haveJTAG, haveSWD, jtagOnly)

	if !jtag {
		if !haveSWD {
			switch {
			case len(missing) < 4 && !haveJTAG:
				return dapPins{}, usageErrorf("JTAG needs all four of --tck, --tms, --tdi and --tdo (missing %s; or jtag_tck, jtag_tms, jtag_tdi and jtag_tdo in the pod's wiring profile)", strings.Join(missing, ", "))
			default:
				return dapPins{}, usageErrorf("--swclk and --swdio are required (or set swd_swclk and swd_swdio in the pod's wiring profile); for JTAG give --tck, --tms, --tdi and --tdo (or jtag_tck, jtag_tms, jtag_tdi and jtag_tdo)")
			}
		}
		swclk, err := parseLAPin(f.swclk)
		if err != nil {
			return dapPins{}, usageErrorf("--swclk: %w", err)
		}
		swdio, err := parseLAPin(f.swdio)
		if err != nil {
			return dapPins{}, usageErrorf("--swdio: %w", err)
		}
		if swclk == swdio {
			return dapPins{}, usageErrorf("--swclk and --swdio must be different pins")
		}
		return dapPins{swclk: swclk, swdio: swdio}, nil
	}

	var v [5]int
	for i := 0; i < 5; i++ {
		if strings.TrimSpace(*flags[i]) == "" {
			continue // only --trst can be empty here
		}
		n, err := parseLAPin(*flags[i])
		if err != nil {
			return dapPins{}, usageErrorf("%s: %w", jtagFlagNames[i], err)
		}
		v[i] = n
	}
	for i := 0; i < 5; i++ {
		for j := i + 1; j < 5; j++ {
			if v[i] != 0 && v[i] == v[j] {
				return dapPins{}, usageErrorf("%s and %s must be different pins", jtagFlagNames[i], jtagFlagNames[j])
			}
		}
	}
	p := dapPins{jtag: true, jtagPins: tcpclient.JTAGPins{TCK: v[0], TMS: v[1], TDI: v[2], TDO: v[3], TRST: v[4]}}
	if haveSWD {
		swclk, err1 := parseLAPin(f.swclk)
		swdio, err2 := parseLAPin(f.swdio)
		if err1 == nil && err2 == nil {
			if err := checkJTAGSWDOverlap(p.jtagPins, swclk, swdio); err != nil {
				return dapPins{}, err
			}
		}
	}
	return p, nil
}

// checkJTAGSWDOverlap allows the two pin sets to share only TCK = SWCLK and TMS = SWDIO (an ARM
// part's SWJ-DP); any other shared channel is a wiring mistake.
func checkJTAGSWDOverlap(j tcpclient.JTAGPins, swclk, swdio int) error {
	for _, c := range []struct {
		name string
		pin  int
	}{{"--tck", j.TCK}, {"--tms", j.TMS}, {"--tdi", j.TDI}, {"--tdo", j.TDO}, {"--trst", j.TRST}} {
		if c.pin == 0 {
			continue
		}
		if c.pin == swclk && c.name != "--tck" || c.pin == swdio && c.name != "--tms" {
			return usageErrorf("%s (LA%d) is also an SWD pin: only TCK may share SWCLK and TMS may share SWDIO", c.name, c.pin)
		}
	}
	return nil
}

// requireJTAGFeature fails when the pod has no JTAG (firmware feature jtag: features.json rev 16,
// gateware 61 or newer), so flash stops before arming the probe.
func requireJTAGFeature(ctx context.Context, c podCommander) error {
	st, err := c.Command(ctx, features.StatusRequest())
	if err != nil {
		return fmt.Errorf("flash: status (checking the pod for JTAG): %w", err)
	}
	ok, err := features.Has(st, "jtag")
	if err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	if !ok {
		return fmt.Errorf("flash: this target needs JTAG, but this pod has no JTAG: it needs firmware with the jtag feature (gateware 61 or newer, and not the emulation FPGA image). Update the pod with `benchpod flash-self`, or wire the target's SWD pins and pass --swclk and --swdio")
	}
	return nil
}
