package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ── SI-unit flag values ─────────────────────────────────────────────────────
//
// The flags that take a physical quantity accept it with its unit (--sample-rate 10MHz,
// --duration 2s, --delay 50us, 3.3V) next to the older unit-in-the-name flags
// (--sample-rate-mhz, --duration-ms, --delay-us), which keep working. Durations use Go's
// duration syntax (time.ParseDuration).

// siPrefixes maps an SI prefix to its factor. "u" and "µ" are both micro.
var siPrefixes = map[string]float64{
	"":  1,
	"n": 1e-9,
	"u": 1e-6,
	"µ": 1e-6,
	"m": 1e-3,
	"k": 1e3,
	"K": 1e3,
	"M": 1e6,
	"G": 1e9,
}

// parseSI parses "<number>[<prefix>]<unit>" (unit matched case-insensitively, e.g. "Hz", "V")
// or a bare number, which is in the base unit. It returns the value in the base unit.
func parseSI(s, unit string) (float64, error) {
	in := strings.TrimSpace(s)
	num := in
	factor := 1.0
	if strings.HasSuffix(strings.ToLower(num), strings.ToLower(unit)) {
		num = strings.TrimSpace(num[:len(num)-len(unit)])
		// What is left is a number with an optional one-rune prefix.
		for p, f := range siPrefixes {
			if p != "" && strings.HasSuffix(num, p) {
				if rest := strings.TrimSpace(strings.TrimSuffix(num, p)); rest != "" {
					if _, err := strconv.ParseFloat(rest, 64); err == nil {
						num, factor = rest, f
						break
					}
				}
			}
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%q is not a value in %s (e.g. 10k%s)", in, unit, unit)
	}
	return v * factor, nil
}

// frequencyValue is a pflag value in Hz that takes "1kHz", "12.5MHz" or a bare number of Hz.
type frequencyValue struct{ hz *float64 }

func newFrequencyValue(def float64, p *float64) *frequencyValue {
	*p = def
	return &frequencyValue{hz: p}
}

func (f *frequencyValue) Set(s string) error {
	v, err := parseSI(s, "Hz")
	if err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("%q: a frequency cannot be negative", s)
	}
	*f.hz = v
	return nil
}

func (f *frequencyValue) String() string {
	if f.hz == nil || *f.hz == 0 {
		return "0" // pflag leaves a zero default out of the help
	}
	return formatSI(*f.hz, "Hz")
}

func (f *frequencyValue) Type() string { return "frequency" }

// formatSI writes v in the largest prefix that keeps it at or above 1 (1000 -> "1kHz").
func formatSI(v float64, unit string) string {
	for _, p := range []struct {
		prefix string
		f      float64
	}{{"G", 1e9}, {"M", 1e6}, {"k", 1e3}} {
		if math.Abs(v) >= p.f {
			return strconv.FormatFloat(v/p.f, 'g', -1, 64) + p.prefix + unit
		}
	}
	return strconv.FormatFloat(v, 'g', -1, 64) + unit
}

// parseLAVoltage reads the LA bank voltage as the pod takes it, in millivolts (1800 or 3300).
// It accepts "3.3V", "1800mV", "3.3" (volts) or "3300" (a bare number above 5 is millivolts, as
// the pod's own commands take it).
func parseLAVoltage(s string) (int, error) {
	in := strings.TrimSpace(s)
	v, err := parseSI(in, "V")
	if err != nil {
		return 0, err
	}
	if !strings.HasSuffix(strings.ToLower(in), "v") && v > 5 {
		v /= 1000 // a bare 1800 or 3300
	}
	mv := int(math.Round(v * 1000))
	if mv != 1800 && mv != 3300 {
		return 0, fmt.Errorf("LA voltage %q: the pod takes 1.8V or 3.3V", in)
	}
	return mv, nil
}

// sampleRateMHz resolves --sample-rate (any unit) and the older --sample-rate-mhz into MHz; 0
// means neither was given. Both at once is a usage error.
func sampleRateMHz(cmd *cobra.Command, hz, mhz float64) (float64, error) {
	if cmd.Flags().Changed("sample-rate") {
		if cmd.Flags().Changed("sample-rate-mhz") {
			return 0, usageErrorf("pass only one of --sample-rate or --sample-rate-mhz")
		}
		return hz / 1e6, nil
	}
	return mhz, nil
}

// wholeUnits resolves a duration flag and its older integer flag (in unit) into whole units:
// the duration flag when set (it must be a whole number of units), else the integer flag. Both
// at once is a usage error.
func wholeUnits(cmd *cobra.Command, durFlag string, d time.Duration, intFlag string, n int, unit time.Duration) (int, error) {
	if !cmd.Flags().Changed(durFlag) {
		return n, nil
	}
	if cmd.Flags().Changed(intFlag) {
		return 0, usageErrorf("pass only one of --%s or --%s", durFlag, intFlag)
	}
	if d < 0 || d%unit != 0 {
		return 0, usageErrorf("--%s %s: must be a whole number of %s", durFlag, d, unitName(unit))
	}
	return int(d / unit), nil
}

func unitName(u time.Duration) string {
	switch u {
	case time.Microsecond:
		return "microseconds"
	case time.Millisecond:
		return "milliseconds"
	}
	return u.String()
}
