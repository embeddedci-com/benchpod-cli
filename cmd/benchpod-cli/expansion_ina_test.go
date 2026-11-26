package main

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// fakeINA226 answers i2c_xfer like an INA226 at 0x40 across a 10 ohm shunt: 1 µA at 3.3 V.
func fakeINA226(pod *fakeSensorPod, session bool) {
	regs := map[int]int{0xFE: 0x5449, 0xFF: 0x2260, 1: 4, 2: 2640}
	ptr := 0
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
			if int(req["addr"].(float64)) != 0x40 {
				return map[string]any{"addr": req["addr"], "ack": false, "nack_at": 0}, ""
			}
			if s, ok := req["write"].(string); ok {
				w, _ := base64.RawURLEncoding.DecodeString(s)
				ptr = int(w[0])
				if len(w) == 3 {
					regs[ptr] = int(w[1])<<8 | int(w[2])
				}
			}
			var rx []byte
			for len(rx) < int(req["read"].(float64)) {
				rx = append(rx, byte(regs[ptr]>>8), byte(regs[ptr]))
				ptr++
			}
			return map[string]any{"addr": 0x40, "ack": true, "rx": base64.RawURLEncoding.EncodeToString(rx)}, ""
		}
		return nil, ""
	}
}

func TestExpansionListsTheINAChips(t *testing.T) {
	_, g := newFakeSensorPod(t)
	out, err := runBus(t, g, newExpansionCmd(g))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ina226", "I2C 0x40-0x4f", "ina3221", "I2C 0x40-0x43", "current monitor", "3.3 V, 5 V"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}

func TestCurrentMonitorReadsOverAnOpenSession(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeINA226(pod, true)
	out, err := runBus(t, g, newExpansionCmd(g), "ina226", "--shunt", "10")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ina226 at 0x40 (SDA LA5, SCL LA6), shunt 10 ohm") ||
		!strings.Contains(out, "current  1.000 µA") || !strings.Contains(out, "bus      3.3000 V") {
		t.Fatalf("output\n%s", out)
	}
	if pod.last("i2c_start") != nil || pod.last("i2c_stop") != nil {
		t.Fatal("the open session must be used as it is")
	}
}

func TestCurrentMonitorOpensItsOwnSessionAndMeasures(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeINA226(pod, false)
	out, err := runBus(t, g, newExpansionCmd(g), "ina226", "--shunt", "10", "--sda", "1", "--scl", "2",
		"--seconds", "0.05", "--averaging", "1", "--conversion-time", "140us", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if pod.last("i2c_start") == nil || pod.last("i2c_stop") == nil {
		t.Fatal("the run's own session must be opened and closed")
	}
	var doc struct {
		Monitors []monitorResult `json:"monitors"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Monitors) != 1 {
		t.Fatalf("%v %s", err, out)
	}
	res := doc.Monitors[0]
	if res.Chip != "ina226" || res.N < 2 || math.Abs(res.Mean-1e-6) > 1e-12 || math.Abs(res.BusVoltage-3.3) > 1e-9 {
		t.Fatalf("json %s", out)
	}
}

func TestCurrentMonitorRefusals(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeINA226(pod, true)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"ina999"}, "unknown expander chip"},
		{[]string{"tca9555", "--shunt", "10"}, "are for the INA current monitors"},
		{[]string{"ina226"}, "--shunt is required"},
		{[]string{"ina226", "--shunt", "10", "--addr", "0x20"}, "0x40-0x4f"},
		{[]string{"ina3221", "--shunt", "10", "--channel", "4"}, "1 to 3"},
		{[]string{"ina260", "--shunt", "10"}, "own 2 mohm"},
		{[]string{"ina226", "--shunt", "10", "--sda", "1"}, "session is open"},
		{[]string{"ina226", "--shunt", "10", "--addr", "0x41"}, "no ina226 answers at 0x41"},
		{[]string{"ina226", "--shunt", "10", "--averaging", "3"}, "averaging is one of"},
	}
	for _, c := range cases {
		_, err := runBus(t, g, newExpansionCmd(g), c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: got %v, want %q", c.args, err, c.want)
		}
	}
}
