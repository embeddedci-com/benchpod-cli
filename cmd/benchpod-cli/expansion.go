package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/expansion"
	"github.com/spf13/cobra"
)

// `benchpod expansion`: the GPIO expander chips the SDK drives, and a read-only look at one I2C
// expander's pins through the pod's I2C master. Attaching and driving EXT pins is the SDK's
// bp.expansion and the MCP server's expansion tool; the CLI keeps no state between runs.

func newExpansionCmd(g *globalFlags) *cobra.Command {
	var addrs, sda, scl, pullups string
	var hz int
	var asJSON bool
	var mf monitorFlags
	cmd := &cobra.Command{
		Use:   "expansion [chip]",
		Short: "List the GPIO expander and INA current monitor chips, or read one (pins, or a µA current)",
		Long: "Without a chip, list the GPIO expander boards the SDK drives (bp.expansion in\n" +
			"Python, the expansion tool of the MCP server). With an I2C chip, read every pin\n" +
			"of it through the pod's I2C master: direction and live level, pins named EXT1..EXTn\n" +
			"as the SDK names them. It only reads; nothing on the chip changes. An open\n" +
			"`benchpod i2c start` session is used as it is; otherwise one is opened on --sda and\n" +
			"--scl (default: the wiring profile's i2c_sda and i2c_scl) and closed afterwards.\n" +
			"With --pullups pod the pull-ups must be on (`benchpod la pull 1 up`).\n\n" +
			"An INA current monitor (ina226, ina228, ina238, ina219, ina260, ina3221) measures µA\n" +
			"currents instead: wire a shunt in series between the pod's target-power output and the\n" +
			"DUT and the chip across it (bus pin on the DUT side), then give --shunt (ohms). It is\n" +
			"probed by ID, reset, configured for continuous conversion and read: the current, bus\n" +
			"voltage and power, or with --seconds N the mean, min and max. With 10 ohm an INA226\n" +
			"resolves 0.25 µA up to about 8 mA. The LA voltage must be 3.3 V or 5 V (not 1.8 V).",
		Example: "  benchpod expansion\n" +
			"  benchpod expansion tca9555 --addr 0x20 --sda 1 --scl 2\n" +
			"  benchpod expansion mcp23017 --addr 0x20,0x21 --pullups external\n" +
			"  benchpod expansion ina226 --shunt 10 --sda 1 --scl 2\n" +
			"  benchpod expansion ina228 --shunt 10 --max-current 0.001 --seconds 5 --json",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("expansion: open output: %w", err)
			}
			defer closeOut()
			if len(args) == 0 {
				return printExpansionChips(out, asJSON)
			}
			chip, err := expansion.Lookup(args[0])
			if err != nil {
				return usageErrorf("%v", err)
			}
			if chip.Bus != "i2c" {
				return usageErrorf("the CLI reads I2C expanders only; drive a %s (SPI) from the SDK (bp.expansion) or the MCP server's expansion tool", chip.Name)
			}
			list, err := parseExpansionAddrs(chip, addrs)
			if err != nil {
				return err
			}
			if pullups != "pod" && pullups != "external" {
				return usageErrorf("--pullups must be pod or external")
			}
			if chip.Kind == "current" {
				if chip.InternalOhm == 0 && mf.shunt <= 0 {
					return usageErrorf("--shunt is required for the %s: the shunt resistor in ohms, e.g. --shunt 10", chip.Name)
				}
				if mf.channel == 0 {
					mf.channel = 1
				}
				if mf.channel < 1 || mf.channel > chip.Channels {
					return usageErrorf("--channel is 1 to %d for the %s", chip.Channels, chip.Name)
				}
				if mf.seconds < 0 || mf.seconds > 600 {
					return usageErrorf("--seconds must be 0 to 600")
				}
			} else if mf.shunt != 0 || mf.channel != 0 || mf.seconds != 0 || mf.averaging != 0 || mf.convTime != 0 || mf.maxCurrent != 0 {
				return usageErrorf("--shunt, --channel, --seconds, --averaging, --conversion-time and --max-current are for the INA current monitors")
			}
			if hz == 0 {
				hz = chip.MaxHz
			}
			if hz < 10000 || hz > chip.MaxHz {
				return usageErrorf("--hz must be 10000 to %d for the %s", chip.MaxHz, chip.Name)
			}
			ctx, cancel, t, err := g.podClient("expansion", i2cTimeout+time.Duration(mf.seconds*float64(time.Second)))
			if err != nil {
				return err
			}
			defer cancel()
			var st i2cSession
			if err := busCommand(ctx, t, "expansion", "I2C master", map[string]any{"cmd": "i2c_status"}, &st); err != nil {
				return err
			}
			if st.Active != nil && *st.Active {
				if sda != "" || scl != "" {
					return usageErrorf("an I2C session is open on SDA LA%d, SCL LA%d: leave out --sda/--scl to use it, or `benchpod i2c stop` first", st.SDA, st.SCL)
				}
			} else {
				if st, err = startExpansionBus(ctx, g, t, sda, scl, hz, pullups, "expansion"); err != nil {
					return err
				}
				defer func() {
					_ = busCommand(context.Background(), t, "expansion", "I2C master", map[string]any{"cmd": "i2c_stop"}, nil)
				}()
			}
			xfer := func(addr int, write []byte, read int) ([]byte, error) {
				req := map[string]any{"cmd": "i2c_xfer", "addr": addr, "read": read}
				if len(write) > 0 {
					req["write"] = base64.RawURLEncoding.EncodeToString(write)
				}
				var rep i2cXferReply
				if err := busCommand(ctx, t, "expansion", "I2C master", req, &rep); err != nil {
					return nil, err
				}
				if !rep.Ack {
					return nil, fmt.Errorf("expansion: no %s answers at 0x%02x on SDA LA%d, SCL LA%d (check the address jumpers, the breakout's power and its pull-ups; `benchpod i2c scan` lists what answers)", chip.Name, addr, st.SDA, st.SCL)
				}
				if rep.Rx == nil {
					return nil, nil
				}
				return base64.RawURLEncoding.DecodeString(strings.TrimRight(*rep.Rx, "="))
			}
			if chip.Kind == "current" {
				return runMonitors(ctx, out, chip, list, mf, st, asJSON, xfer)
			}
			pins, err := expansion.ReadI2C(chip, list, xfer)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(map[string]any{"chip": chip.Name, "addrs": list, "sda": st.SDA, "scl": st.SCL, "pins": pins}, "", "  ")
				_, err := fmt.Fprintln(out, string(b))
				return err
			}
			addrText := make([]string, len(list))
			for i, a := range list {
				addrText[i] = fmt.Sprintf("0x%02x", a)
			}
			fmt.Fprintf(out, "%s at %s (SDA LA%d, SCL LA%d): %d pins\n", chip.Name, strings.Join(addrText, ", "), st.SDA, st.SCL, len(pins))
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			for _, p := range pins {
				extra := ""
				if p.Out != nil && *p.Out != p.Level {
					extra = fmt.Sprintf("  (driven %d, reads %d)", *p.Out, p.Level)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d%s\n", p.Pin, p.ChipPin, p.Dir, p.Level, extra)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&addrs, "addr", "", "the chip's 7-bit address, or several for chips on one bus: 0x20,0x21 (default: the chip's first)")
	cmd.Flags().StringVar(&sda, "sda", "", "LA pin for SDA when no I2C session is open (default: i2c_sda from the pod's wiring profile)")
	cmd.Flags().StringVar(&scl, "scl", "", "LA pin for SCL when no I2C session is open (default: i2c_scl from the pod's wiring profile)")
	cmd.Flags().IntVar(&hz, "hz", 0, "SCL rate in Hz when a session is opened (default: the chip's limit, 400 kHz or 100 kHz)")
	cmd.Flags().StringVar(&pullups, "pullups", "pod", "pod (the pod's pull-ups on LA1-LA6, which must be on: benchpod la pullup 1 on) or external (the breakout's)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().Float64Var(&mf.shunt, "shunt", 0, "INA monitors: the shunt resistor in ohms (required, except for the INA260)")
	cmd.Flags().IntVar(&mf.channel, "channel", 0, "ina3221: channel 1-3 (default 1)")
	cmd.Flags().Float64Var(&mf.seconds, "seconds", 0, "INA monitors: measure this many seconds (mean, min, max); 0 is one reading")
	cmd.Flags().IntVar(&mf.averaging, "averaging", 0, "INA monitors: conversions averaged per result: 1, 4, 16, 64, 128, 256, 512, 1024 (ina219: 1-128, powers of two); default 16")
	cmd.Flags().DurationVar(&mf.convTime, "conversion-time", 0, "INA monitors: per shunt and bus conversion, the nearest the chip has: 1.1ms (default); 50us to 8.2ms")
	cmd.Flags().Float64Var(&mf.maxCurrent, "max-current", 0, "INA monitors: largest current expected in amps (0.001 = 1 mA): ina219, ina228 and ina238 pick the narrowest range covering it")
	return cmd
}

func parseExpansionAddrs(chip expansion.Chip, s string) ([]int, error) {
	if strings.TrimSpace(s) == "" {
		return []int{chip.Addrs[0]}, nil
	}
	var out []int
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		a, err := parseI2CAddr(part)
		if err != nil {
			return nil, usageErrorf("--addr: %v", err)
		}
		if !chip.HasAddr(a) {
			return nil, usageErrorf("--addr: the %s has addresses %s, not 0x%02x", chip.Name, chip.AddrRange(), a)
		}
		if seen[a] {
			return nil, usageErrorf("--addr: 0x%02x is listed twice", a)
		}
		seen[a] = true
		out = append(out, a)
	}
	return out, nil
}

func startExpansionBus(ctx context.Context, g *globalFlags, t *podTarget, sda, scl string, hz int, pullups, what string) (i2cSession, error) {
	r := &pinResolver{g: g}
	if sda == "" || scl == "" {
		if w := r.wiring(); w != nil {
			pinFromWiring(&sda, w.I2cSda)
			pinFromWiring(&scl, w.I2cScl)
		}
	}
	if sda == "" || scl == "" {
		return i2cSession{}, usageErrorf("--sda and --scl are required (or set i2c_sda and i2c_scl in the pod's wiring profile, or open a session with `benchpod i2c start`)")
	}
	sdaPin, err := r.pin("--sda", sda)
	if err != nil {
		return i2cSession{}, err
	}
	sclPin, err := r.pin("--scl", scl)
	if err != nil {
		return i2cSession{}, err
	}
	if sdaPin == sclPin {
		return i2cSession{}, usageErrorf("--sda and --scl are both LA%d; each needs its own pin", sdaPin)
	}
	if err := ensureBusMasterImage(ctx, t.client, "the I2C master"); err != nil {
		return i2cSession{}, fmt.Errorf("%s: %w", what, err)
	}
	var s i2cSession
	req := map[string]any{"cmd": "i2c_start", "sda": sdaPin, "scl": sclPin, "clock_hz": hz, "pullups": pullups}
	if err := busCommand(ctx, t, what, "I2C master", req, &s); err != nil {
		return i2cSession{}, err
	}
	fmt.Fprintf(os.Stderr, "%s: opened I2C on %s for this run\n", what, s.String())
	return s, nil
}

func printExpansionChips(out io.Writer, asJSON bool) error {
	chips := expansion.Chips()
	if asJSON {
		b, _ := json.MarshalIndent(chips, "", "  ")
		_, err := fmt.Fprintln(out, string(b))
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHIP\tBUS\tLA PINS\tPINS\tLA VOLTAGE\tDIRECTIONS")
	for _, c := range chips {
		bus := strings.ToUpper(c.Bus)
		if r := c.AddrRange(); r != "" && c.Bus == "i2c" {
			bus += " " + r
		}
		volts := make([]string, len(c.Voltages))
		for i, v := range c.Voltages {
			volts[i] = fmt.Sprintf("%g V", v)
		}
		pins := fmt.Sprint(c.Pins)
		if c.Kind == "current" {
			pins = "current monitor"
		} else if len(c.Addrs) == 0 {
			pins += " per chip"
		}
		name := c.Name
		if len(c.Aliases) > 0 {
			name += " (" + strings.Join(c.Aliases, ", ") + ")"
		}
		dirs := c.Directions
		if dirs == "" {
			dirs = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", name, bus, c.LAPins, pins, strings.Join(volts, ", "), dirs)
	}
	return tw.Flush()
}
