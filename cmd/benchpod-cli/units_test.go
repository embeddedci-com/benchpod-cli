package main

import (
	"testing"
)

func TestParseSI(t *testing.T) {
	for _, c := range []struct {
		in   string
		unit string
		want float64
	}{
		{"1000", "Hz", 1000},
		{"1kHz", "Hz", 1000},
		{"12.5MHz", "Hz", 12.5e6},
		{"10 MHz", "Hz", 10e6},
		{"10mhz", "Hz", 0.01}, // SI: m is milli
		{"3.3V", "V", 3.3},
		{"3300mV", "V", 3.3},
		{"3.3", "V", 3.3},
		{"50uV", "V", 50e-6},
		{"50µV", "V", 50e-6},
	} {
		got, err := parseSI(c.in, c.unit)
		if err != nil || !near(got, c.want) {
			t.Errorf("parseSI(%q, %s) = %v, %v; want %v", c.in, c.unit, got, err, c.want)
		}
	}
	for _, in := range []string{"", "fast", "1xHz", "Hz", "1kV"} {
		if _, err := parseSI(in, "Hz"); err == nil {
			t.Errorf("parseSI(%q) accepted", in)
		}
	}
}

func near(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9*(1+b)
}

func TestParseLAVoltage(t *testing.T) {
	for in, want := range map[string]int{"3.3V": 3300, "3.3v": 3300, "1.8V": 1800, "1800mV": 1800, "3.3": 3300, "1.8": 1800,
		"5V": 5000, "5.0": 5000, "5000mV": 5000} { // 5 V: digital v4 (other pods refuse it)
		if got, err := parseLAVoltage(in); err != nil || got != want {
			t.Errorf("parseLAVoltage(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	// A bare number is volts: CLI 0.10 dropped "a number above 5 means mV".
	for _, in := range []string{"2.5", "1234", "4.9", "x", "", "3300", "1800"} {
		if _, err := parseLAVoltage(in); err == nil {
			t.Errorf("parseLAVoltage(%q) accepted", in)
		}
	}
}

// generate and la step send firmware 4.0's names and units (Hz, volts), from the SI flags.
func TestSIFlagsAndFirmware4Names(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	t.Setenv("BENCHPOD_TIMEOUT", "")
	addr, got := fakeLANPod(t, `{"status":"ok","data":null}`)
	request := func(args ...string) string {
		t.Helper()
		if code := run(append(args, "--connection", addr)); code != exitOK {
			t.Fatalf("%v: exit %d", args, code)
		}
		return <-got
	}
	if req := request("generate", "--waveform", "sine", "--freq", "2kHz", "--duration", "2s", "--sample-rate", "10MHz",
		"--path", "3v3", "--amplitude-v", "1", "--offset-v", "1.65"); req != `{"amplitude_v":1,"cmd":"generate","duration_ms":2000,`+
		`"freq_hz":2000,"offset_v":1.65,"path":"3v3","sample_rate_hz":10000000,"waveform":"sine"}` {
		t.Fatalf("generate %s", req)
	}
	if req := request("generate", "--waveform", "square", "--amplitude-ma", "4", "--offset-ma", "12"); req !=
		`{"amplitude_ua":4000,"cmd":"generate","duration_ms":0,"freq_hz":1000,"offset_ua":12000,"waveform":"square"}` {
		t.Fatalf("generate mA %s", req)
	}
	if req := request("la", "step", "--la", "6", "--steps", "3", "--delay", "50us"); req != `{"cmd":"la_step","delay_us":50,"la":6,"steps":3}` {
		t.Fatalf("la step %s", req)
	}
	if req := request("measure", "--waveform", "sine", "--freq", "1kHz", "--sample-rate", "1MHz", "--samples", "4"); req !=
		`{"amplitude":127,"cmd":"measure","freq_hz":1000,"offset":128,"sample_rate_hz":1000000,"samples":4,"waveform":"sine"}` {
		t.Fatalf("measure %s", req)
	}

	volts := []string{"--path", "3v3", "--amplitude-v", "1", "--offset-v", "1"}
	for _, args := range [][]string{
		{"generate", "--waveform", "sine"}, // no volts or mA: the 8-bit levels are gone
		{"generate", "--waveform", "sine", "--amplitude", "100"},
		append([]string{"generate", "--waveform", "sine", "--duration-ms", "5"}, volts...),
		append([]string{"generate", "--waveform", "sine", "--duration", "1500us"}, volts...),
		append([]string{"generate", "--waveform", "sine", "--sample-rate", "fast"}, volts...),
		append([]string{"generate", "--waveform", "sine", "--sample-rate-mhz", "1"}, volts...),
		{"la", "step", "--la", "6", "--steps", "3", "--delay", "1500ns"},
		{"la", "step", "--la", "6", "--steps", "3", "--delay-us", "50"},
		{"capture", "--sample-rate-mhz", "1"},
	} {
		if code := run(append(args, "--connection", addr)); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// One name each: --out for output files, --format for formats, --addr for addresses.
func TestOneFlagName(t *testing.T) {
	root := newRootCmd()
	for _, c := range []struct {
		path      []string
		has, gone string
	}{
		{[]string{"capture"}, "format", "output"},
		{[]string{"measure"}, "format", "output"},
		{[]string{"test"}, "format", "output"},
		{[]string{"sensor", "export"}, "out", "output"},
		{[]string{"sensor", "start"}, "addr", "address"},
		{[]string{"flash-self"}, "addr", "address"},
	} {
		cmd, _, err := root.Find(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Flags().Lookup(c.has) == nil || cmd.Flags().Lookup(c.gone) != nil {
			t.Errorf("%v: want --%s and no --%s", c.path, c.has, c.gone)
		}
	}
	if root.PersistentFlags().Lookup("out") == nil || root.PersistentFlags().Lookup("output-filename") != nil {
		t.Error("global --out")
	}
}
