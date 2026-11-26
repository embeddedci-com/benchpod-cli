package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/currentmonitor"
	"github.com/spf13/cobra"
)

// `benchpod current-monitor`: external INA current monitors (µA currents) through the pod's I2C
// master. The SDK's bp.current_monitor and the MCP server's current_monitor tool are the reference
// (the chip table and register model are the same); the CLI keeps no state between runs.

func newCurrentMonitorCmd(g *globalFlags) *cobra.Command {
	var addrFlag, sda, scl, pullups string
	var shunt, maxCurrent, seconds float64
	var channel, averaging, hz int
	var convTime time.Duration
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "current-monitor [chip]",
		Short: "List the INA current monitor chips, or read one (a µA-resolution current)",
		Long: "Without a chip, list the INA current monitor breakouts the SDK drives (bp.current_monitor\n" +
			"in Python, the current_monitor tool of the MCP server). With a chip, probe it, configure\n" +
			"continuous conversion and read the current through its shunt: --seconds N measures for N\n" +
			"seconds (mean, min, max). Wire the shunt in series between the pod's target-power output\n" +
			"and the DUT, the chip across it (bus pin on the DUT side); with 10 ohm an INA226 resolves\n" +
			"0.25 µA up to about 8 mA. The LA voltage must be 3.3 V (or 5 V): the chips do not run at\n" +
			"1.8 V. An open `benchpod i2c start` session is used as it is; otherwise one is opened on\n" +
			"--sda and --scl (default: the wiring profile's i2c_sda and i2c_scl) and closed afterwards.\n" +
			"With --pullups pod the pull-ups must be on (`benchpod la pull 1 up`). The chip is reset and\n" +
			"configured on every run, and keeps running afterwards.",
		Example: "  benchpod current-monitor\n" +
			"  benchpod current-monitor ina226 --shunt 10 --sda 1 --scl 2\n" +
			"  benchpod current-monitor ina228 --shunt 10 --max-current 0.001 --seconds 5 --json",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("current-monitor: open output: %w", err)
			}
			defer closeOut()
			if len(args) == 0 {
				return printMonitorChips(out, asJSON)
			}
			chip, err := currentmonitor.Lookup(args[0])
			if err != nil {
				return usageErrorf("%v", err)
			}
			addr := chip.Addrs[0]
			if strings.TrimSpace(addrFlag) != "" {
				if addr, err = parseI2CAddr(addrFlag); err != nil {
					return usageErrorf("--addr: %v", err)
				}
				if !chip.HasAddr(addr) {
					return usageErrorf("--addr: the %s has addresses %s, not 0x%02x", chip.Name, chip.AddrRange(), addr)
				}
			}
			if chip.InternalOhm > 0 {
				if shunt != 0 && shunt != chip.InternalOhm {
					return usageErrorf("the %s has its own %g mohm shunt: leave --shunt out", chip.Name, chip.InternalOhm*1000)
				}
				shunt = chip.InternalOhm
			} else if shunt <= 0 {
				return usageErrorf("--shunt is required: the shunt resistor in ohms, e.g. --shunt 10")
			}
			if channel == 0 {
				channel = 1
			}
			if channel < 1 || channel > chip.Channels {
				return usageErrorf("--channel is 1 to %d for the %s", chip.Channels, chip.Name)
			}
			if pullups != "pod" && pullups != "external" {
				return usageErrorf("--pullups must be pod or external")
			}
			if hz < 10000 || hz > 1000000 {
				return usageErrorf("--hz must be 10000 to 1000000")
			}
			if seconds < 0 || seconds > 600 {
				return usageErrorf("--seconds must be 0 to 600")
			}
			maxShunt := maxCurrent * shunt
			ctx, cancel, t, err := g.podClient("current-monitor", i2cTimeout+time.Duration(seconds*float64(time.Second)))
			if err != nil {
				return err
			}
			defer cancel()
			var st i2cSession
			if err := busCommand(ctx, t, "current-monitor", "I2C master", map[string]any{"cmd": "i2c_status"}, &st); err != nil {
				return err
			}
			if st.Active != nil && *st.Active {
				if sda != "" || scl != "" {
					return usageErrorf("an I2C session is open on SDA LA%d, SCL LA%d: leave out --sda/--scl to use it, or `benchpod i2c stop` first", st.SDA, st.SCL)
				}
			} else {
				if st, err = startExpansionBus(ctx, g, t, sda, scl, hz, pullups, "current-monitor"); err != nil {
					return err
				}
				defer func() {
					_ = busCommand(context.Background(), t, "current-monitor", "I2C master", map[string]any{"cmd": "i2c_stop"}, nil)
				}()
			}
			xfer := func(a int, write []byte, read int) ([]byte, error) {
				req := map[string]any{"cmd": "i2c_xfer", "addr": a, "read": read}
				if len(write) > 0 {
					req["write"] = base64.RawURLEncoding.EncodeToString(write)
				}
				var rep i2cXferReply
				if err := busCommand(ctx, t, "current-monitor", "I2C master", req, &rep); err != nil {
					return nil, err
				}
				if !rep.Ack {
					return nil, fmt.Errorf("current-monitor: no %s answers at 0x%02x on SDA LA%d, SCL LA%d (check the A0/A1 address pins, the breakout's power and its pull-ups; `benchpod i2c scan` lists what answers)", chip.Name, a, st.SDA, st.SCL)
				}
				if rep.Rx == nil {
					return nil, nil
				}
				return base64.RawURLEncoding.DecodeString(strings.TrimRight(*rep.Rx, "="))
			}
			mon := currentmonitor.New(chip, addr, channel, shunt, xfer)
			if err := mon.Probe(); err != nil {
				return err
			}
			if err := mon.Configure(true, averaging, convTime.Seconds(), maxShunt); err != nil {
				return usageErrorf("%v", err)
			}
			res, err := sampleMonitor(ctx, mon, seconds)
			if err != nil {
				return err
			}
			res.Chip, res.Addr, res.Channel, res.ShuntOhm, res.SDA, res.SCL = chip.Name, addr, channel, shunt, st.SDA, st.SCL
			if asJSON {
				b, _ := json.MarshalIndent(res, "", "  ")
				_, err := fmt.Fprintln(out, string(b))
				return err
			}
			fmt.Fprintf(out, "%s at 0x%02x (SDA LA%d, SCL LA%d), shunt %g ohm\n", chip.Name, addr, st.SDA, st.SCL, shunt)
			if res.N == 1 {
				fmt.Fprintf(out, "current  %s\nbus      %.4f V\nshunt    %s\npower    %s\n", formatAmps(res.Mean), res.BusVoltage,
					formatVolts(res.Mean*shunt), formatWatts(res.Mean*res.BusVoltage))
			} else {
				fmt.Fprintf(out, "%d readings in %.2f s\nmean     %s\nmin      %s\nmax      %s\nbus      %.4f V\ncharge   %.3g C\n",
					res.N, res.Duration, formatAmps(res.Mean), formatAmps(res.Min), formatAmps(res.Max), res.BusVoltage, res.Mean*res.Duration)
			}
			if res.Overflow {
				fmt.Fprintln(out, "warning: a reading was at or past the chip's input range: use a smaller shunt")
			}
			return nil
		},
	}
	cmd.Flags().Float64Var(&shunt, "shunt", 0, "the shunt resistor in ohms (required, except for the INA260)")
	cmd.Flags().StringVar(&addrFlag, "addr", "", "the chip's 7-bit address (default: the chip's first, 0x40)")
	cmd.Flags().IntVar(&channel, "channel", 0, "INA3221 channel 1-3 (default 1)")
	cmd.Flags().Float64Var(&seconds, "seconds", 0, "measure for this many seconds (mean, min, max); 0 is one reading")
	cmd.Flags().IntVar(&averaging, "averaging", 0, "conversions averaged per result: 1, 4, 16, 64, 128, 256, 512, 1024 (INA219: 1-128, powers of two); default 16")
	cmd.Flags().DurationVar(&convTime, "conversion-time", 0, "per shunt and bus conversion, the nearest the chip has: 1.1ms (default); 50us to 8.2ms")
	cmd.Flags().Float64Var(&maxCurrent, "max-current", 0, "largest current expected in amps (0.001 = 1 mA): INA219, INA228 and INA238 pick the narrowest range covering it")
	cmd.Flags().StringVar(&sda, "sda", "", "LA pin for SDA when no I2C session is open (default: i2c_sda from the pod's wiring profile)")
	cmd.Flags().StringVar(&scl, "scl", "", "LA pin for SCL when no I2C session is open (default: i2c_scl from the pod's wiring profile)")
	cmd.Flags().IntVar(&hz, "hz", 400000, "SCL rate in Hz when a session is opened")
	cmd.Flags().StringVar(&pullups, "pullups", "pod", "pod (the pod's pull-ups on LA1-LA6, which must be on: benchpod la pull 1 up) or external (the breakout's)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

type monitorResult struct {
	Chip         string  `json:"chip"`
	Addr         int     `json:"addr"`
	Channel      int     `json:"channel"`
	ShuntOhm     float64 `json:"shunt_ohm"`
	SDA          int     `json:"sda"`
	SCL          int     `json:"scl"`
	N            int     `json:"n"`
	Duration     float64 `json:"duration_s"`
	Mean         float64 `json:"current_a"`
	Min          float64 `json:"min_current_a"`
	Max          float64 `json:"max_current_a"`
	BusVoltage   float64 `json:"bus_voltage_v"`
	ShuntVoltage float64 `json:"shunt_voltage_v"`
	Overflow     bool    `json:"overflow"`
}

// sampleMonitor takes one reading, or reads once per conversion cycle (at least 5 ms) for seconds.
func sampleMonitor(ctx context.Context, mon *currentmonitor.Monitor, seconds float64) (monitorResult, error) {
	period := time.Duration(math.Max(mon.Cycle(), 0.005) * float64(time.Second))
	start := time.Now()
	var res monitorResult
	var sum, vsum float64
	for {
		r, err := mon.Read()
		if err != nil {
			return res, err
		}
		if res.N == 0 || r.Current < res.Min {
			res.Min = r.Current
		}
		if res.N == 0 || r.Current > res.Max {
			res.Max = r.Current
		}
		res.N++
		sum += r.Current
		vsum += r.BusVoltage
		res.Overflow = res.Overflow || r.Overflow
		res.Duration = time.Since(start).Seconds()
		if seconds <= 0 || res.Duration+period.Seconds() >= seconds {
			break
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(period):
		}
	}
	res.Mean, res.BusVoltage = sum/float64(res.N), vsum/float64(res.N)
	res.ShuntVoltage = res.Mean * mon.ShuntOhm
	return res, nil
}

func formatAmps(a float64) string {
	switch v := math.Abs(a); {
	case v >= 1:
		return fmt.Sprintf("%.4f A", a)
	case v >= 1e-3:
		return fmt.Sprintf("%.4f mA", a*1e3)
	default:
		return fmt.Sprintf("%.3f µA", a*1e6)
	}
}

func formatVolts(v float64) string {
	if math.Abs(v) >= 1e-3 {
		return fmt.Sprintf("%.3f mV", v*1e3)
	}
	return fmt.Sprintf("%.2f µV", v*1e6)
}

func formatWatts(w float64) string {
	if math.Abs(w) >= 1e-3 {
		return fmt.Sprintf("%.3f mW", w*1e3)
	}
	return fmt.Sprintf("%.3f µW", w*1e6)
}

func printMonitorChips(out io.Writer, asJSON bool) error {
	chips := currentmonitor.Chips()
	if asJSON {
		b, _ := json.MarshalIndent(chips, "", "  ")
		_, err := fmt.Fprintln(out, string(b))
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHIP\tADDRESSES\tLA VOLTAGE\tNOTES")
	for _, c := range chips {
		volts := make([]string, len(c.Voltages))
		for i, v := range c.Voltages {
			volts[i] = fmt.Sprintf("%g V", v)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", c.Name, c.AddrRange(), strings.Join(volts, ", "), c.Note)
	}
	return tw.Flush()
}
