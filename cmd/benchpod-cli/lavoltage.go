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

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── la voltage ──────────────────────────────────────────────────────────────
//
// The LA bank I/O voltage (firmware la_voltage / console la-voltage). The pod refuses every LA
// operation (capture, UART, SWD flash, pull-ups, sensor emulation) until it is set, so this
// is the first thing a bench needs. It works over the network and over USB.

const laVoltageMissing = "this firmware has no LA voltage selection; update the pod's firmware"

func newLAVoltageCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "voltage [VOLTAGE]",
		Short: "Show or set the LA bank I/O voltage (1.8V or 3.3V, 5V on digital v4: the DUT's logic level)",
		Long: "Show or set the voltage of the pod's LA bank (LA1-LA14), which must match the\n" +
			"DUT's I/O voltage. The pod refuses LA capture, UART, SWD flash, pull-ups and\n" +
			"sensor emulation until it is set. 1.8V needs a v3 pod, and the pod refuses a\n" +
			"change while any LA pin is in use.\n\n" +
			"VOLTAGE is 1.8V or 3.3V (also 1800mV, or 3.3: a bare number is volts).\n\n" +
			"5V (5.0, 5000) works on a pod with the digital board v4: the I/O reference (VREF OUT,\n" +
			"J1 pin 21), the pull-up rail, goes to 5V and outputs reach 5V through their pull-ups\n" +
			"(benchpod la pull PIN up). The output then shows VREF OUT. BP0028: 5V did not come up. Other pods refuse 5000. Details:\n" +
			"https://embeddedci.com/docs/benchpod-targets/5v-rs485#5v-io\n\n" +
			"Omit VOLTAGE to show the current setting. Works over the network, over USB (--connection usb) and through\n" +
			"embeddedci.com (--connection embeddedci:<name>).\n\n" +
			"The pod forgets the voltage on a restart. Firmware 3.10.8 and later keeps wiring\n" +
			"profiles on the pod: the profile's I/O voltage is applied when the DUT powers on and\n" +
			"(firmware 3.10.10+) on the first LA operation, and this command saves nothing. With older firmware, when this machine is signed in\n" +
			"(`benchpod login`) and the pod is registered to that account, setting it over\n" +
			"the network or embeddedci.com also saves it to the pod's wiring profile on\n" +
			"embeddedci.com, which the server applies on every connect. Over USB it is set\n" +
			"on the pod only.",
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

// laVoltageReply is the pod's JSON answer to la_voltage.
type laVoltageReply struct {
	MV         int `json:"mv"`
	ReadbackMV int `json:"readback_mv"`
	// VrefOut is the I/O reference (VREF OUT) on the digital board v4 (nil on other boards).
	VrefOut *vrefOut `json:"vref_out,omitempty"`
}

// vrefOut is VREF OUT in the la_voltage reply (digital board v4).
type vrefOut struct {
	Source string `json:"source"` // "bank" or "5v"
	SetMV  int    `json:"set_mv"`
	MV     int    `json:"mv"` // -1 without a reading
	State  string `json:"state"`
	Up     bool   `json:"up"`
}

// vrefOutText is the VREF OUT line and, when it is not ok, a warning ("" when there is none).
func vrefOutText(v *vrefOut) (line, warning string) {
	measured := "no reading"
	if v.MV >= 0 {
		measured = formatMV(v.MV)
	}
	line = fmt.Sprintf("I/O reference (VREF OUT): %s, set to %s (%s)", measured, formatMV(v.SetMV), v.State)
	switch v.State {
	case "high":
		warning = "the I/O reference (VREF OUT) is above its setting: a DUT signal above the I/O voltage lifts it through the pull " +
			"resistors. Select 5V (benchpod la voltage 5.0) or remove the signal."
	case "low":
		warning = "the I/O reference (VREF OUT) is below its setting: something on it draws too much current."
	case "short":
		warning = "the I/O reference (VREF OUT) is shorted or held down: the pull resistors are off until it is back."
	}
	return line, warning
}

func runLAVoltage(g *globalFlags, mv int, out, warn io.Writer) error {
	label, rep, err := laVoltageDo(g, mv)
	if err != nil {
		return err
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
	// In 5 V mode the bank stays at 3.3 V while the I/O voltage is 5 V.
	if rep.ReadbackMV != 0 && rep.ReadbackMV != rep.MV && !(rep.MV == 5000 && rep.ReadbackMV == 3300) {
		fmt.Fprintf(warn, "Warning: the bank's power mux reports %s, not %s\n", formatMV(rep.ReadbackMV), formatMV(rep.MV))
	}
	if rep.VrefOut != nil {
		line, w := vrefOutText(rep.VrefOut)
		fmt.Fprintln(out, line)
		if w != "" {
			fmt.Fprintln(warn, "Warning: "+w)
		}
	}
	if mv != 0 {
		fmt.Fprintln(out, "Not saved: the pod uses the I/O voltage in its wiring profile after a restart (change it there with `benchpod wiring save`, or in the Pins pane).")
	}
	return nil
}

// saveLaMVToProfile stores mv as la_mv in the wiring profile of the pod with this cloud device id
// on embeddedci.com (read-modify-write: every other field is kept). changed is false when the
// profile already had it. A variable so tests can replace it.
var saveLaMVToProfile = func(deviceID string, mv int) (changed bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	api := newServerAPI(cloudServerURL())
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		return false, fmt.Errorf("sign in: %w", err)
	}
	return api.SetDeviceWiringLaMV(ctx, cred, deviceID, mv)
}

// laVoltageDo shows (mv 0) or sets the LA voltage over the effective connection (network, USB
// or embeddedci.com) and returns a label for the pod plus its reply. Shared by la voltage, status
// and setup.
func laVoltageDo(g *globalFlags, mv int) (string, laVoltageReply, error) {
	what := "la voltage"
	spec, err := g.resolveTarget()
	if err != nil {
		return "", laVoltageReply{}, err
	}
	var rep laVoltageReply
	if spec.IsSerial() {
		// Over USB: the pod's USB network adapter (podClient says why when it is not usable).
		if _, err := g.networkTarget(what); err != nil {
			return "", rep, err
		}
	}
	ctx, cancel, pod, err := g.podClient(what, 15*time.Second)
	if err != nil {
		return "", rep, err
	}
	defer cancel()
	if rep, err = laVoltageLAN(ctx, pod.client, mv); err != nil {
		return "", rep, laVoltageError(what, err)
	}
	return pod.where(), rep, nil
}

// laVoltageLAN runs la_voltage as a JSON command, on the pod's network port or through
// embeddedci.com.
func laVoltageLAN(ctx context.Context, client podCommander, mv int) (laVoltageReply, error) {
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
	if pe, ok := tcpclient.AsPodError(err); ok {
		if isUnknownCommand(err) {
			return errors.New(laVoltageMissing)
		}
		return refusedError(what, pe)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// formatMV is 3300 -> "3.3 V".
func formatMV(mv int) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", float64(mv)/1000), "0"), ".") + " V"
}
