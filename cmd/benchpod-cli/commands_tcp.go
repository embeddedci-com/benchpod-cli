package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The commands in this file speak the firmware's TCP/JSON API. Most are
// wifi-only and use g.networkClient(...), which resolves the connection and bails
// with a clear message on a serial transport. `status` is the exception: the
// firmware exposes a `status` console command too, so it runs over either
// transport.

// ── ping ─────────────────────────────────────────────────────────────────────

func newPingCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "ping",
		Short: "Connectivity check",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel, pod, err := g.podClient("ping", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := pod.client.Command(ctx, map[string]any{"cmd": "ping"})
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("ping: open output: %w", err)
			}
			defer closeOut()
			var pong string
			if err := json.Unmarshal(data, &pong); err == nil {
				fmt.Fprintln(out, pong)
			} else {
				printJSON(out, data)
			}
			return nil
		},
	}
}

// ── generate ─────────────────────────────────────────────────────────────────

func newGenerateCmd(g *globalFlags) *cobra.Command {
	var waveform, path string
	var freq, sampleRateHz float64
	var amplitudeV, offsetV, amplitudeMA, offsetMA float64
	var duration time.Duration
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Start DAC waveform output (volts on a path, or mA on the 4-20 mA output)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validWaveform(waveform) {
				return usageErrorf("--waveform is required (sine|square|sawtooth)")
			}
			durationMS, err := wholeUnits(cmd, "duration", duration, time.Millisecond)
			if err != nil {
				return err
			}
			calibrated, err := generateCalibrated(cmd, path, amplitudeV, offsetV, amplitudeMA, offsetMA)
			if err != nil {
				return err
			}
			ctx, cancel, pod, err := g.podClient("generate", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			req := map[string]any{
				"cmd":         "generate",
				"waveform":    waveform,
				"freq_hz":     freq,
				"duration_ms": durationMS,
			}
			for k, v := range calibrated {
				req[k] = v
			}
			if sampleRateHz > 0 {
				req["sample_rate_hz"] = sampleRateHz
			}
			data, err := pod.client.Command(ctx, req)
			if err != nil {
				return fmt.Errorf("generate: %w", err)
			}
			// generate has no data payload; only write a file when one is requested.
			if g.outputFilename != "" {
				out, closeOut, oErr := resolveOutput(g.outputFilename)
				if oErr != nil {
					return fmt.Errorf("generate: open output: %w", oErr)
				}
				defer closeOut()
				printJSON(out, data)
			}
			fmt.Fprintln(os.Stderr, "generate: ok")
			return nil
		},
	}
	cmd.Flags().StringVar(&waveform, "waveform", "", "waveform: sine|square|sawtooth (required)")
	cmd.Flags().Var(newFrequencyValue(1000, &freq), "freq", "output frequency, e.g. 1kHz (a bare number is Hz)")
	cmd.Flags().StringVar(&path, "path", "", "DAC output for --amplitude-v/--offset-v: 3v3|5v|12v")
	cmd.Flags().Float64Var(&amplitudeV, "amplitude-v", 0, "peak in volts on --path, played calibrated at 16 bits")
	cmd.Flags().Float64Var(&offsetV, "offset-v", 0, "center in volts on --path (with --amplitude-v)")
	cmd.Flags().Float64Var(&amplitudeMA, "amplitude-ma", 0, "peak in mA on the 4-20 mA output, 16-bit")
	cmd.Flags().Float64Var(&offsetMA, "offset-ma", 0, "center in mA on the 4-20 mA output (with --amplitude-ma)")
	cmd.Flags().DurationVar(&duration, "duration", 0, "how long to output, e.g. 2s or 500ms (0 = run until next command)")
	cmd.Flags().Var(newFrequencyValue(0, &sampleRateHz), "sample-rate", "FPGA sample-clock rate, e.g. 10MHz (omit to auto-pick)")
	return cmd
}

// generateCalibrated turns the volts / milliamp flags into the generate fields: path,
// amplitude_v and offset_v, or amplitude_ua and offset_ua. Firmware 4.0 takes only these (the
// 8-bit levels are gone), so one set is required.
func generateCalibrated(cmd *cobra.Command, path string, ampV, offV, ampMA, offMA float64) (map[string]any, error) {
	f := cmd.Flags()
	volts := f.Changed("amplitude-v") || f.Changed("offset-v") || f.Changed("path")
	amps := f.Changed("amplitude-ma") || f.Changed("offset-ma")
	if !volts && !amps {
		return nil, usageErrorf("give volts (--path, --amplitude-v, --offset-v) or mA (--amplitude-ma, --offset-ma)")
	}
	if volts && amps {
		return nil, usageErrorf("give volts (--path, --amplitude-v, --offset-v) or mA (--amplitude-ma, --offset-ma), not both")
	}
	if amps {
		if !f.Changed("amplitude-ma") || !f.Changed("offset-ma") {
			return nil, usageErrorf("--amplitude-ma and --offset-ma go together")
		}
		if !(ampMA > 0) {
			return nil, usageErrorf("--amplitude-ma must be above 0")
		}
		return map[string]any{"amplitude_ua": math.Round(ampMA * 1000), "offset_ua": math.Round(offMA * 1000)}, nil
	}
	if path != "3v3" && path != "5v" && path != "12v" {
		return nil, usageErrorf("--path must be 3v3, 5v or 12v with --amplitude-v")
	}
	if !f.Changed("amplitude-v") || !f.Changed("offset-v") {
		return nil, usageErrorf("--amplitude-v and --offset-v go together with --path")
	}
	if !(ampV > 0) {
		return nil, usageErrorf("--amplitude-v must be above 0")
	}
	return map[string]any{"path": path, "amplitude_v": ampV, "offset_v": offV}, nil
}

// ── capture ──────────────────────────────────────────────────────────────────

func newCaptureCmd(g *globalFlags) *cobra.Command {
	return newSampleCmd(g, "capture", "Blocking ADC snapshot")
}

func newSampleCmd(g *globalFlags, name, short string) *cobra.Command {
	var samples int
	var output string
	var sampleRateHz float64
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validSamples(samples) {
				return usageErrorf("--samples must be between 1 and 4096")
			}
			if !validOutput(output) {
				return usageErrorf("--output must be json, csv, or ndjson")
			}
			ctx, cancel, client, err := g.networkClient(name, 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			req := map[string]any{"cmd": name, "samples": samples}
			if sampleRateHz > 0 {
				req["sample_rate_hz"] = sampleRateHz
			}
			got, err := client.Samples(ctx, req)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			return emitSamples(name, g.outputFilename, got, output)
		},
	}
	cmd.Flags().IntVar(&samples, "samples", 256, "number of ADC samples (1-4096)")
	cmd.Flags().StringVar(&output, "output", "json", "output format: json|csv|ndjson")
	cmd.Flags().Var(newFrequencyValue(0, &sampleRateHz), "sample-rate", "ADC sample rate, e.g. 100kHz (omit for the maximum, 400 kSPS)")
	return cmd
}

// ── measure ──────────────────────────────────────────────────────────────────

func newMeasureCmd(g *globalFlags) *cobra.Command {
	var waveform, output string
	var freq, sampleRateHz float64
	var amplitude, offset, samples int
	cmd := &cobra.Command{
		Use:   "measure",
		Short: "DAC + ADC loopback capture",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validWaveform(waveform) {
				return usageErrorf("--waveform is required (sine|square|sawtooth)")
			}
			if !validSamples(samples) {
				return usageErrorf("--samples must be between 1 and 4096")
			}
			if !validOutput(output) {
				return usageErrorf("--output must be json, csv, or ndjson")
			}
			ctx, cancel, client, err := g.networkClient("measure", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			req := map[string]any{
				"cmd":       "measure",
				"waveform":  waveform,
				"freq_hz":   freq,
				"amplitude": amplitude,
				"offset":    offset,
				"samples":   samples,
			}
			if sampleRateHz > 0 {
				req["sample_rate_hz"] = sampleRateHz
			}
			got, err := client.Samples(ctx, req)
			if err != nil {
				return fmt.Errorf("measure: %w", err)
			}
			return emitSamples("measure", g.outputFilename, got, output)
		},
	}
	cmd.Flags().StringVar(&waveform, "waveform", "", "waveform: sine|square|sawtooth (required)")
	cmd.Flags().Var(newFrequencyValue(1000, &freq), "freq", "output frequency, e.g. 1kHz (a bare number is Hz)")
	cmd.Flags().IntVar(&amplitude, "amplitude", 127, "half-scale amplitude 0-127")
	cmd.Flags().IntVar(&offset, "offset", 128, "DC offset 0-255")
	cmd.Flags().IntVar(&samples, "samples", 256, "number of ADC samples (1-4096)")
	cmd.Flags().Var(newFrequencyValue(0, &sampleRateHz), "sample-rate", "sample-clock rate, e.g. 10MHz (omit to auto-pick)")
	cmd.Flags().StringVar(&output, "output", "json", "output format: json|csv|ndjson")
	return cmd
}

// ── test ─────────────────────────────────────────────────────────────────────

func newTestCmd(g *globalFlags) *cobra.Command {
	var pattern, output string
	var value, samples int
	cmd := &cobra.Command{
		Use:   "test",
		Short: "MCU-side diagnostic pattern (no FPGA)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validSamples(samples) {
				return usageErrorf("--samples must be between 1 and 4096")
			}
			if !validOutput(output) {
				return usageErrorf("--output must be json, csv, or ndjson")
			}

			// Mirror the firmware default: const when --value is given without
			// --pattern, else sine.
			req := map[string]any{"cmd": "test", "samples": samples}
			switch {
			case cmd.Flags().Changed("pattern"):
				req["pattern"] = pattern
			case cmd.Flags().Changed("value"):
				req["pattern"] = "const"
			default:
				req["pattern"] = "sine"
			}
			if cmd.Flags().Changed("value") {
				req["value"] = value
			}

			ctx, cancel, client, err := g.networkClient("test", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			got, err := client.Samples(ctx, req)
			if err != nil {
				return fmt.Errorf("test: %w", err)
			}
			return emitSamples("test", g.outputFilename, got, output)
		},
	}
	cmd.Flags().StringVar(&pattern, "pattern", "", "pattern: sine|counter|ramp|const (default sine, or const when --value is set)")
	cmd.Flags().IntVar(&value, "value", 255, "constant byte value 0-255 (used by const pattern)")
	cmd.Flags().IntVar(&samples, "samples", 256, "number of samples (1-4096)")
	cmd.Flags().StringVar(&output, "output", "json", "output format: json|csv|ndjson")
	return cmd
}

// ── la (logic-analyzer pins: pull-ups + step pulses) ─────────────────────────

func newLACmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "la",
		Short: "Logic-analyzer pin control (bank voltage, pull resistors, step pulses, pin list and directions)",
	}
	cmd.AddCommand(newLAVoltageCmd(g), newLAPullCmd(g), newLAStatusCmd(g), newLAStepCmd(g),
		newLAPinsCmd(g), newLADirCmd(g))
	return cmd
}

func newLAPullCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "pull PIN up|down|off",
		Short: "Pull an LA pin up or down, or switch its pull resistor off (PIN: 1-14 or la1)",
		Long: "Pull an LA pin up or down, or switch its pull resistor off. The pod picks the resistor\n" +
			"and reports its value (ohms). Which pins have one and which way each can pull depends on the\n" +
			"board: today LA1-LA6 pull up, LA7/LA8 pull down and LA9-LA14 have none; the reply's dirs\n" +
			"lists them. Another direction is refused (BP0024), and up or down on a pin without a resistor\n" +
			"(BP0023); off works on every pin. The resistors go to the LA bank voltage.\n\n" +
			"Digital board v3r4: every pin, LA1-LA14, pulls up or down with a 10k, a 2.2k or both (about\n" +
			"1.8k). The pod picks the strength from the pin's function (2.2k for I2C, one-wire UART, open\n" +
			"drain and outputs in 5 V mode, else 10k); the reply's strength says which. The pull-ups go to\n" +
			"J1 pin 21, so they work at 1.8, 3.3 and 5 V; while pin 21 is down they are off (BP0029).",
		Example: "  benchpod la pull 1 up\n  benchpod la pull 7 down\n  benchpod la pull 7 off",
		Args:    cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			pin, err := parseLAPin(args[0])
			if err != nil {
				return usageErrorf("%v", err)
			}
			want := strings.ToLower(strings.TrimSpace(args[1]))
			if want != "up" && want != "down" && want != "off" {
				return usageErrorf("invalid direction %q (use up, down or off)", args[1])
			}
			ctx, cancel, pod, err := g.podClient("la pull", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := setLAPull(ctx, pod.client, pin, want)
			if err != nil {
				return fmt.Errorf("la pull: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("la pull: open output: %w", err)
			}
			defer closeOut()
			printJSON(out, data)
			return nil
		},
	}
}

func newLAStatusCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status [PIN]",
		Short: "Report LA pull-up state (one PIN, or the bitmask of all if omitted)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			req := map[string]any{"cmd": "la_pull"}
			if len(args) == 1 {
				pin, err := parseLAPin(args[0])
				if err != nil {
					return err
				}
				req["la"] = pin
			}
			ctx, cancel, pod, err := g.podClient("la status", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := pod.client.Command(ctx, req)
			if err != nil {
				return fmt.Errorf("la status: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("la status: open output: %w", err)
			}
			defer closeOut()
			printJSON(out, data)
			return nil
		},
	}
}

func newLAStepCmd(g *globalFlags) *cobra.Command {
	var laArg, dirLAArg string
	var steps, direction int
	var delay time.Duration
	cmd := &cobra.Command{
		Use:   "step",
		Short: "Pulse an LA pin N times (step/dir stepper drivers)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if laArg == "" {
				return usageErrorf("--la is required")
			}
			pin, err := parseLAPin(laArg)
			if err != nil {
				return err
			}
			if steps <= 0 {
				return usageErrorf("--steps must be positive")
			}
			delayUS, err := wholeUnits(cmd, "delay", delay, time.Microsecond)
			if err != nil {
				return err
			}
			if delayUS <= 0 {
				return usageErrorf("--delay must be positive")
			}

			req := map[string]any{
				"cmd":      "la_step",
				"la":       pin,
				"steps":    steps,
				"delay_us": delayUS,
			}
			if dirLAArg != "" {
				dirPin, err := parseLAPin(dirLAArg)
				if err != nil {
					return usageErrorf("--dir-la: %w", err)
				}
				if direction != 0 && direction != 1 {
					return usageErrorf("--direction must be 0 or 1")
				}
				req["dir_la"] = dirPin
				req["direction"] = direction
			} else if cmd.Flags().Changed("direction") {
				return usageErrorf("--direction requires --dir-la")
			}

			// The FPGA runs the train autonomously, but size the deadline to the
			// work (steps × delay_us, high+low) plus margin so a slow link is fine.
			totalUS := int64(steps) * int64(delayUS) * 2
			def := time.Duration(totalUS)*time.Microsecond + 15*time.Second
			if def < 30*time.Second {
				def = 30 * time.Second
			}

			ctx, cancel, pod, err := g.podClient("la step", def)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := pod.client.Command(ctx, req)
			if err != nil {
				return fmt.Errorf("la step: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("la step: open output: %w", err)
			}
			defer closeOut()
			printJSON(out, data)
			return nil
		},
	}
	cmd.Flags().StringVar(&laArg, "la", "", "LA pin to pulse, e.g. 6 or la6 (required)")
	cmd.Flags().IntVar(&steps, "steps", 0, "number of pulses (required, positive)")
	cmd.Flags().DurationVar(&delay, "delay", 0, "high/low half-period, e.g. 50us or 1ms (required)")
	cmd.Flags().StringVar(&dirLAArg, "dir-la", "", "optional direction LA pin, driven before stepping")
	cmd.Flags().IntVar(&direction, "direction", 0, "direction level 0|1 applied to --dir-la")
	return cmd
}
