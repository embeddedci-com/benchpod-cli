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
	why          string // why this port was chosen, for the log
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

// flagOrigin records which pin flags the user typed (before the wiring profile filled the rest)
// and the profile's debug_port.
type flagOrigin struct {
	jtag, swd bool   // a JTAG flag / an SWD flag was given on the command line
	debugPort string // the profile's debug_port ("" = automatic)
	debugSet  bool   // the profile has a debug_port
}

// snapshotOrigin notes the flags the user gave, before the profile fills in the others.
func (f *flashFlags) snapshotOrigin() {
	o := &flagOrigin{swd: strings.TrimSpace(f.swclk) != "" || strings.TrimSpace(f.swdio) != ""}
	for _, p := range f.jtagFlagPins() {
		o.jtag = o.jtag || strings.TrimSpace(*p) != ""
	}
	f.origin = o
}

// flagsDecidePort reports whether the pin flags alone pick the port (JTAG flags xor SWD flags).
func (f *flashFlags) flagsDecidePort() bool {
	o := f.originOrNow()
	return o.jtag != o.swd
}

// originOrNow is the recorded origin, or, when no profile was read (fillFromWiring did not run),
// every pin present counts as given on the command line.
func (f *flashFlags) originOrNow() flagOrigin {
	if f.origin != nil {
		return *f.origin
	}
	saved := f.origin
	f.snapshotOrigin()
	o := *f.origin
	f.origin = saved
	return o
}

// validDebugPort checks a wiring profile's debug_port: "swd" or "jtag".
func validDebugPort(v string) error {
	if v != "swd" && v != "jtag" {
		return fmt.Errorf(`debug_port must be "swd" or "jtag" (or left out for automatic), got %q`, v)
	}
	return nil
}

// choosePort is THE rule for SWD versus JTAG. port is "swd", "jtag", or "" for the automatic
// choice (useJTAG); why says where a fixed choice came from, for the log.
//  1. Pin flags win: JTAG flags and no SWD flags -> JTAG; SWD flags and no JTAG flags -> SWD.
//  2. The profile's debug_port, when set.
//  3. Automatic (useJTAG).
func (f *flashFlags) choosePort() (port, why string, err error) {
	o := f.originOrNow()
	switch {
	case o.jtag && !o.swd:
		return "jtag", "JTAG pins given as flags", nil
	case o.swd && !o.jtag:
		return "swd", "SWD pins given as flags", nil
	}
	if o.debugSet {
		d := strings.ToLower(strings.TrimSpace(o.debugPort))
		if err := validDebugPort(d); err != nil {
			return "", "", usageErrorf("the pod's wiring profile: %w", err)
		}
		return d, "the wiring profile's debug_port", nil
	}
	return "", "automatic", nil
}

// missingProfileFields names the profile fields of the port's pins that are not set.
func (f *flashFlags) missingProfileFields(jtag bool) []string {
	var m []string
	if jtag {
		for i, n := range []string{"jtag_tck", "jtag_tms", "jtag_tdi", "jtag_tdo"} {
			if strings.TrimSpace(*f.jtagFlagPins()[i]) == "" {
				m = append(m, n)
			}
		}
		return m
	}
	if strings.TrimSpace(f.swclk) == "" {
		m = append(m, "swd_swclk")
	}
	if strings.TrimSpace(f.swdio) == "" {
		m = append(m, "swd_swdio")
	}
	return m
}

// resolveDAPPins decides SWD or JTAG (choosePort, then useJTAG when automatic) from the flags
// (already filled from the wiring profile) and the target, and parses and checks the pins of the
// chosen one.
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
	port, why, err := f.choosePort()
	if err != nil {
		return dapPins{}, err
	}
	var jtag bool
	switch port {
	case "jtag":
		jtag = true
		if !haveJTAG {
			if why == "the wiring profile's debug_port" {
				return dapPins{}, usageErrorf("debug_port is jtag but %s not set", fieldList(f.missingProfileFields(true)))
			}
			return dapPins{}, usageErrorf("JTAG needs all four of --tck, --tms, --tdi and --tdo (missing %s; or jtag_tck, jtag_tms, jtag_tdi and jtag_tdo in the pod's wiring profile)", strings.Join(missing, ", "))
		}
	case "swd":
		if !haveSWD {
			if why == "the wiring profile's debug_port" {
				return dapPins{}, usageErrorf("debug_port is swd but %s not set", fieldList(f.missingProfileFields(false)))
			}
			return dapPins{}, usageErrorf("--swclk and --swdio are required (or set swd_swclk and swd_swdio in the pod's wiring profile)")
		}
	default:
		jtag = useJTAG(haveJTAG, haveSWD, jtagOnlyTarget(f.target))
	}

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
		return dapPins{swclk: swclk, swdio: swdio, why: f.portWhy(why, jtagOnlyTarget(f.target), false)}, nil
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
	p := dapPins{jtag: true, why: f.portWhy(why, jtagOnlyTarget(f.target), true), jtagPins: tcpclient.JTAGPins{TCK: v[0], TMS: v[1], TDI: v[2], TDO: v[3], TRST: v[4]}}
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

// fieldList renders profile field names with the right verb ("jtag_tdo is", "jtag_tdi, jtag_tdo are").
func fieldList(names []string) string {
	if len(names) == 1 {
		return names[0] + " is"
	}
	return strings.Join(names, ", ") + " are"
}

// portWhy is the log text for the chosen port: where the choice came from.
func (f *flashFlags) portWhy(why string, jtagOnly, jtag bool) string {
	if why != "automatic" {
		return why
	}
	if jtag && jtagOnly {
		return "automatic: a JTAG-only target"
	}
	if jtag {
		return "automatic: only JTAG pins are known"
	}
	return "automatic"
}
