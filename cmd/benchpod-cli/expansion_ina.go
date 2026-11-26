package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/currentmonitor"
	"github.com/embeddedci-com/benchpod-cli/internal/expansion"
)

// INA current monitors through `benchpod expansion ina226 ...`: external INA breakouts for µA
// currents on the pod's I2C master. The SDK's bp.expansion("ina226", ...) with measure_power(monitor=)
// and the MCP server's expansion tool are the reference (the chip table and register model are the
// same); the CLI keeps no state between runs.

// monitorFlags are the INA-only flags of `benchpod expansion`.
type monitorFlags struct {
	shunt, maxCurrent, seconds float64
	channel, averaging         int
	convTime                   time.Duration
}

// monitorJob is one INA reading: an address and channel with the settings that apply to it.
type monitorJob struct {
	addr, channel, averaging int
	shunt, maxCurrent        float64
	convTime                 time.Duration
	name                     string
	sda, scl                 int // from the profile entry; 0 = not set
}

// monitorJobs builds the readings for chip. addrs are the --addr addresses when addrGiven, else
// the chip's first. The profile's monitors entries for the chip supply the settings the flags leave
// out: with --addr the entries at those addresses, without it every entry. An address no entry
// covers (or a profile without entries) is read from the flags alone.
func monitorJobs(chip expansion.Chip, addrs []int, addrGiven bool, f monitorFlags, w *podWiring) ([]monitorJob, error) {
	entries := w.monitorsFor(chip)
	var jobs []monitorJob
	add := func(addr int, e *profileMonitor) {
		j := monitorJob{addr: addr, channel: 1, shunt: f.shunt, maxCurrent: f.maxCurrent, averaging: f.averaging, convTime: f.convTime}
		if e != nil {
			j.channel, j.averaging, j.maxCurrent = max(e.Channel, 1), e.Averaging, e.MaxCurrent
			j.convTime = time.Duration(math.Round(e.ConversionTime * 1e9))
			if e.ShuntOhm != nil {
				j.shunt = *e.ShuntOhm
			}
			j.name = e.displayName(chip)
			if e.SDA != nil && e.SCL != nil {
				j.sda, j.scl = *e.SDA, *e.SCL
			}
			// explicit flags win over the entry
			if f.shunt != 0 {
				j.shunt = f.shunt
			}
			if f.averaging != 0 {
				j.averaging = f.averaging
			}
			if f.convTime != 0 {
				j.convTime = f.convTime
			}
			if f.maxCurrent != 0 {
				j.maxCurrent = f.maxCurrent
			}
		}
		if f.channel != 0 {
			j.channel = f.channel
		}
		jobs = append(jobs, j)
	}
	if !addrGiven && len(entries) > 0 {
		for i := range entries {
			add(entries[i].Addr, &entries[i])
		}
	} else {
		for _, a := range addrs {
			found := false
			for i := range entries {
				if entries[i].Addr == a {
					add(a, &entries[i])
					found = true
				}
			}
			if !found {
				add(a, nil)
			}
		}
	}
	for _, j := range jobs {
		if chip.InternalOhm == 0 && j.shunt <= 0 {
			return nil, usageErrorf("--shunt is required for the %s: the shunt resistor in ohms, e.g. --shunt 10 (or shunt_ohm in a monitors entry of the wiring profile)", chip.Name)
		}
		if chip.InternalOhm > 0 && f.shunt != 0 && f.shunt != chip.InternalOhm {
			return nil, usageErrorf("the %s has its own %g mohm shunt: leave --shunt out", chip.Name, chip.InternalOhm*1000)
		}
		if j.channel > chip.Channels {
			return nil, usageErrorf("--channel is 1 to %d for the %s", chip.Channels, chip.Name)
		}
	}
	return jobs, nil
}

// runMonitors reads each INA monitor job and prints it (one reading, or seconds of them).
func runMonitors(ctx context.Context, out io.Writer, chip expansion.Chip, jobs []monitorJob, seconds float64,
	st i2cSession, asJSON bool, xfer currentmonitor.Xfer) error {
	var results []monitorResult
	for _, j := range jobs {
		shunt := j.shunt
		if chip.InternalOhm > 0 {
			shunt = chip.InternalOhm
		}
		mon := currentmonitor.New(chip, j.addr, j.channel, shunt, xfer)
		if err := mon.Probe(); err != nil {
			return err
		}
		if err := mon.Configure(true, j.averaging, j.convTime.Seconds(), j.maxCurrent*shunt); err != nil {
			return usageErrorf("%v", err)
		}
		res, err := sampleMonitor(ctx, mon, seconds)
		if err != nil {
			return err
		}
		res.Chip, res.Name, res.Addr, res.Channel, res.ShuntOhm, res.SDA, res.SCL = chip.Name, j.name, j.addr, j.channel, shunt, st.SDA, st.SCL
		results = append(results, res)
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{"chip": chip.Name, "monitors": results}, "", "  ")
		_, err := fmt.Fprintln(out, string(b))
		return err
	}
	for _, res := range results {
		label := res.Chip
		if res.Name != "" {
			label += " (" + res.Name + ")"
		}
		fmt.Fprintf(out, "%s at 0x%02x (SDA LA%d, SCL LA%d), shunt %g ohm\n", label, res.Addr, st.SDA, st.SCL, res.ShuntOhm)
		if res.N == 1 {
			fmt.Fprintf(out, "current  %s\nbus      %.4f V\nshunt    %s\npower    %s\n", formatAmps(res.Mean), res.BusVoltage,
				formatVolts(res.Mean*res.ShuntOhm), formatWatts(res.Mean*res.BusVoltage))
		} else {
			fmt.Fprintf(out, "%d readings in %.2f s\nmean     %s\nmin      %s\nmax      %s\nbus      %.4f V\ncharge   %.3g C\n",
				res.N, res.Duration, formatAmps(res.Mean), formatAmps(res.Min), formatAmps(res.Max), res.BusVoltage, res.Mean*res.Duration)
		}
		if res.Overflow {
			fmt.Fprintln(out, "warning: a reading was at or past the chip's input range: use a smaller shunt")
		}
	}
	return nil
}

type monitorResult struct {
	Chip         string  `json:"chip"`
	Name         string  `json:"name,omitempty"`
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
