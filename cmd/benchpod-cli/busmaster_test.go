package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runBus runs one command tree (i2c or la) with args against the fake pod and returns its output.
func runBus(t *testing.T, g *globalFlags, root *cobra.Command, args ...string) (string, error) {
	t.Helper()
	g.outputFilename = filepath.Join(t.TempDir(), "out.txt")
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SilenceUsage, root.SilenceErrors = true, true
	err := root.Execute()
	out, _ := os.ReadFile(g.outputFilename)
	return strings.TrimSpace(string(out)), err
}

func reqJSON(req map[string]any) string {
	b, _ := json.Marshal(req)
	return string(b)
}

func TestI2CStartSendsPinsAndPrintsTheRate(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "i2c_start" {
			return map[string]any{"sda": 1, "scl": 2, "hz": 363636, "pullups": "pod"}, ""
		}
		return nil, ""
	}
	out, err := runBus(t, g, newI2CCmd(g), "start", "--sda", "la1", "--scl", "2", "--hz", "400000")
	if err != nil {
		t.Fatal(err)
	}
	if got := reqJSON(pod.last("i2c_start")); got != `{"cmd":"i2c_start","hz":400000,"pullups":"pod","scl":2,"sda":1}` {
		t.Fatalf("request %s", got)
	}
	if out != "i2c: SDA LA1, SCL LA2 at 363.636 kHz, the pod's pull-ups" {
		t.Fatalf("output %q", out)
	}
}

func TestI2CStartPinsFromTheWiringProfile(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	sda, scl := 3, 4
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring {
		w := &podWiring{I2cSda: &sda, I2cScl: &scl}
		w.Signals = append(w.Signals, struct {
			Name string `json:"name"`
			LA   int    `json:"la"`
		}{"BUS_CLK", 9})
		return w
	}
	if _, err := runBus(t, g, newI2CCmd(g), "start", "--pullups", "external"); err != nil {
		t.Fatal(err)
	}
	if got := reqJSON(pod.last("i2c_start")); got != `{"cmd":"i2c_start","hz":100000,"pullups":"external","scl":4,"sda":3}` {
		t.Fatalf("request %s", got)
	}
	// A wiring name for a pin.
	if _, err := runBus(t, g, newI2CCmd(g), "start", "--scl", "bus_clk"); err != nil {
		t.Fatal(err)
	}
	if got := pod.last("i2c_start"); got["sda"] != 3.0 || got["scl"] != 9.0 {
		t.Fatalf("request %v", got)
	}
	if _, err := runBus(t, g, newI2CCmd(g), "start", "--sda", "3", "--scl", "la3"); err == nil || !strings.Contains(err.Error(), "both LA3") {
		t.Fatalf("err = %v", err)
	}
}

func TestI2CStartWithoutPinsIsAUsageError(t *testing.T) {
	_, g := newFakeSensorPod(t)
	_, err := runBus(t, g, newI2CCmd(g), "start")
	if err == nil || !strings.Contains(err.Error(), "--sda and --scl are required") || exitCode(err) != exitUsage {
		t.Fatalf("err = %v (exit %d)", err, exitCode(err))
	}
}

func TestI2CXferHexInHexOut(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "i2c_xfer" {
			return map[string]any{"addr": 72, "ack": true, "rx": "ARc"}, "" // 01 17
		}
		return nil, ""
	}
	out, err := runBus(t, g, newI2CCmd(g), "xfer", "0x48", "--write", "0d", "--read", "2")
	if err != nil {
		t.Fatal(err)
	}
	if got := reqJSON(pod.last("i2c_xfer")); got != `{"addr":72,"cmd":"i2c_xfer","read":2,"write":"DQ"}` {
		t.Fatalf("request %s", got)
	}
	if out != "01 17" {
		t.Fatalf("output %q", out)
	}
	// A probe: no write, no read.
	if out, err = runBus(t, g, newI2CCmd(g), "xfer", "72"); err != nil || out != "0x48: ack" {
		t.Fatalf("probe: %q, %v", out, err)
	}
	if got := reqJSON(pod.last("i2c_xfer")); got != `{"addr":72,"cmd":"i2c_xfer"}` {
		t.Fatalf("probe request %s", got)
	}
}

func TestI2CXferNackSaysWhichByte(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	at := 0
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "i2c_xfer" {
			return map[string]any{"addr": 72, "ack": false, "nack_at": at, "rx": ""}, ""
		}
		return nil, ""
	}
	for _, c := range []struct {
		at   int
		want string
	}{{0, "no device answered at 0x48"}, {2, "written byte 2 of 2"}, {3, "did not acknowledge the read"}} {
		at = c.at
		_, err := runBus(t, g, newI2CCmd(g), "xfer", "0x48", "--write", "0d 1f", "--read", "1")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("nack_at %d: err = %v, want %q", c.at, err, c.want)
		}
	}
}

func TestI2CScanPrintsHexAddresses(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	found := []int{72, 118}
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "i2c_scan" {
			return map[string]any{"from": 8, "to": 119, "found": found}, ""
		}
		return nil, ""
	}
	out, err := runBus(t, g, newI2CCmd(g), "scan")
	if err != nil || out != "0x48 0x76" {
		t.Fatalf("%q, %v", out, err)
	}
	if got := reqJSON(pod.last("i2c_scan")); got != `{"cmd":"i2c_scan"}` {
		t.Fatalf("request %s", got)
	}
	found = nil
	if out, _ = runBus(t, g, newI2CCmd(g), "scan", "--from", "0x40", "--to", "0x4f"); out != "no device answered (0x08 to 0x77)" {
		t.Fatalf("empty scan %q", out)
	}
	if got := reqJSON(pod.last("i2c_scan")); got != `{"cmd":"i2c_scan","from":64,"to":79}` {
		t.Fatalf("request %s", got)
	}
}

func TestI2CStatusAndStop(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	active := false
	pod.reply = func(req map[string]any) (any, string) {
		switch req["cmd"] {
		case "i2c_status":
			if !active {
				return map[string]any{"active": false}, ""
			}
			return map[string]any{"active": true, "sda": 5, "scl": 6, "hz": 100000, "pullups": "external"}, ""
		case "i2c_stop":
			return "i2c stopped", ""
		}
		return nil, ""
	}
	if out, err := runBus(t, g, newI2CCmd(g), "status"); err != nil || !strings.Contains(out, "no session") {
		t.Fatalf("%q, %v", out, err)
	}
	active = true
	if out, err := runBus(t, g, newI2CCmd(g), "status"); err != nil || out != "i2c: SDA LA5, SCL LA6 at 100 kHz, the board's pull-ups" {
		t.Fatalf("%q, %v", out, err)
	}
	if out, err := runBus(t, g, newI2CCmd(g), "stop"); err != nil || out != "i2c: stopped" {
		t.Fatalf("%q, %v", out, err)
	}
}

// Firmware before gateware v55 does not know the commands: say which update it needs.
func TestBusMasterOnOldFirmwareNamesTheUpdate(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] != "status" {
			return nil, "unknown cmd"
		}
		return nil, ""
	}
	for _, c := range []struct {
		root *cobra.Command
		args []string
	}{
		{newI2CCmd(g), []string{"start", "--sda", "1", "--scl", "2"}},
		{newI2CCmd(g), []string{"scan"}},
		{newLACmd(g), []string{"dir", "9", "dut_output"}},
	} {
		_, err := runBus(t, g, c.root, c.args...)
		if err == nil || !strings.Contains(err.Error(), "gateware v55 or later") || !strings.Contains(err.Error(), "benchpod flash-self") {
			t.Fatalf("%v: err = %v", c.args, err)
		}
		if exitCode(err) != exitRefused {
			t.Fatalf("%v: exit %d", c.args, exitCode(err))
		}
	}
	// The new firmware on old gateware says it itself; the CLI adds the command.
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "i2c_start" {
			return nil, "the I2C master needs gateware v55+: update the pod's firmware"
		}
		return nil, ""
	}
	_, err := runBus(t, g, newI2CCmd(g), "start", "--sda", "1", "--scl", "2")
	if err == nil || !strings.Contains(err.Error(), "needs gateware v55+") || !strings.Contains(err.Error(), "benchpod flash-self") {
		t.Fatalf("err = %v", err)
	}
}

func TestI2CPullupRefusalNamesTheCLICommand(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "i2c_start" {
			return nil, `I2C needs pull-ups: switch on LA1's with {"cmd":"la","la":1,"pullup":"on"}, or pass "pullups":"external"`
		}
		return nil, ""
	}
	_, err := runBus(t, g, newI2CCmd(g), "start", "--sda", "1", "--scl", "2")
	if err == nil || !strings.Contains(err.Error(), "benchpod la pullup 1 on") || !strings.Contains(err.Error(), "--pullups external") ||
		strings.Contains(err.Error(), `{"cmd"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestPinConflictWithTheI2CMaster(t *testing.T) {
	shown, hint := translateRefusal(`pin conflict: LA1 is in use by i2cm_sda; stop the I2C session first ({"cmd":"i2c_stop"})`)
	if shown != "pin conflict: LA1 is in use by i2cm_sda" || !strings.Contains(hint, "benchpod i2c stop") {
		t.Fatalf("%q, %q", shown, hint)
	}
}

func TestLADirSendsPins(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "la_dir" {
			return map[string]any{"pins": []any{
				map[string]any{"la": 9, "function": "none", "gpio": nil, "level": nil, "pull": nil, "dir": "dut_output"},
			}}, ""
		}
		return nil, ""
	}
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring {
		w := &podWiring{}
		w.Signals = append(w.Signals, struct {
			Name string `json:"name"`
			LA   int    `json:"la"`
		}{"DRDY", 12})
		return w
	}
	out, err := runBus(t, g, newLACmd(g), "dir", "9-10,la9,drdy", "DUT_OUTPUT")
	if err != nil {
		t.Fatal(err)
	}
	if got := reqJSON(pod.last("la_dir")); got != `{"cmd":"la_dir","dir":"dut_output","la":[9,10,12]}` {
		t.Fatalf("request %s", got)
	}
	if !strings.Contains(out, "LA9") || !strings.Contains(out, "dut_output") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := runBus(t, g, newLACmd(g), "dir", "all", "unknown"); err != nil {
		t.Fatal(err)
	}
	if got := reqJSON(pod.last("la_dir")); got != `{"cmd":"la_dir","dir":"unknown","la":"all"}` {
		t.Fatalf("request %s", got)
	}
	if _, err := runBus(t, g, newLACmd(g), "dir"); err != nil {
		t.Fatal(err)
	}
	if got := reqJSON(pod.last("la_dir")); got != `{"cmd":"la_dir"}` {
		t.Fatalf("list request %s", got)
	}
	for _, bad := range [][]string{{"dir", "9", "output"}, {"dir", "9"}, {"dir", "15", "bidir"}, {"dir", "10-9", "bidir"}} {
		if _, err := runBus(t, g, newLACmd(g), bad...); err == nil || exitCode(err) != exitUsage {
			t.Fatalf("%v: err = %v", bad, err)
		}
	}
}

func TestLAPinsShowsDirectionAndWarnings(t *testing.T) {
	pod, g := newFakeSensorPod(t)
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "la_pins" {
			return map[string]any{"levels": 2, "pins": []any{
				map[string]any{"la": 2, "function": "gpio", "gpio": "output", "level": 1,
					"pull": map[string]any{"dir": "up", "ohms": "4.7k", "on": true}, "dir": "dut_input", "warn": 3},
				map[string]any{"la": 1, "function": "i2cm_sda", "gpio": nil, "level": nil,
					"pull": map[string]any{"dir": "up", "ohms": "4.7k", "on": true}},
			}}, ""
		}
		return nil, ""
	}
	out, err := runBus(t, g, newLACmd(g), "pins")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[1], "LA1 ") || !strings.HasPrefix(lines[2], "LA2 ") {
		t.Fatalf("output:\n%s", out)
	}
	for _, want := range []string{"i2cm_sda", "unknown", "output 1", "up 4.7k on", "dut_input",
		"warning: LA2: pull-up on under a push-pull output", "warning: LA2: driven high or pulled up while the pod's target power is off"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(lines[2], " 1 ") || !strings.Contains(lines[1], " 0 ") {
		t.Fatalf("live levels:\n%s", out)
	}
	if out, _ = runBus(t, g, newLACmd(g), "pins", "--json"); !strings.Contains(out, `"warn": 3`) {
		t.Fatalf("json:\n%s", out)
	}
}

func TestParseHexBytesAndAddresses(t *testing.T) {
	for in, want := range map[string]string{"0d": "0d", "0x0d1f": "0d 1f", "0d 1f": "0d 1f", "0d,1f:aa": "0d 1f aa", "d": "0d", "0xD": "0d"} {
		b, err := parseHexBytes(in)
		if err != nil || formatHexBytes(b) != want {
			t.Fatalf("%q: %q, %v", in, formatHexBytes(b), err)
		}
	}
	if _, err := parseHexBytes("zz"); err == nil {
		t.Fatal("accepted zz")
	}
	for in, want := range map[string]int{"0x48": 72, "48h": 72, "72": 72, "0X7f": 127} {
		if n, err := parseI2CAddr(in); err != nil || n != want {
			t.Fatalf("%q: %d, %v", in, n, err)
		}
	}
	for _, bad := range []string{"0x80", "128", "-1", "x"} {
		if _, err := parseI2CAddr(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
