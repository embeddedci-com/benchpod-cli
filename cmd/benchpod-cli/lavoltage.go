package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── la voltage ──────────────────────────────────────────────────────────────
//
// The LA bank I/O voltage (firmware la_voltage / console la-voltage). The pod refuses every LA
// operation (capture, UART, SWD flash, pull-ups, I2C-sensor emulation) until it is set, so this
// is the first thing a bench needs. It works over the network and over USB.

const laVoltageMissing = "this firmware has no LA voltage selection; update the pod's firmware"

func newLAVoltageCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "voltage [VOLTAGE]",
		Short: "Show or set the LA bank I/O voltage (1.8V or 3.3V, the DUT's logic level)",
		Long: "Show or set the voltage of the pod's LA bank (LA1-LA14), which must match the\n" +
			"DUT's I/O voltage. The pod refuses LA capture, UART, SWD flash, pull-ups and\n" +
			"I2C-sensor emulation until it is set. 1.8V needs a v3 pod, and the pod refuses a\n" +
			"change while any LA pin is in use.\n\n" +
			"VOLTAGE is 1.8V or 3.3V (also 1800mV, 3.3, or 3300). Omit it to show the\n" +
			"current setting. Works over the network and over USB (--connection usb).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			mv := 0
			if len(args) == 1 {
				v, err := parseLAVoltage(args[0])
				if err != nil {
					return &usageError{err: err}
				}
				mv = v
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("la voltage: open output: %w", err)
			}
			defer closeOut()
			return runLAVoltage(g, mv, out, os.Stderr)
		},
	}
}

// laVoltageConsole is the part of the USB console la voltage uses.
type laVoltageConsole interface {
	LAVoltage(ctx context.Context, mv int) (int, error)
	Close() error
}

// openLAVoltageConsole opens the pod's USB console; tests replace it with a fake.
var openLAVoltageConsole = func(g *globalFlags, device string, timeout time.Duration) (laVoltageConsole, string, context.Context, context.CancelFunc, error) {
	console, path, ctx, cancel, err := g.openSerialConsole(device, timeout)
	if err != nil {
		return nil, "", nil, cancel, err
	}
	return console, path, ctx, cancel, nil
}

// laVoltageReply is the pod's JSON answer to la_voltage.
type laVoltageReply struct {
	MV         int `json:"mv"`
	ReadbackMV int `json:"readback_mv"`
}

func runLAVoltage(g *globalFlags, mv int, out, warn io.Writer) error {
	what := "la voltage"
	spec, err := g.resolveConnection()
	if err != nil {
		return err
	}
	var label string
	var rep laVoltageReply
	if spec.IsSerial() {
		console, path, ctx, cancel, err := openLAVoltageConsole(g, spec.Device, g.effectiveTimeout(15*time.Second))
		if err != nil {
			return err
		}
		defer cancel()
		defer console.Close()
		label = "the pod on " + path
		rep.MV, err = console.LAVoltage(ctx, mv)
		if err != nil {
			return laVoltageError(what, err)
		}
	} else {
		ctx, cancel, client, err := g.wifiClient(what, 15*time.Second)
		if err != nil {
			return err
		}
		defer cancel()
		label = spec.Addr
		if rep, err = laVoltageLAN(ctx, client, mv); err != nil {
			return laVoltageError(what, err)
		}
	}

	verb := ":"
	if mv != 0 {
		verb = " is now"
	}
	if rep.MV == 0 {
		fmt.Fprintf(out, "LA voltage of %s: not set (the pod refuses LA operations until it is)\n", label)
		return nil
	}
	fmt.Fprintf(out, "LA voltage of %s%s %s\n", label, verb, formatMV(rep.MV))
	if rep.ReadbackMV != 0 && rep.ReadbackMV != rep.MV {
		fmt.Fprintf(warn, "Warning: the bank's power mux reports %s, not %s\n", formatMV(rep.ReadbackMV), formatMV(rep.MV))
	}
	return nil
}

func laVoltageLAN(ctx context.Context, client *tcpclient.Client, mv int) (laVoltageReply, error) {
	req := map[string]any{"cmd": "la_voltage"}
	if mv != 0 {
		req["mv"] = mv
	}
	data, err := client.Command(ctx, req)
	if err != nil {
		return laVoltageReply{}, err
	}
	var rep laVoltageReply
	if err := json.Unmarshal(data, &rep); err != nil {
		return laVoltageReply{}, fmt.Errorf("unexpected reply: %s", strings.TrimSpace(string(data)))
	}
	return rep, nil
}

// laVoltageError says when the firmware is too old, and leaves other refusals to the pod.
func laVoltageError(what string, err error) error {
	if errors.Is(err, serialconsole.ErrUnknownCommand) {
		return errors.New(laVoltageMissing)
	}
	if pe, ok := tcpclient.AsPodError(err); ok {
		if pe.Reason == "unknown cmd" {
			return errors.New(laVoltageMissing)
		}
		return refusedError(what, pe, pe.Cmd == "")
	}
	return fmt.Errorf("%s: %w", what, err)
}

// formatMV is 3300 -> "3.3 V".
func formatMV(mv int) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", float64(mv)/1000), "0"), ".") + " V"
}
