package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// The ADC input range (firmware 3.9.5+, analog module v3r4 and later): relay K5 swaps the ADC
// input divider between normal (÷12, ±32.8 V) and low (÷2, ±5.46 V, about 6x the resolution).
// The range lives on the pod until it is changed, the analog path goes off or to cal2, or the pod
// restarts.

const adcRangeMissing = "this firmware has no ADC range; update the pod's firmware (3.9.5 or later)"

func newADCRangeCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "adc-range [show|normal|low|test]",
		Short: "Show or switch the ADC input range (normal ±32.8 V, low ±5.46 V), or self-test it",
		Long: "Show the ADC input range, switch it, or run its self-test (firmware 3.9.5+).\n\n" +
			"normal is the ÷12 divider: ±32.8 V, about 1 mV per count. low is ÷2: ±5.46 V, about\n" +
			"167 µV per count, on analog module v3r4 and later. The low range's input is 1.82 MΩ:\n" +
			"use 1x leads, not 10x scope probes. It applies to ext, cal1 and current_in, and stays\n" +
			"until it is changed, the analog path goes off or to cal2, or the pod restarts.\n\n" +
			"test drives the 5 V output into the cal1 loopback at 1 V and 3.5 V and reads both\n" +
			"ranges: disconnect anything on the 5 V output first. It exits non-zero when it fails.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"show", "normal", "low", "test"},
		RunE: func(cmd *cobra.Command, args []string) error {
			action := "show"
			if len(args) == 1 {
				action = args[0]
			}
			switch action {
			case "show", "normal", "low", "test":
			default:
				return fmt.Errorf("adc-range: use show, normal, low or test (got %q)", action)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			return runADCRange(g, action, out)
		},
	}
}

type adcRangeReply struct {
	Range       string `json:"range"`
	Supported   bool   `json:"supported"`
	FullscaleMV int    `json:"fullscale_mv"`
	NormalMV    int    `json:"normal_fullscale_mv"`
	LowMV       *int   `json:"low_fullscale_mv"`
}

type adcRangeTestReply struct {
	Pass   bool `json:"pass"`
	Points []struct {
		SetMV    int  `json:"set_mv"`
		NormalMV int  `json:"normal_mv"`
		LowMV    int  `json:"low_mv"`
		OK       bool `json:"ok"`
	} `json:"points"`
}

func runADCRange(g *globalFlags, action string, out io.Writer) error {
	what := "adc-range"
	if action != "show" {
		what += " " + action
	}
	ctx, cancel, t, err := g.podClient(what, 15*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	return adcRange(ctx, t.client, t.label, action, what, out)
}

func adcRange(ctx context.Context, c podCommander, via, action, what string, out io.Writer) error {
	req := map[string]any{"cmd": "adc_range"}
	switch action {
	case "normal", "low":
		req["range"] = action
	case "test":
		req["test"] = true
	}
	data, err := c.Command(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "unknown cmd") {
			return errors.New(adcRangeMissing)
		}
		if pe, ok := tcpclient.AsPodError(err); ok {
			return refusedError(what, pe, false)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	if action == "test" {
		var rep adcRangeTestReply
		if err := json.Unmarshal(data, &rep); err != nil || len(rep.Points) == 0 {
			return fmt.Errorf("%s: unexpected reply %s", what, strings.TrimSpace(string(data)))
		}
		fmt.Fprintf(out, "ADC range self-test of the pod at %s (cal1 loopback, 5 V output):\n", via)
		for _, p := range rep.Points {
			verdict := "ok"
			if !p.OK {
				verdict = "FAIL"
			}
			fmt.Fprintf(out, "  set %5d mV  normal %5d mV  low %5d mV  %s\n", p.SetMV, p.NormalMV, p.LowMV, verdict)
		}
		if !rep.Pass {
			return fmt.Errorf("%s: the two ranges disagree: check that nothing is connected to the 5 V output; "+
				"a relay that does not switch reads about 6x off", what)
		}
		fmt.Fprintln(out, "passed")
		return nil
	}
	var rep adcRangeReply
	if err := json.Unmarshal(data, &rep); err != nil || rep.Range == "" {
		return fmt.Errorf("%s: unexpected reply %s", what, strings.TrimSpace(string(data)))
	}
	fmt.Fprintf(out, "ADC range of the pod at %s: %s, ±%s\n", via, rep.Range, volts(rep.FullscaleMV))
	if !rep.Supported {
		fmt.Fprintln(out, "This board has no low range (it needs analog module v3r4 or later).")
	} else if rep.LowMV != nil && action == "show" {
		fmt.Fprintf(out, "Ranges: normal ±%s, low ±%s (1x leads only: 1.82 MΩ input)\n", volts(rep.NormalMV), volts(*rep.LowMV))
	}
	return nil
}

// volts prints millivolts as volts with two decimals: 32856 -> "32.86 V".
func volts(mv int) string { return fmt.Sprintf("%.2f V", float64(mv)/1000) }
