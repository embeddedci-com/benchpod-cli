package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/spf13/cobra"
)

// The commands in this file speak the firmware's TCP/JSON API. Most are
// wifi-only and use g.wifiClient(...), which resolves the connection and bails
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
			ctx, cancel, client, err := g.wifiClient("ping", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := client.Command(ctx, map[string]any{"cmd": "ping"})
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

// ── status ───────────────────────────────────────────────────────────────────

func newStatusCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Firmware / WiFi info (wifi: JSON over TCP; serial: `status` console text)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			spec, err := g.resolveConnection()
			if err != nil {
				return err
			}
			if spec.IsSerial() {
				return runStatusSerial(g, spec.Device)
			}

			ctx, cancel, client, err := g.wifiClient("status", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := client.Command(ctx, map[string]any{"cmd": "status"})
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("status: open output: %w", err)
			}
			defer closeOut()
			printJSON(out, data)
			return nil
		},
	}
}

// runStatusSerial runs the firmware's `status` command over the USB serial
// console and prints its (plain-text) output.
func runStatusSerial(g *globalFlags, device string) error {
	console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(15*time.Second))
	if err != nil {
		return err
	}
	defer cancel()
	defer console.Close()

	text, err := console.Status(ctx)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	out, closeOut, err := resolveOutput(g.outputFilename)
	if err != nil {
		return fmt.Errorf("status: open output: %w", err)
	}
	defer closeOut()
	fmt.Fprintln(out, text)
	return nil
}

// ── generate ─────────────────────────────────────────────────────────────────

func newGenerateCmd(g *globalFlags) *cobra.Command {
	var waveform string
	var freq, sampleRate float64
	var amplitude, offset, durationMS int
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Start DAC waveform output",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !validWaveform(waveform) {
				return fmt.Errorf("--waveform is required (sine|square|sawtooth)")
			}
			ctx, cancel, client, err := g.wifiClient("generate", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			req := map[string]any{
				"cmd":         "generate",
				"waveform":    waveform,
				"freq":        freq,
				"amplitude":   amplitude,
				"offset":      offset,
				"duration_ms": durationMS,
			}
			if sampleRate > 0 {
				req["sample_rate_mhz"] = sampleRate
			}
			data, err := client.Command(ctx, req)
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
			log.Printf("ok")
			return nil
		},
	}
	cmd.Flags().StringVar(&waveform, "waveform", "", "waveform: sine|square|sawtooth (required)")
	cmd.Flags().Float64Var(&freq, "freq", 1000, "output frequency in Hz")
	cmd.Flags().IntVar(&amplitude, "amplitude", 127, "half-scale amplitude 0-127")
	cmd.Flags().IntVar(&offset, "offset", 128, "DC offset 0-255")
	cmd.Flags().IntVar(&durationMS, "duration-ms", 0, "duration in ms (0 = run until next command)")
	cmd.Flags().Float64Var(&sampleRate, "sample-rate-mhz", 0, "FPGA sample-clock rate in MHz (omit to auto-pick)")
	return cmd
}

// ── capture / stream ─────────────────────────────────────────────────────────

func newCaptureCmd(g *globalFlags) *cobra.Command {
	return newSampleCmd(g, "capture", "Blocking ADC snapshot")
}
func newStreamCmd(g *globalFlags) *cobra.Command {
	return newSampleCmd(g, "stream", "Async ADC capture")
}

func newSampleCmd(g *globalFlags, name, short string) *cobra.Command {
	var samples int
	var output string
	var sampleRate float64
	cmd := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !validSamples(samples) {
				return fmt.Errorf("--samples must be between 1 and 4096")
			}
			if !validOutput(output) {
				return fmt.Errorf("--output must be json, csv, or ndjson")
			}
			ctx, cancel, client, err := g.wifiClient(name, 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			req := map[string]any{"cmd": name, "samples": samples}
			if sampleRate > 0 {
				req["sample_rate_mhz"] = sampleRate
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
	cmd.Flags().Float64Var(&sampleRate, "sample-rate-mhz", 0, "ADC sample-clock rate in MHz (omit for max 12 MSPS)")
	return cmd
}

// ── measure ──────────────────────────────────────────────────────────────────

func newMeasureCmd(g *globalFlags) *cobra.Command {
	var waveform, output string
	var freq, sampleRate float64
	var amplitude, offset, samples int
	cmd := &cobra.Command{
		Use:   "measure",
		Short: "DAC + ADC loopback capture",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !validWaveform(waveform) {
				return fmt.Errorf("--waveform is required (sine|square|sawtooth)")
			}
			if !validSamples(samples) {
				return fmt.Errorf("--samples must be between 1 and 4096")
			}
			if !validOutput(output) {
				return fmt.Errorf("--output must be json, csv, or ndjson")
			}
			ctx, cancel, client, err := g.wifiClient("measure", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			req := map[string]any{
				"cmd":       "measure",
				"waveform":  waveform,
				"freq":      freq,
				"amplitude": amplitude,
				"offset":    offset,
				"samples":   samples,
			}
			if sampleRate > 0 {
				req["sample_rate_mhz"] = sampleRate
			}
			got, err := client.Samples(ctx, req)
			if err != nil {
				return fmt.Errorf("measure: %w", err)
			}
			return emitSamples("measure", g.outputFilename, got, output)
		},
	}
	cmd.Flags().StringVar(&waveform, "waveform", "", "waveform: sine|square|sawtooth (required)")
	cmd.Flags().Float64Var(&freq, "freq", 1000, "output frequency in Hz")
	cmd.Flags().IntVar(&amplitude, "amplitude", 127, "half-scale amplitude 0-127")
	cmd.Flags().IntVar(&offset, "offset", 128, "DC offset 0-255")
	cmd.Flags().IntVar(&samples, "samples", 256, "number of ADC samples (1-4096)")
	cmd.Flags().Float64Var(&sampleRate, "sample-rate-mhz", 0, "sample-clock rate in MHz (omit to auto-pick)")
	cmd.Flags().StringVar(&output, "output", "json", "output format: json|csv|ndjson")
	return cmd
}

// ── test ─────────────────────────────────────────────────────────────────────

func newTestCmd(g *globalFlags) *cobra.Command {
	var pattern, output string
	var value, samples int
	cmd := &cobra.Command{
		Use:   "test",
		Short: "Pico-side diagnostic pattern (no FPGA)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !validSamples(samples) {
				return fmt.Errorf("--samples must be between 1 and 4096")
			}
			if !validOutput(output) {
				return fmt.Errorf("--output must be json, csv, or ndjson")
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

			ctx, cancel, client, err := g.wifiClient("test", 30*time.Second)
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

// ── set-gpio ─────────────────────────────────────────────────────────────────

func newSetGPIOCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "set-gpio PIN STATE",
		Short: "Drive a digital GPIO output (PIN: 6 or gpio6; STATE: 0|1|z, z|hiz|highz = high-impedance)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			pin, err := parseGPIOPin(args[0])
			if err != nil {
				return err
			}
			state, err := parseGPIOState(args[1])
			if err != nil {
				return err
			}
			ctx, cancel, client, err := g.wifiClient("set-gpio", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := client.Command(ctx, map[string]any{"cmd": "gpio_set", "gpio": pin, "state": state})
			if err != nil {
				return fmt.Errorf("set-gpio: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("set-gpio: open output: %w", err)
			}
			defer closeOut()
			printJSON(out, data)
			return nil
		},
	}
}

// ── step-gpio ────────────────────────────────────────────────────────────────

func newStepGPIOCmd(g *globalFlags) *cobra.Command {
	var gpioArg, dirGPIOArg string
	var steps, delayUS, direction int
	cmd := &cobra.Command{
		Use:   "step-gpio",
		Short: "Pulse a GPIO N times (step/dir stepper drivers)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if gpioArg == "" {
				return fmt.Errorf("--gpio is required")
			}
			pin, err := parseGPIOPin(gpioArg)
			if err != nil {
				return err
			}
			if steps <= 0 {
				return fmt.Errorf("--steps must be positive")
			}
			if delayUS <= 0 {
				return fmt.Errorf("--delay-us must be positive")
			}

			req := map[string]any{
				"cmd":      "gpio_step",
				"gpio":     pin,
				"steps":    steps,
				"delay_us": delayUS,
			}
			if dirGPIOArg != "" {
				dirPin, err := parseGPIOPin(dirGPIOArg)
				if err != nil {
					return fmt.Errorf("--dir-gpio: %w", err)
				}
				if direction != 0 && direction != 1 {
					return fmt.Errorf("--direction must be 0 or 1")
				}
				req["dir_gpio"] = dirPin
				req["direction"] = direction
			} else if cmd.Flags().Changed("direction") {
				return fmt.Errorf("--direction requires --dir-gpio")
			}

			// The firmware busy-waits for the whole run (steps × delay_us,
			// high+low), so size the deadline to the work plus margin.
			totalUS := int64(steps) * int64(delayUS) * 2
			def := time.Duration(totalUS)*time.Microsecond + 15*time.Second
			if def < 30*time.Second {
				def = 30 * time.Second
			}

			ctx, cancel, client, err := g.wifiClient("step-gpio", def)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := client.Command(ctx, req)
			if err != nil {
				return fmt.Errorf("step-gpio: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("step-gpio: open output: %w", err)
			}
			defer closeOut()
			printJSON(out, data)
			return nil
		},
	}
	cmd.Flags().StringVar(&gpioArg, "gpio", "", "GPIO pin to pulse, e.g. 6 or gpio6 (required)")
	cmd.Flags().IntVar(&steps, "steps", 0, "number of pulses (required, positive)")
	cmd.Flags().IntVar(&delayUS, "delay-us", 0, "high/low half-period in microseconds (required, positive)")
	cmd.Flags().StringVar(&dirGPIOArg, "dir-gpio", "", "optional direction GPIO pin, driven before stepping")
	cmd.Flags().IntVar(&direction, "direction", 0, "direction level 0|1 applied to --dir-gpio")
	return cmd
}
