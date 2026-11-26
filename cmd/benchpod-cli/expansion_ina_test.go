package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/expansion"
)

// fakeINA226 answers i2c_xfer like an INA226 at 0x40 across a 10 ohm shunt: 1 µA at 3.3 V.
func fakeINA226(pod *fakeSensorPod, session bool, addrs ...int) {
	if len(addrs) == 0 {
		addrs = []int{0x40}
	}
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
			if a := int(req["addr"].(float64)); !slices.Contains(addrs, a) {
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
			return map[string]any{"addr": req["addr"], "ack": true, "rx": base64.RawURLEncoding.EncodeToString(rx)}, ""
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

// withProfile makes loadPodWiring return the profile JSON for the test.
func withMonitorProfile(t *testing.T, profile string) {
	t.Helper()
	old := loadPodWiring
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring {
		var w podWiring
		if err := json.Unmarshal([]byte(profile), &w); err != nil {
			t.Fatal(err)
		}
		return &w
	}
	t.Cleanup(func() { loadPodWiring = old })
}

const monitorProfile = `{"i2c_sda": 1, "i2c_scl": 2, "monitors": [
  {"chip": "ina226", "addr": 64, "shunt_ohm": 10, "name": "dut", "sda": 1, "scl": 2, "averaging": 1, "conversion_time": 0.00014, "max_current": 0.001},
  {"chip": "ina226", "addr": 65, "shunt_ohm": 100, "name": "radio"},
  {"chip": "ina228", "addr": 66, "shunt_ohm": 1}]}`

func TestCurrentMonitorFromProfileReadsEveryMatchingEntry(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeINA226(pod, false, 0x40, 0x41)
	withMonitorProfile(t, monitorProfile)
	out, err := runBus(t, g, newExpansionCmd(g), "ina226")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ina226 (dut) at 0x40", "shunt 10 ohm", "ina226 (radio) at 0x41", "shunt 100 ohm"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if st := pod.last("i2c_start"); st == nil || st["sda"] != float64(1) || st["scl"] != float64(2) {
		t.Fatalf("the entry's SDA/SCL must open the session: %v", st)
	}
}

func TestCurrentMonitorFromProfileAddrAndFlagOverride(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	fakeINA226(pod, true, 0x40, 0x41)
	withMonitorProfile(t, monitorProfile)
	out, err := runBus(t, g, newExpansionCmd(g), "ina226", "--addr", "0x41", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Monitors []monitorResult `json:"monitors"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Monitors) != 1 {
		t.Fatalf("%v %s", err, out)
	}
	if m := doc.Monitors[0]; m.Name != "radio" || m.Addr != 0x41 || m.ShuntOhm != 100 {
		t.Fatalf("json %s", out)
	}
	out, err = runBus(t, g, newExpansionCmd(g), "ina226", "--addr", "0x41", "--shunt", "10")
	if err != nil || !strings.Contains(out, "ina226 (radio) at 0x41 (SDA LA5, SCL LA6), shunt 10 ohm") {
		t.Fatalf("explicit --shunt must win: %v\n%s", err, out)
	}
	// an address no entry covers is read from the flags alone
	if _, err = runBus(t, g, newExpansionCmd(g), "ina226", "--addr", "0x40,0x41"); err != nil {
		t.Fatal(err)
	}
	if _, err = runBus(t, g, newExpansionCmd(g), "ina228", "--addr", "0x43"); err == nil || !strings.Contains(err.Error(), "--shunt is required") ||
		!strings.Contains(err.Error(), "wiring profile") {
		t.Fatalf("got %v", err)
	}
}

func TestMonitorsInTheProfileModel(t *testing.T) {
	var w podWiring
	if err := json.Unmarshal([]byte(monitorProfile), &w); err != nil || len(w.Monitors) != 3 || w.Monitors[1].Name != "radio" {
		t.Fatalf("%v %+v", err, w.Monitors)
	}
	ina3221, _ := expansion.Lookup("ina3221")
	if n := (profileMonitor{Chip: "ina3221", Addr: 0x40, Channel: 2}).displayName(ina3221); n != "ina3221@0x40.2" {
		t.Fatal(n)
	}
}

func TestValidateMonitors(t *testing.T) {
	ok := func(s string) map[string]any {
		var p map[string]any
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := validateMonitors(ok(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := validateMonitors(ok(`{"monitors": []}`)); err != nil {
		t.Fatal(err)
	}
	if err := validateMonitors(ok(monitorProfile)); err != nil {
		t.Fatal(err)
	}
	if err := validateMonitors(ok(`{"monitors": [{"chip":"ina260","addr":64,"shunt_ohm":0.002},{"chip":"ina3221","addr":65,"shunt_ohm":1,"channel":3}]}`)); err != nil {
		t.Fatal(err)
	}
	many := strings.Repeat(`{"chip":"ina226","addr":64,"shunt_ohm":1},`, 9)
	cases := []struct{ profile, want string }{
		{`{"monitors": {}}`, "array"},
		{`{"monitors": [` + strings.TrimSuffix(many, ",") + `]}`, "at most 8"},
		{`{"monitors": [{"chip":"tca9555","addr":64,"shunt_ohm":1}]}`, "INA current monitor"},
		{`{"monitors": [{"chip":"ina226","addr":32,"shunt_ohm":1}]}`, "addr must be"},
		{`{"monitors": [{"chip":"ina3221","addr":68,"shunt_ohm":1}]}`, "addr must be"},
		{`{"monitors": [{"chip":"ina226","addr":64}]}`, "shunt_ohm is required"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":0}]}`, "shunt_ohm must be"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":2e6}]}`, "shunt_ohm must be"},
		{`{"monitors": [{"chip":"ina260","addr":64,"shunt_ohm":1}]}`, "own 2 mohm"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"channel":2}]}`, "ina3221 only"},
		{`{"monitors": [{"chip":"ina3221","addr":64,"shunt_ohm":1,"channel":4}]}`, "channel must be"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"name":"EXT"}]}`, "not be"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"name":"1bad"}]}`, "name must"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"name":"a"},{"chip":"ina226","addr":65,"shunt_ohm":1,"name":"A"}]}`, "used twice"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1},{"chip":"ina226","addr":64,"shunt_ohm":2}]}`, "listed twice"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"averaging":2}]}`, "averaging for the ina226"},
		{`{"monitors": [{"chip":"ina219","addr":64,"shunt_ohm":1,"averaging":1024}]}`, "averaging for the ina219"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"conversion_time":0.02}]}`, "conversion_time"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"max_current":0}]}`, "max_current"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"bogus":1}]}`, "unknown key bogus"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"sda":15,"scl":2}]}`, "sda must be"},
		{`{"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1,"sda":2,"scl":2}]}`, "each needs its own pin"},
		{`{"i2c_sda":1,"i2c_scl":2,"monitors": [{"chip":"ina226","addr":64,"shunt_ohm":1},{"chip":"ina226","addr":65,"shunt_ohm":1,"sda":5,"scl":6}]}`, "one I2C master"},
	}
	for _, c := range cases {
		if err := validateMonitors(ok(c.profile)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: got %v, want %q", c.profile, err, c.want)
		}
	}
}
