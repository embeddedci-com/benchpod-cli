package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/embeddedci-com/benchpod-cli/internal/progdev"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/detect"
)

// detectPod is a scripted pod for detect: it answers the commands detect sends from fields and
// records everything it was sent.
type detectPod struct {
	status     string
	efuse1     bool
	owners     map[int]string
	swd        map[bool]string // by "has a read list": the dap_start op id reply
	jtag       string
	usb        string
	progID     string // prog_flash info result id ("" = no answer)
	progStyle  int    // >= 0: the chip answers only info sent with flags & 0x73 == progStyle
	infoFlags  int    // the flags of the last info
	espChip    string
	espRunning int
	sent       []map[string]any
}

func newDetectPod() *detectPod {
	return &detectPod{status: okStatus("4.0.5", "v3r2", 19), efuse1: true, owners: map[int]string{}, usb: `{"mode":"device"}`, progStyle: -1}
}

func (p *detectPod) count(cmd string) int {
	n := 0
	for _, c := range p.sent {
		if c["cmd"] == cmd {
			n++
		}
	}
	return n
}

func (p *detectPod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	p.sent = append(p.sent, req)
	switch req["cmd"] {
	case "status":
		return json.RawMessage(p.status), nil
	case "target_status":
		return json.RawMessage(fmt.Sprintf(`{"efuse1":{"enabled":%v,"fault":false},"efuse2":{"enabled":false,"fault":false}}`, p.efuse1)), nil
	case "la_pins":
		var pins []string
		for i := 1; i <= 14; i++ {
			pins = append(pins, fmt.Sprintf(`{"la":%d,"function":"gpio","owner":%q}`, i, p.owners[i]))
		}
		return json.RawMessage(`{"pins":[` + strings.Join(pins, ",") + `]}`), nil
	case "dap_start":
		if req["op"] != "id" {
			return nil, fmt.Errorf("detect must only send op id")
		}
		if _, ok := req["tck"]; ok {
			return json.RawMessage(p.jtag), nil
		}
		_, withReads := req["read"]
		return json.RawMessage(p.swd[withReads]), nil
	case "usb_host":
		return json.RawMessage(p.usb), nil
	case "la_voltage":
		return json.RawMessage(`{"mv":3300}`), nil
	case "fpga_image":
		return json.RawMessage(`{"name":"deep"}`), nil
	case "prog_flash":
		if req["op"] == "info" {
			p.infoFlags, _ = req["flags"].(int)
			return json.RawMessage(`{"running":true}`), nil
		}
		if p.progID == "" || (p.progStyle >= 0 && p.infoFlags&0x73 != p.progStyle) {
			return json.RawMessage(`{"running":false,"phase":"enter","result":"no answer"}`), nil
		}
		return json.RawMessage(fmt.Sprintf(`{"running":false,"phase":"done","id":%q,"result":"ok"}`, p.progID)), nil
	case "esp_flash":
		if req["op"] == "sync" {
			return json.RawMessage(`{"running":true}`), nil
		}
		if p.espRunning > 0 {
			p.espRunning--
			return json.RawMessage(`{"running":true,"phase":"sync"}`), nil
		}
		return json.RawMessage(fmt.Sprintf(`{"running":false,"phase":"done","chip":%q,"result":"ok"}`, p.espChip)), nil
	}
	return nil, fmt.Errorf("unexpected command %v", req["cmd"])
}

const (
	swdFirst  = `{"port":"swd","found":true,"dpidr":"0x2ba01477","targetid":null,"apidr":"0x24770011","cpuid":"0x410fc241","reads":[]}`
	swdSecond = `{"port":"swd","found":true,"dpidr":"0x2ba01477","targetid":null,"apidr":"0x24770011","cpuid":"0x410fc241","reads":[{"addr":"0xe0042000","value":"0x10006421"},{"addr":"0x10000100","value":null}]}`
	jtagReply = `{"port":"jtag","found":true,"idcodes":["0x4ba00477","0x06421041"]}`
)

func ip(n int) *int { return &n }

func benchWiring() *podWiring {
	w := &podWiring{SwdSwclk: ip(11), SwdSwdio: ip(12), Efuse: ip(1)}
	return w
}

func addSignal(w *podWiring, name string, la int) {
	w.Signals = append(w.Signals, struct {
		Name string `json:"name"`
		LA   int    `json:"la"`
	}{name, la})
}

func shortPolls(t *testing.T) {
	a, b := progPollInterval, espPollInterval
	progPollInterval, espPollInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { progPollInterval, espPollInterval = a, b })
}

func runDetectFor(t *testing.T, p *detectPod, w *podWiring, o detectOpts) (*detect.Result, string, error) {
	t.Helper()
	shortPolls(t)
	var out bytes.Buffer
	err := runDetect(context.Background(), p, w, o, &out)
	var res detect.Result
	if o.asJSON {
		if e := json.Unmarshal(out.Bytes(), &res); e != nil {
			t.Fatalf("not JSON: %v\n%s", e, out.String())
		}
	}
	return &res, out.String(), err
}

func TestDetectSWDHumanOutput(t *testing.T) {
	p := newDetectPod()
	p.swd = map[bool]string{false: swdFirst, true: swdSecond}
	_, out, err := runDetectFor(t, p, benchWiring(), detectOpts{})
	if err != nil {
		t.Fatal(err)
	}
	want := "swd  STM32F446 (STMicroelectronics, Cortex-M4)  id 0x10006421  high\n"
	if out != want {
		t.Fatalf("output %q, want %q", out, want)
	}
	// the second call asks for the ID registers of the M4 core
	var second map[string]any
	for _, c := range p.sent {
		if c["cmd"] == "dap_start" {
			second = c
		}
	}
	if reads, _ := second["read"].([]string); len(reads) != 2 || reads[0] != "0xe0042000" {
		t.Fatalf("second dap_start: %v", second)
	}
	if p.count("dap_start") != 2 {
		t.Fatalf("dap_start calls: %d", p.count("dap_start"))
	}
}

func TestDetectJSONShapeAndJTAG(t *testing.T) {
	p := newDetectPod()
	p.swd = map[bool]string{false: swdFirst, true: swdSecond}
	p.jtag = jtagReply
	w := benchWiring()
	w.JtagTck, w.JtagTms, w.JtagTdi, w.JtagTdo = ip(11), ip(12), ip(9), ip(10)
	res, out, err := runDetectFor(t, p, w, detectOpts{asJSON: true, noInvasive: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Powered || len(res.Findings) != 3 {
		t.Fatalf("result: %s", out)
	}
	for _, key := range []string{`"powered"`, `"findings"`, `"skipped"`, `"notes"`, `"method"`, `"chip"`, `"vendor"`, `"core"`, `"id"`, `"confidence"`, `"pins"`, `"raw"`, `"invasive"`} {
		if !strings.Contains(out, key) {
			t.Errorf("JSON lacks %s:\n%s", key, out)
		}
	}
	if res.Findings[0].Method != "swd" || res.Findings[1].Method != "jtag" || res.Findings[2].Chip != "STM32F446" {
		t.Errorf("findings: %+v", res.Findings)
	}
	// the JTAG request carries all four pins and trst 0
	for _, c := range p.sent {
		if _, ok := c["tck"]; ok && (c["tdi"] != 9 || c["tdo"] != 10 || c["trst"] != 0) {
			t.Errorf("jtag request: %v", c)
		}
	}
}

func TestDetectPowerOff(t *testing.T) {
	p := newDetectPod()
	p.efuse1 = false
	res, out, err := runDetectFor(t, p, benchWiring(), detectOpts{asJSON: true})
	var o *detectOutcome
	if !asDetectOutcome(err, &o) || o.code != exitDetectUnpowered || exitCode(err) != 9 {
		t.Fatalf("err %v", err)
	}
	if res.Powered || len(res.Findings) != 0 || res.Notes == nil || !strings.Contains(out, "power is off") {
		t.Fatalf("result: %s", out)
	}
	if p.count("dap_start") != 0 || p.count("la_pins") != 0 {
		t.Fatal("nothing may be probed while the target is off")
	}
}

func asDetectOutcome(err error, into **detectOutcome) bool {
	o, ok := err.(*detectOutcome)
	*into = o
	return ok
}

func TestDetectRailFollowsProfile(t *testing.T) {
	p := newDetectPod() // efuse1 on, efuse2 off
	w := benchWiring()
	w.Efuse = ip(2)
	_, _, err := runDetectFor(t, p, w, detectOpts{})
	if exitCode(err) != exitDetectUnpowered {
		t.Fatalf("the profile's rail is efuse 2, which is off: %v", err)
	}
}

func TestDetectNothingFound(t *testing.T) {
	p := newDetectPod()
	p.swd = map[bool]string{false: `{"port":"swd","found":false}`}
	_, out, err := runDetectFor(t, p, benchWiring(), detectOpts{})
	if exitCode(err) != exitDetectNothing {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(out, "nothing detected") || !strings.Contains(out, "nothing answered on SWCLK LA11") {
		t.Fatalf("output %q", out)
	}
}

func TestDetectOwnedPinIsSkipped(t *testing.T) {
	p := newDetectPod()
	p.owners[11] = "uart"
	res, out, err := runDetectFor(t, p, benchWiring(), detectOpts{asJSON: true})
	if exitCode(err) != exitDetectNothing {
		t.Fatalf("err %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Method != "swd" || !strings.Contains(res.Skipped[0].Reason, "LA11") || !strings.Contains(res.Skipped[0].Reason, "uart") {
		t.Fatalf("skipped: %s", out)
	}
	if p.count("dap_start") != 0 {
		t.Fatal("an owned pin must not be probed")
	}
}

func TestDetectNeedsDapID(t *testing.T) {
	p := newDetectPod()
	p.status = okStatus("4.0.4", "v3r2", 18)
	w := benchWiring()
	w.JtagTck, w.JtagTms, w.JtagTdi, w.JtagTdo = ip(9), ip(10), ip(13), ip(14)
	res, _, err := runDetectFor(t, p, w, detectOpts{asJSON: true})
	if exitCode(err) != exitDetectNothing {
		t.Fatalf("err %v", err)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("skipped: %+v", res.Skipped)
	}
	for _, s := range res.Skipped {
		if s.Reason != "needs pod firmware with dap_id" {
			t.Errorf("%+v", s)
		}
	}
	if p.count("dap_start") != 0 {
		t.Fatal("firmware without dap_id must never get dap_start op id (it would start a DAP session)")
	}
}

// A PIC answers only in its own entry style: without a device name, detect tries each style of
// the table (python#154's identify) and finds a PIC18 Q (flags 3) and a PIC16F18xxx (flags 1).
func TestDetectPICEntryStyles(t *testing.T) {
	for _, tc := range []struct {
		id, chip string
		style    int
	}{{"c074", "PIC18F57Q43", 3}, {"d430", "PIC16F18446", 1}} {
		p := newDetectPod()
		p.progID, p.progStyle = tc.id, tc.style
		w := &podWiring{Efuse: ip(1)}
		addSignal(w, "ICSPCLK", 4)
		addSignal(w, "ICSPDAT", 3)
		res, out, err := runDetectFor(t, p, w, detectOpts{asJSON: true})
		if err != nil {
			t.Fatalf("%s: %v", tc.chip, err)
		}
		if len(res.Findings) != 1 || res.Findings[0].Chip != tc.chip || res.Findings[0].Confidence != "high" {
			t.Fatalf("%s: %s", tc.chip, out)
		}
		var styles []int
		for _, c := range p.sent {
			if c["cmd"] == "prog_flash" && c["op"] == "info" {
				f, _ := c["flags"].(int)
				styles = append(styles, f)
			}
		}
		if styles[0] != 0 || styles[len(styles)-1]&0x73 != tc.style {
			t.Fatalf("%s: info flags sent %v", tc.chip, styles)
		}
	}
	// a PIC that answers no style: a note, no finding, and every style was tried once
	p := newDetectPod()
	w := &podWiring{Efuse: ip(1)}
	addSignal(w, "ICSPCLK", 4)
	addSignal(w, "ICSPDAT", 3)
	res, out, _ := runDetectFor(t, p, w, detectOpts{asJSON: true})
	if len(res.Findings) != 0 || p.count("prog_flash") < 2*len(progdev.PICEntryStyles()) {
		t.Fatalf("no answer: %s (prog_flash calls %d)", out, p.count("prog_flash"))
	}
}

func TestDetectPICFromProfileSignals(t *testing.T) {
	p := newDetectPod()
	p.progID = "a400" // PIC16F1847's ID
	w := &podWiring{Efuse: ip(1)}
	addSignal(w, "ICSPCLK", 3)
	addSignal(w, "ICSPDAT", 4)
	res, out, err := runDetectFor(t, p, w, detectOpts{asJSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("result: %s", out)
	}
	f := res.Findings[0]
	if f.Method != "pic" || !strings.HasPrefix(f.Chip, "PIC16F") || f.Confidence != "high" || !f.Invasive || f.Vendor != "Microchip" {
		t.Fatalf("finding: %+v", f)
	}
	var info map[string]any
	for _, c := range p.sent {
		if c["cmd"] == "prog_flash" && c["op"] == "info" {
			info = c
		}
	}
	if info["proto"] != "pic" || info["clk"] != 3 || info["data"] != 4 || info["reset"] != "nrst" {
		t.Fatalf("prog_flash info: %v", info)
	}
	// with --no-invasive the pod is not touched for it
	p2 := newDetectPod()
	p2.progID = "a400"
	_, _, err = runDetectFor(t, p2, w, detectOpts{noInvasive: true})
	if exitCode(err) != exitDetectNothing || p2.count("prog_flash") != 0 {
		t.Fatalf("--no-invasive: err %v, prog_flash calls %d", err, p2.count("prog_flash"))
	}
}

func TestDetectProgNeedsItsOwnSignals(t *testing.T) {
	// The SWD pins are not a fallback for a programmer: only a signal named for the protocol counts.
	p := newDetectPod()
	p.swd = map[bool]string{false: `{"port":"swd","found":false}`}
	_, _, _ = runDetectFor(t, p, benchWiring(), detectOpts{})
	if p.count("prog_flash") != 0 {
		t.Fatal("prog_flash was sent without a signal named for a protocol")
	}
	// Asking for the method says why it is not probed.
	res, _, _ := runDetectFor(t, p, benchWiring(), detectOpts{asJSON: true, methods: []string{"updi"}})
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "UPDI") {
		t.Fatalf("skipped: %+v", res.Skipped)
	}
}

func TestDetectNoAnswerFromProgrammerIsANote(t *testing.T) {
	p := newDetectPod()
	w := &podWiring{}
	addSignal(w, "UPDI", 7)
	res, _, err := runDetectFor(t, p, w, detectOpts{asJSON: true})
	if exitCode(err) != exitDetectNothing {
		t.Fatalf("err %v", err)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "updi: no chip answered") {
		t.Fatalf("notes: %v", res.Notes)
	}
}

func TestDetectESP32(t *testing.T) {
	p := newDetectPod()
	p.espChip, p.espRunning = "ESP32-S3", 2
	w := &podWiring{UartRx: ip(5), UartTx: ip(4)}
	res, _, err := runDetectFor(t, p, w, detectOpts{asJSON: true})
	if exitCode(err) != exitDetectNothing || len(res.Skipped) != 0 {
		t.Fatalf("no boot signal: not wired, silently left out: %v %+v", err, res.Skipped)
	}
	addSignal(w, "IO0", 6)
	res, _, err = runDetectFor(t, p, w, detectOpts{asJSON: true})
	if err != nil || len(res.Findings) != 1 || res.Findings[0].Chip != "ESP32-S3" || res.Findings[0].Vendor != "Espressif" || !res.Findings[0].Invasive {
		t.Fatalf("result: %+v %v", res, err)
	}
}

func TestDetectUSB(t *testing.T) {
	const dev = `{"mode":"host","port":"usb_a","attached":true,"connected":true,"device":{"vid":5824,"pid":1144,"manufacturer":"PJRC","product":"Teensy Bootloader"}}`
	p := newDetectPod()
	p.status = okStatus("4.0.5", "v3r4", 19)
	p.usb = dev
	res, _, err := runDetectFor(t, p, &podWiring{}, detectOpts{asJSON: true, noInvasive: true})
	if err != nil || len(res.Findings) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if f := res.Findings[0]; f.Method != "usb" || f.ID != "16c0:0478" || f.Chip != "Teensy (HalfKay bootloader)" || f.Confidence != "high" || f.Invasive {
		t.Fatalf("finding: %+v", f)
	}
	// host mode off: skipped, and never turned on
	p.usb = `{"mode":"device"}`
	res, _, _ = runDetectFor(t, p, &podWiring{}, detectOpts{asJSON: true, methods: []string{"usb"}})
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "host mode is off") {
		t.Fatalf("skipped: %+v", res.Skipped)
	}
	for _, c := range p.sent {
		if _, ok := c["enabled"]; ok {
			t.Fatalf("detect sent %v", c)
		}
	}
}

func TestDetectMethodsFlag(t *testing.T) {
	p := newDetectPod()
	p.swd = map[bool]string{false: swdFirst, true: swdSecond}
	p.jtag = jtagReply
	w := benchWiring()
	w.JtagTck, w.JtagTms, w.JtagTdi, w.JtagTdo = ip(9), ip(10), ip(13), ip(14)
	res, _, err := runDetectFor(t, p, w, detectOpts{asJSON: true, methods: []string{"jtag"}})
	if err != nil || len(res.Findings) != 2 || res.Findings[0].Method != "jtag" {
		t.Fatalf("%+v %v", res, err)
	}
	for _, c := range p.sent {
		if _, ok := c["swclk"]; ok {
			t.Fatal("swd was probed though --methods named only jtag")
		}
	}
	if err := validateDetectMethods([]string{"swd", "spi"}); exitCode(err) != exitUsage {
		t.Fatalf("unknown method: %v", err)
	}
}

func TestDetectCommandWiring(t *testing.T) {
	if exitCode(&detectOutcome{exitDetectNothing, "x"}) != 8 {
		t.Fatal("exit code")
	}
	if code := run([]string{"detect", "--methods", "nope"}); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
}
