package main

import (
	"encoding/json"
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
	for in, want := range map[string]int{"3.3V": 3300, "3.3v": 3300, "1.8V": 1800, "1800mV": 1800, "3.3": 3300, "1.8": 1800, "3300": 3300, "1800": 1800} {
		if got, err := parseLAVoltage(in); err != nil || got != want {
			t.Errorf("parseLAVoltage(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"5V", "2.5", "1234", "x", ""} {
		if _, err := parseLAVoltage(in); err == nil {
			t.Errorf("parseLAVoltage(%q) accepted", in)
		}
	}
}

// The SI-unit flags send exactly what the older unit-in-the-name flags send.
func TestSIFlagsMatchTheOldFlags(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	t.Setenv("BENCHPOD_TIMEOUT", "")
	addr, got := fakeLANPod(t, `{"status":"ok","data":null}`)
	request := func(args ...string) map[string]any {
		t.Helper()
		if code := run(append(args, "--connection", addr)); code != exitOK {
			t.Fatalf("%v: exit %d", args, code)
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(<-got), &req); err != nil {
			t.Fatal(err)
		}
		return req
	}
	same := func(a, b map[string]any) {
		t.Helper()
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Fatalf("\n new %s\n old %s", ja, jb)
		}
	}
	same(request("generate", "--waveform", "sine", "--freq", "2kHz", "--duration", "2s", "--sample-rate", "10MHz"),
		request("generate", "--waveform", "sine", "--freq", "2000", "--duration-ms", "2000", "--sample-rate-mhz", "10"))
	same(request("la", "step", "--la", "6", "--steps", "3", "--delay", "50us"),
		request("la", "step", "--la", "6", "--steps", "3", "--delay-us", "50"))

	for _, args := range [][]string{
		{"generate", "--waveform", "sine", "--duration", "2s", "--duration-ms", "5"},
		{"generate", "--waveform", "sine", "--duration", "1500us"},
		{"generate", "--waveform", "sine", "--sample-rate", "fast"},
		{"generate", "--waveform", "sine", "--sample-rate", "1MHz", "--sample-rate-mhz", "1"},
		{"la", "step", "--la", "6", "--steps", "3", "--delay", "1500ns"},
	} {
		if code := run(append(args, "--connection", addr)); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}
