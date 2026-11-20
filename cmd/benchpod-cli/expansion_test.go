package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/expansion"
)

// fakeTCA9555 answers i2c_xfer like a TCA9555 at 0x20: EXT1 an output driven high, the rest inputs.
func fakeTCA9555(pod *fakeSensorPod, session bool) {
	regs := []byte{0x05, 0x80, 0x01, 0x00, 0, 0, 0xFE, 0xFF} // in, out, polarity, config
	active := session
	pod.reply = func(req map[string]any) (any, string) {
		switch req["cmd"] {
		case "i2c_status":
			if !active {
				return map[string]any{"active": false}, ""
			}
			return map[string]any{"active": true, "sda": 5, "scl": 6, "clock_hz": 100000, "pullups": "external"}, ""
		case "i2c_start":
			active = true
			return map[string]any{"sda": req["sda"], "scl": req["scl"], "clock_hz": req["clock_hz"], "pullups": req["pullups"]}, ""
		case "i2c_stop":
			active = false
			return "i2c stopped", ""
		case "i2c_xfer":
			if int(req["addr"].(float64)) != 0x20 {
				return map[string]any{"addr": req["addr"], "ack": false, "nack_at": 0}, ""
			}
			w, _ := base64.RawURLEncoding.DecodeString(req["write"].(string))
			reg := int(w[0])
			n := int(req["read"].(float64))
			rx := regs[reg : reg+n]
			return map[string]any{"addr": 0x20, "ack": true, "rx": base64.RawURLEncoding.EncodeToString(rx)}, ""
		}
		return nil, ""
	}
}

func TestExpansionListsTheSDKChips(t *testing.T) {
	_, g := newFakeSensorPod(t)
	out, err := runBus(t, g, newExpansionCmd(g))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tca9555 (tca9535)", "I2C 0x20-0x27", "74hc595", "8 per chip", "quasi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if len(expansion.Chips()) < 11 {
		t.Fatal("chips.json lost chips")
	}
}

func TestExpansionReadsPinsOverAnOpenSession(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeTCA9555(pod, true)
	out, err := runBus(t, g, newExpansionCmd(g), "tca9555")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "tca9555 at 0x20 (SDA LA5, SCL LA6): 16 pins" {
		t.Fatalf("header %q", lines[0])
	}
	if f := strings.Fields(lines[1]); strings.Join(f, " ") != "EXT1 P00 output 1" {
		t.Fatalf("EXT1 %q", lines[1])
	}
	if f := strings.Fields(lines[3]); strings.Join(f, " ") != "EXT3 P02 input 1" {
		t.Fatalf("EXT3 %q", lines[3])
	}
	if f := strings.Fields(lines[16]); strings.Join(f, " ") != "EXT16 P17 input 1" {
		t.Fatalf("EXT16 %q", lines[16])
	}
	if pod.last("i2c_start") != nil || pod.last("i2c_stop") != nil {
		t.Fatal("the open session must be used as it is")
	}
}

func TestExpansionOpensAndClosesItsOwnSession(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeTCA9555(pod, false)
	out, err := runBus(t, g, newExpansionCmd(g), "tca9555", "--sda", "1", "--scl", "2", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if got := reqJSON(pod.last("i2c_start")); got != `{"clock_hz":400000,"cmd":"i2c_start","pullups":"pod","scl":2,"sda":1}` {
		t.Fatalf("start %s", got)
	}
	if pod.last("i2c_stop") == nil {
		t.Fatal("the run's own session must be closed")
	}
	if !strings.Contains(out, `"chip_pin": "P00"`) || !strings.Contains(out, `"out": 1`) {
		t.Fatalf("json %s", out)
	}
}

func TestExpansionRefusals(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeTCA9555(pod, true)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"ds2413"}, "unknown expander chip"},
		{[]string{"74hc595"}, "I2C expanders only"},
		{[]string{"tca9555", "--addr", "0x40"}, "0x20-0x27"},
		{[]string{"tca9555", "--addr", "0x20,0x20"}, "listed twice"},
		{[]string{"tca9555", "--sda", "1"}, "session is open"},
		{[]string{"tca9555", "--addr", "0x21"}, "no tca9555 answers at 0x21"},
		{[]string{"pcf8574", "--hz", "400000"}, "10000 to 100000"},
	}
	for _, c := range cases {
		_, err := runBus(t, g, newExpansionCmd(g), c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: got %v, want %q", c.args, err, c.want)
		}
	}
}
