package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// usbExtPod adds scripted usb_check and usb_hid state to the fake USB pod.
type usbExtPod struct {
	*fakeUSBPod
	checkRuns   int    // usb_check polls that report running
	checkResult string // the final usb_check reply
	checkErr    error  // refusal of the start
	hidList     string
	hidMore     []string // replies to the descriptor continuation reads
	hidReads    []string // replies to successive reads
	hidSeen     []map[string]any
}

func newUSBExtPod(t *testing.T) *usbExtPod {
	p := &usbExtPod{fakeUSBPod: newFakeUSBPod(t)}
	p.status = okStatus("4.0.3", "v3r4", 15)
	p.device = usbDev(0x0483, 0x5740, "Board", usbIface(3, 1, 1, "hid"))
	return p
}

func (p *usbExtPod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	switch req["cmd"] {
	case "usb_check":
		p.cmds = append(p.cmds, req)
		if _, ok := req["abort"]; ok {
			p.checkRuns = 0
			return json.RawMessage(`{"running":false,"have_result":false}`), nil
		}
		if _, ok := req["test"]; ok {
			if p.checkErr != nil {
				return nil, p.checkErr
			}
			return json.RawMessage(`{"running":true,"have_result":false,"test":"x"}`), nil
		}
		if p.checkRuns > 0 {
			p.checkRuns--
			return json.RawMessage(`{"running":true,"have_result":false,"test":"enumerate","phase":"power_off","done":2,"total":10,"ms":900}`), nil
		}
		return json.RawMessage(p.checkResult), nil
	case "usb_hid":
		p.cmds = append(p.cmds, req)
		p.hidSeen = append(p.hidSeen, req)
		if _, ok := req["capture"]; ok {
			return json.RawMessage(`{"capturing":true,"buffered":0,"dropped":0}`), nil
		}
		if _, ok := req["read"]; ok {
			if len(p.hidReads) == 0 {
				return json.RawMessage(`{"reports":[],"count":0,"dropped":0,"capturing":true}`), nil
			}
			r := p.hidReads[0]
			p.hidReads = p.hidReads[1:]
			return json.RawMessage(r), nil
		}
		if _, ok := req["offset"]; ok && len(p.hidMore) > 0 {
			r := p.hidMore[0]
			p.hidMore = p.hidMore[1:]
			return json.RawMessage(r), nil
		}
		return json.RawMessage(p.hidList), nil
	}
	return p.fakeUSBPod.Command(ctx, req)
}

func runCheckFake(t *testing.T, pod *usbExtPod, o usbCheckOpts) (string, string, error) {
	t.Helper()
	fastUSB(t)
	var out, prog bytes.Buffer
	o.hostSecs = 360
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runUSBCheck(ctx, pod, o, &out, &prog)
	return out.String(), prog.String(), err
}

const enumerateResult = `{"running":false,"have_result":true,"test":"enumerate","phase":"done","done":10,"total":10,"ms":9100,"pass":true,"error":"",` +
	`"result":{"vid":1155,"pid":22336,"cycles":10,"ok":10,"no_connect":0,"stuck":0,"timeout":0,"off_ms":500,` +
	`"connect_ms":{"min":110,"avg":120,"max":140},"descriptor_ms":{"min":150,"avg":160,"max":190},"mount_ms":{"min":300,"avg":320,"max":380}}}`

func TestRunUSBCheckEnumerateStartsHostAndPrints(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.checkRuns = 3
	pod.checkResult = enumerateResult
	out, prog, err := runCheckFake(t, pod, usbCheckOpts{action: "enumerate", count: 10, offMS: []int{500}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prog, "USB host mode was off: starting it on usb_a") || !strings.Contains(prog, "power_off: 2 of 10") {
		t.Errorf("progress:\n%s", prog)
	}
	for _, want := range []string{"USB check enumerate: PASS", "board 0483:5740", "10 ok (power off 500 ms)", "min 110, avg 120, max 140 ms", "Mounted:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	start := pod.sent("usb_check")[0]
	if start["test"] != "enumerate" || start["count"] != 10 || start["off_ms"] != 500 || start["timeout_ms"] != nil {
		t.Errorf("start %v", start)
	}
}

func TestRunUSBCheckReplugSendsListAndFailsOnFailure(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.mode, pod.port, pod.remain = "host", "usb_a", 3000
	pod.checkResult = `{"running":false,"have_result":true,"test":"replug","phase":"done","done":8,"total":8,"ms":4000,"pass":false,"error":"",` +
		`"result":{"vid":1,"pid":2,"count":2,"lengths":[{"off_ms":5,"stayed":2,"reenumerated":0,"failed":0},{"off_ms":100,"stayed":0,"reenumerated":1,"failed":1}]}}`
	out, _, err := runCheckFake(t, pod, usbCheckOpts{action: "replug", count: 2, offMS: []int{5, 100}, cycleTO: 3000})
	if err == nil || !strings.Contains(err.Error(), "replug check failed") || exitCode(err) == exitOK {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "100 ms:    1 re-enumerated, 0 stayed on the bus, 1 failed") {
		t.Errorf("output:\n%s", out)
	}
	req := pod.sent("usb_check")[0]
	if offs, ok := req["off_ms"].([]int); !ok || len(offs) != 2 || req["timeout_ms"] != 3000 {
		t.Errorf("start %#v", req)
	}
}

func TestRunUSBCheckPowerWaitsForBoardAndStopsEarly(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.checkResult = `{"running":false,"have_result":true,"test":"power","phase":"done","ms":50,"pass":false,"error":"not_powered_by_efuse1","result":{"vid":1,"pid":2,"samples":0,"avg_ua":0,"min_ua":0,"peak_ua":0,"limit_ma":100,"verdict":"ok"}}`
	_, prog, err := runCheckFake(t, pod, usbCheckOpts{action: "power", durationMS: 5000})
	if err == nil || !strings.Contains(err.Error(), "not wired to eFuse 1") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(prog, "starting it on usb_a") {
		t.Errorf("progress %s", prog)
	}
	if req := pod.sent("usb_check")[0]; req["duration_ms"] != 5000 || req["count"] != nil {
		t.Errorf("start %v", req)
	}
}

func TestRunUSBCheckSuspendOutput(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.checkResult = `{"running":false,"have_result":true,"test":"suspend","phase":"done","ms":2000,"pass":true,"error":"","result":{"vid":1,"pid":2,` +
		`"samples":100,"avg_ua":1800,"min_ua":1700,"peak_ua":2100,"self_powered":false,"limit_ua":2500,"current":"ok","suspend_ok":true,"susprdy":true,"sof_stopped":true,"resume":"signaling","sof_resumed":true,"answers":true,"recovered_by_reset":false,"port_reinit":false,"reinit_ok":false}}`
	out, _, err := runCheckFake(t, pod, usbCheckOpts{action: "suspend", settleMS: 300})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PASS", "avg 1.80 mA against 2.50 mA: ok", "Resume:      signaling, board answers: yes"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if req := pod.sent("usb_check")[0]; req["settle_ms"] != 300 {
		t.Errorf("start %v", req)
	}
}

func TestRunUSBCheckStatusAbortAndJSON(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.checkResult = `{"running":false,"have_result":false}`
	out, _, err := runCheckFake(t, pod, usbCheckOpts{action: "status"})
	if err != nil || !strings.Contains(out, "No USB check has run yet") {
		t.Fatalf("status: %q %v", out, err)
	}
	if len(pod.sent("usb_host")) != 0 {
		t.Error("status must not touch host mode")
	}
	pod.checkRuns = 1
	out, _, _ = runCheckFake(t, pod, usbCheckOpts{action: "status"})
	if !strings.Contains(out, "running, phase power_off, 2 of 10") {
		t.Errorf("running status: %q", out)
	}
	if _, _, err = runCheckFake(t, pod, usbCheckOpts{action: "abort"}); err != nil {
		t.Fatal(err)
	}
	if got := pod.sent("usb_check"); got[len(got)-1]["abort"] != true {
		t.Errorf("abort %v", got)
	}
	pod.checkResult = enumerateResult
	out, _, err = runCheckFake(t, pod, usbCheckOpts{action: "enumerate", asJSON: true})
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), `{"running":false,"have_result":true,"test":"enumerate"`) {
		t.Errorf("json: %q %v", out, err)
	}
}

func TestRunUSBCheckRefusalsAndOldFirmware(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.status = okStatus("4.0.2", "v3r4", 14)
	if _, _, err := runCheckFake(t, pod, usbCheckOpts{action: "reset"}); err == nil || !strings.Contains(err.Error(), "needs firmware 4.0.3") {
		t.Errorf("old firmware: %v", err)
	}
	pod = newUSBExtPod(t)
	pod.checkErr = &tcpclient.PodError{Cmd: "usb_check", Code: "BP0088", Reason: "The board is not powered from target-power eFuse 1"}
	_, _, err := runCheckFake(t, pod, usbCheckOpts{action: "power"})
	if err == nil || !strings.Contains(err.Error(), "eFuse 1") {
		t.Errorf("refusal: %v", err)
	}
}

func TestRunUSBCheckAbortsWhenCancelled(t *testing.T) {
	fastUSB(t)
	pod := newUSBExtPod(t)
	pod.checkRuns = 1 << 30
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var out, prog bytes.Buffer
	err := runUSBCheck(ctx, pod, usbCheckOpts{action: "reset", hostSecs: 360}, &out, &prog)
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("err = %v", err)
	}
	if got := pod.sent("usb_check"); got[len(got)-1]["abort"] != true {
		t.Errorf("no abort sent: %v", got)
	}
}

func TestValidateUSBCheck(t *testing.T) {
	bad := []struct {
		name string
		o    usbCheckOpts
		dur  time.Duration
		set  [5]bool // count, off, cycleTO, duration, settle
	}{
		{"unknown test", usbCheckOpts{action: "boot"}, 0, [5]bool{}},
		{"count on power", usbCheckOpts{action: "power", count: 3}, 0, [5]bool{true}},
		{"duration on reset", usbCheckOpts{action: "reset"}, time.Second, [5]bool{false, false, false, true}},
		{"settle on power", usbCheckOpts{action: "power", settleMS: 100}, 0, [5]bool{false, false, false, false, true}},
		{"count too big", usbCheckOpts{action: "replug", count: 101}, 0, [5]bool{true}},
		{"enumerate two off", usbCheckOpts{action: "enumerate", offMS: []int{100, 200}}, 0, [5]bool{false, true}},
		{"enumerate off low", usbCheckOpts{action: "enumerate", offMS: []int{10}}, 0, [5]bool{false, true}},
		{"replug 7 lengths", usbCheckOpts{action: "replug", offMS: []int{1, 2, 3, 4, 5, 6, 7}}, 0, [5]bool{false, true}},
		{"cycle timeout", usbCheckOpts{action: "reset", cycleTO: 100}, 0, [5]bool{false, false, true}},
		{"duration low", usbCheckOpts{action: "power"}, 10 * time.Millisecond, [5]bool{false, false, false, true}},
		{"status with flags", usbCheckOpts{action: "status", count: 1}, 0, [5]bool{true}},
		{"bad port", usbCheckOpts{action: "reset", port: "usb_b"}, 0, [5]bool{}},
	}
	for _, c := range bad {
		o := c.o
		err := validateUSBCheck(&o, c.dur, 0, c.set[0], c.set[1], c.set[2], c.set[3], c.set[4])
		if err == nil || exitCode(err) != exitUsage {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	o := usbCheckOpts{action: "replug", count: 5, offMS: []int{5, 20}}
	if err := validateUSBCheck(&o, 0, 0, true, true, false, false, false); err != nil {
		t.Errorf("good replug: %v", err)
	}
	o = usbCheckOpts{action: "power"}
	if err := validateUSBCheck(&o, 3*time.Second, 0, false, false, false, true, false); err != nil || o.durationMS != 3000 {
		t.Errorf("good power: %v %+v", err, o)
	}
}

// ── usb-hid ─────────────────────────────────────────────────────────────────

func runHIDFake(t *testing.T, pod *usbExtPod, o usbHIDOpts) (string, string, error) {
	t.Helper()
	fastUSB(t)
	var out, prog bytes.Buffer
	o.hostSecs = 360
	if o.duration == 0 {
		o.duration = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runUSBHID(ctx, pod, o, &out, &prog)
	return out.String(), prog.String(), err
}

func TestRunUSBHIDListReadsLongDescriptors(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.hidList = `{"attached":true,"interfaces":[{"instance":0,"interface":0,"protocol":"keyboard","descriptor_len":4,"descriptor_offset":0,"truncated":true,"descriptor":"0501"},` +
		`{"instance":1,"interface":1,"protocol":"mouse","descriptor_len":2,"descriptor_offset":0,"truncated":false,"descriptor":"0502"}]}`
	pod.hidMore = []string{`{"attached":true,"interfaces":[{"instance":0,"interface":0,"protocol":"keyboard","descriptor_len":4,"descriptor_offset":2,"truncated":false,"descriptor":"0906"}]}`}
	out, prog, err := runHIDFake(t, pod, usbHIDOpts{action: "list", instance: -1})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hid0: USB interface 0, protocol keyboard, report descriptor 4 bytes", "05010906", "hid1: USB interface 1, protocol mouse"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cut short") || !strings.Contains(prog, "starting it on usb_a") {
		t.Errorf("out %q prog %q", out, prog)
	}
	cont := pod.hidSeen[1]
	if cont["offset"] != 2 || cont["instance"] != 0 {
		t.Errorf("continuation %v", cont)
	}
}

func TestRunUSBHIDListJSONAndNone(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.hidList = `{"attached":true,"interfaces":[]}`
	out, _, err := runHIDFake(t, pod, usbHIDOpts{action: "list", instance: -1})
	if err != nil || !strings.Contains(out, "no HID interface") {
		t.Errorf("none: %q %v", out, err)
	}
	pod.hidList = `{"attached":true,"interfaces":[{"instance":0,"interface":0,"protocol":"none","descriptor_len":2,"descriptor_offset":0,"truncated":false,"descriptor":"0501"}]}`
	out, _, err = runHIDFake(t, pod, usbHIDOpts{action: "list", instance: 0, asJSON: true})
	var l usbHIDList
	if err != nil || json.Unmarshal([]byte(out), &l) != nil || len(l.Interfaces) != 1 || l.Interfaces[0].Descriptor != "0501" {
		t.Errorf("json: %q %v", out, err)
	}
	if pod.hidSeen[len(pod.hidSeen)-1]["instance"] != 0 {
		t.Errorf("instance not sent: %v", pod.hidSeen)
	}
}

func TestRunUSBHIDCaptureCountStopsAndFreesTheRing(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.hidReads = []string{
		`{"reports":[{"ms":812,"instance":0,"data":"0000040000000000"},{"ms":830,"instance":1,"data":"00010000"}],"count":2,"dropped":0,"capturing":true}`,
		`{"reports":[{"ms":920,"instance":0,"data":"0000000000000000"}],"count":1,"dropped":3,"capturing":true}`,
	}
	out, prog, err := runHIDFake(t, pod, usbHIDOpts{action: "capture", instance: -1, count: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"    812 ms  hid0  0000040000000000", "    830 ms  hid1  00010000", "    920 ms  hid0  0000000000000000"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(prog, "3 report(s) were dropped") {
		t.Errorf("progress %q", prog)
	}
	first, last := pod.hidSeen[0], pod.hidSeen[len(pod.hidSeen)-1]
	if first["capture"] != true || last["capture"] != false {
		t.Errorf("capture start/stop: %v ... %v", first, last)
	}
	if rd := pod.hidSeen[1]; rd["read"] != true || rd["count"] != 3 {
		t.Errorf("first read %v", rd)
	}
	if rd := pod.hidSeen[2]; rd["count"] != 1 {
		t.Errorf("second read asks for what is left: %v", rd)
	}
}

func TestRunUSBHIDCaptureEndsWhenTheBoardLeavesAndEmptyJSON(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.hidReads = []string{`{"reports":[],"count":0,"dropped":0,"capturing":false}`}
	out, prog, err := runHIDFake(t, pod, usbHIDOpts{action: "capture", instance: 1, asJSON: true})
	if err != nil || !strings.Contains(prog, "capture ended early") {
		t.Fatalf("%v %q", err, prog)
	}
	var r usbHIDReadReply
	if json.Unmarshal([]byte(out), &r) != nil || r.Reports == nil || r.Count != 0 || r.Capturing {
		t.Errorf("json %q", out)
	}
	if rd := pod.hidSeen[1]; rd["instance"] != 1 {
		t.Errorf("read %v", rd)
	}
}

func TestRunUSBHIDOldFirmwareAndValidation(t *testing.T) {
	pod := newUSBExtPod(t)
	pod.status = okStatus("4.0.2", "v3r4", 14)
	if _, _, err := runHIDFake(t, pod, usbHIDOpts{action: "list", instance: -1}); err == nil || !strings.Contains(err.Error(), "needs firmware 4.0.3") {
		t.Errorf("old firmware: %v", err)
	}
	for _, c := range []struct {
		o                             usbHIDOpts
		hasInstance, hasCount, hasDur bool
	}{
		{usbHIDOpts{action: "dump"}, false, false, false},
		{usbHIDOpts{action: "list"}, false, true, false},
		{usbHIDOpts{action: "list"}, false, false, true},
		{usbHIDOpts{action: "capture", duration: time.Millisecond}, false, false, true},
		{usbHIDOpts{action: "capture", duration: time.Second, count: -1}, false, true, false},
		{usbHIDOpts{action: "capture", duration: time.Second, instance: 2}, true, false, false},
		{usbHIDOpts{action: "capture", duration: time.Second, port: "usb_x"}, false, false, false},
	} {
		o := c.o
		if err := validateUSBHID(&o, c.hasInstance, c.hasCount, c.hasDur); err == nil || exitCode(err) != exitUsage {
			t.Errorf("%+v: %v", c.o, err)
		}
	}
}

// ── usb-flash: PICOBOOT, read-back, time ────────────────────────────────────

func TestChooseAndPlanPicoboot(t *testing.T) {
	bootsel := usbDev(0x2E8A, 0x0003, "RP2 Boot", usbIface(8, 6, 80, "msc"), usbIface(0xFF, 0, 0, "vendor"))
	bin := []byte{1, 2, 3, 4}
	if mode, why, err := chooseUSBFlashMode("app.bin", bin, bootsel); err != nil || mode != "picoboot" || why != "a Pico in BOOTSEL" {
		t.Errorf("choose bin: %q %q %v", mode, why, err)
	}
	pico2 := usbDev(0x2E8A, 0x000F, "RP2350 Boot", usbIface(8, 6, 80, "msc"))
	if mode, _, _ := chooseUSBFlashMode("app.hex", bin, pico2); mode != "picoboot" {
		t.Errorf("pico 2: %q", mode)
	}
	if mode, _, _ := chooseUSBFlashMode("app.uf2", bin, bootsel); mode != "uf2" {
		t.Errorf("a .uf2 file stays uf2: %q", mode)
	}
	p, err := planUSBFlash(usbFlashOpts{file: "app.bin"}, bin, bootsel)
	if err != nil || p.Mode != "picoboot" || !p.HasAddr || p.Address != 0x10000000 {
		t.Errorf("plan %+v %v", p, err)
	}
	hexFile := []byte(hexRec(0x04, 0, 0x10, 0x00) + hexRec(0, 0x2000, 7, 8) + hexRec(1, 0))
	if p, err = planUSBFlash(usbFlashOpts{file: "app.hex"}, hexFile, bootsel); err != nil || p.Address != 0x10002000 || len(p.Data) != 2 {
		t.Errorf("hex plan %+v %v", p, err)
	}
	if p, err = planUSBFlash(usbFlashOpts{file: "app.bin", address: "0x10010000", mode: "picoboot"}, bin, nil); err != nil || p.Address != 0x10010000 {
		t.Errorf("--address: %+v %v", p, err)
	}
	if _, err = planUSBFlash(usbFlashOpts{file: "app.bin", address: "0x10000100", mode: "picoboot"}, bin, nil); err == nil || !strings.Contains(err.Error(), "multiple of 4096") {
		t.Errorf("unaligned: %v", err)
	}
}

func TestRunUSBFlashPicoboot(t *testing.T) {
	pod := newFakeUSBPod(t)
	pod.status = okStatus("4.0.3", "v3r4", 15)
	pod.mode, pod.port, pod.remain = "host", "usb_a", 3000
	pod.device = usbDev(0x2E8A, 0x0003, "RP2 Boot", usbIface(8, 6, 80, "msc"))
	pod.flashResult = usbFlashStatus{Mode: "picoboot", Phase: "done", Done: 9, Total: 9, Result: "ok", OK: true, Protocol: "picoboot"}
	out, prog, err := runFlashFake(t, pod, usbFlashOpts{file: "app.bin", noLeave: true}, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	start := pod.sent("usb_flash")[0]
	if start["mode"] != "picoboot" || start["address"] != "0x10000000" || start["leave"] != false || start["time"] != nil {
		t.Errorf("start %v", start)
	}
	if !strings.Contains(prog, "picoboot mode (a Pico in BOOTSEL)") || !strings.Contains(out, "picoboot mode (picoboot)") || !strings.Contains(out, "stays in its bootloader") {
		t.Errorf("prog %q out %q", prog, out)
	}
	// A pod without the feature refuses before touching the board.
	old := newFakeUSBPod(t)
	old.status = okStatus("4.0.3", "v3r4", 14)
	old.mode, old.port, old.remain = "host", "usb_a", 3000
	old.device = pod.device
	_, _, err = runFlashFake(t, old, usbFlashOpts{file: "app.bin"}, []byte{1})
	if err == nil || !strings.Contains(err.Error(), "needs firmware 4.0.3") || len(old.sent("usb_flash")) != 0 {
		t.Errorf("old pod: %v %v", err, old.sent("usb_flash"))
	}
}

func TestRunUSBFlashFilePassesTimeAndReadBack(t *testing.T) {
	old := usbFlashNow
	usbFlashNow = func() time.Time { return time.Unix(1800000000, 0) }
	t.Cleanup(func() { usbFlashNow = old })
	pod := newFakeUSBPod(t)
	pod.status = okStatus("4.0.3", "v3r4", 15)
	pod.mode, pod.port, pod.remain = "host", "usb_a", 3000
	pod.device = usbDev(0x0D28, 0x0204, "DAPLink", usbIface(8, 6, 80, "msc"))
	pod.flashResult = usbFlashStatus{Mode: "file", Phase: "done", Result: "ok", OK: true, Note: "FAIL.TXT was not checked: the drive did not come back"}
	out, _, err := runFlashFake(t, pod, usbFlashOpts{file: "fw.bin", checkFail: true}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	st := pod.sent("usb_flash")[0]
	if st["time"] != int64(1800000000) || st["check_fail_txt"] != true || st["verify"] != nil {
		t.Errorf("start %v", st)
	}
	if !strings.Contains(out, "Read-back: FAIL.TXT was not checked") {
		t.Errorf("out %q", out)
	}
	// FAIL.TXT found: the flash fails and the note is in the message.
	pod2 := newFakeUSBPod(t)
	pod2.status = pod.status
	pod2.mode, pod2.port, pod2.remain = "host", "usb_a", 3000
	pod2.device = pod.device
	pod2.flashResult = usbFlashStatus{Mode: "file", Phase: "read file", Result: "the loader reported a failure", Note: "The interface firmware FAILED to reset/halt the target MCU"}
	_, _, err = runFlashFake(t, pod2, usbFlashOpts{file: "fw.bin", checkFail: true}, []byte{1})
	if err == nil || !strings.Contains(err.Error(), "[The interface firmware FAILED") || !strings.Contains(err.Error(), "loader rejected the file") {
		t.Errorf("err = %v", err)
	}
}

func TestRunUSBFlashUF2Verify(t *testing.T) {
	pod := newFakeUSBPod(t)
	pod.status = okStatus("4.0.3", "v3r4", 15)
	pod.mode, pod.port, pod.remain = "host", "usb_a", 3000
	pod.device = usbDev(0x2E8A, 3, "Pico", usbIface(8, 6, 80, "msc"))
	pod.flashResult = usbFlashStatus{Mode: "uf2", Phase: "done", Result: "ok", OK: true, Note: "verified 12 blocks"}
	out, _, err := runFlashFake(t, pod, usbFlashOpts{file: "a.uf2", verify: true}, []byte("x"))
	if err != nil || !strings.Contains(out, "Read-back: verified 12 blocks") || pod.sent("usb_flash")[0]["verify"] != true {
		t.Errorf("%v %q %v", err, out, pod.sent("usb_flash")[0])
	}
	// --verify with a mode that is not uf2 is a usage error, before any flash starts.
	pod2 := newFakeUSBPod(t)
	pod2.status = pod.status
	pod2.mode, pod2.port, pod2.remain = "host", "usb_a", 3000
	pod2.device = usbDev(0x0483, 0xDF11, "DFU", usbIface(0xFE, 1, 2, ""))
	_, _, err = runFlashFake(t, pod2, usbFlashOpts{file: "a.bin", verify: true}, []byte("x"))
	if err == nil || exitCode(err) != exitUsage || len(pod2.sent("usb_flash")) != 0 {
		t.Errorf("verify on dfu: %v", err)
	}
	// An older pod (table rev 14) lacks the read-back.
	pod3 := newFakeUSBPod(t)
	pod3.device = pod.device
	_, _, err = runFlashFake(t, pod3, usbFlashOpts{file: "a.uf2", verify: true}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "needs firmware 4.0.3") {
		t.Errorf("rev 14: %v", err)
	}
}

func TestUSBFlashModesIncludePicoboot(t *testing.T) {
	o := usbFlashOpts{mode: "PicoBoot"}
	if err := validateUSBFlashFlags(&o); err != nil || o.mode != "picoboot" {
		t.Errorf("%+v %v", o, err)
	}
}
