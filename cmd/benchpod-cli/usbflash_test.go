package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── fakes ───────────────────────────────────────────────────────────────────

func usbIface(class, sub, proto int, name string) usbInterface {
	return usbInterface{Class: class, Subclass: sub, Protocol: proto, ClassName: name, Endpoints: 2}
}

func usbDev(vid, pid int, product string, ifaces ...usbInterface) *usbDevice {
	return &usbDevice{VID: vid, PID: pid, Product: product, Speed: "full", USB: "2.00", Release: "1.00",
		Config: &usbConfig{Value: 1, MaxPowerMA: 100, Interfaces: ifaces}}
}

// fakeUSBPod is a pod with scripted usb_host and usb_flash state. It records every command.
type fakeUSBPod struct {
	t        *testing.T
	status   string // the status reply data
	mode     string // usb_host mode now: device, starting, host
	port     string
	remain   int
	hostSeen int // usb_host reads so far
	device   *usbDevice
	// the device enumerates after this many usb_host reads in host mode (0 = at once)
	enumAfter int
	// usb_flash behaviour
	flashRuns   int // status polls that report running
	flashResult usbFlashStatus
	startErr    error // usb_flash start refusal
	staged      []byte
	stageErr    error
	cmds        []map[string]any
	polls       int
	// cancel is called when the host mode has been read cancelHostAt times, or the flash polled
	// cancelFlashAt times (0 = never). Tests end a run with it instead of a real-time deadline, which
	// fires at a random point on a loaded machine.
	cancel                      context.CancelFunc
	cancelHostAt, cancelFlashAt int
}

func okStatus(version, boardRev string, featuresRev int) string {
	return fmt.Sprintf(`{"version":%q,"board_rev":%q,"features_rev":%d,"analog":false,"gateware":60}`, version, boardRev, featuresRev)
}

func newFakeUSBPod(t *testing.T) *fakeUSBPod {
	return &fakeUSBPod{t: t, status: okStatus("4.0.3", "v3r4", 14), mode: "device", port: "none"}
}

func (f *fakeUSBPod) sent(cmd string) []map[string]any {
	var out []map[string]any
	for _, c := range f.cmds {
		if c["cmd"] == cmd {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeUSBPod) report() string {
	r := usbHostReport{Mode: f.mode, Port: f.port, RemainingS: f.remain}
	if f.mode == "host" {
		r.Connected, r.LineSpeed, r.Enumeration = f.device != nil, "full", "none"
		if f.device != nil && f.hostSeen > f.enumAfter {
			r.Attached, r.Enumeration, r.Device, r.Enumerations = true, "done", f.device, 1
		} else if f.device != nil {
			r.Enumeration = "running"
		}
	}
	b, _ := json.Marshal(r)
	return string(b)
}

func (f *fakeUSBPod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.cmds = append(f.cmds, req)
	switch req["cmd"] {
	case "status":
		return json.RawMessage(f.status), nil
	case "usb_host":
		if en, ok := req["enabled"].(bool); ok {
			if en {
				f.mode, f.port, f.remain = "host", req["port"].(string), req["timeout_s"].(int)
				f.hostSeen = 0
			} else {
				f.mode, f.port = "device", "none"
			}
			return json.RawMessage(`{}`), nil
		}
		if f.mode == "host" {
			f.hostSeen++
			if f.cancel != nil && f.cancelHostAt > 0 && f.hostSeen == f.cancelHostAt {
				f.cancel()
			}
		}
		return json.RawMessage(f.report()), nil
	case "usb_flash":
		if _, ok := req["abort"]; ok {
			f.flashRuns = 0
			return json.RawMessage(`{}`), nil
		}
		if _, ok := req["mode"]; ok {
			if f.startErr != nil {
				return nil, f.startErr
			}
			return json.RawMessage(`{"running":true}`), nil
		}
		f.polls++
		if f.cancel != nil && f.cancelFlashAt > 0 && f.polls == f.cancelFlashAt {
			f.cancel()
		}
		st := f.flashResult
		if f.flashRuns > 0 {
			f.flashRuns--
			st = usbFlashStatus{Running: true, Phase: "write", Done: 5 - f.flashRuns, Total: 5}
		} else {
			st.HaveResult = true
		}
		b, _ := json.Marshal(st)
		return b, nil
	}
	f.t.Fatalf("unexpected command %v", req)
	return nil, nil
}

func (f *fakeUSBPod) StagePSRAM(ctx context.Context, data []byte) (int, error) {
	if f.stageErr != nil {
		return 0, f.stageErr
	}
	f.staged = append([]byte(nil), data...)
	return len(data), nil
}

func fastUSB(t *testing.T) {
	old := usbHostPoll
	usbHostPoll = time.Millisecond
	t.Cleanup(func() { usbHostPoll = old })
}

// ── port choice ─────────────────────────────────────────────────────────────

func TestUSBHostPortChoice(t *testing.T) {
	for rev, want := range map[string]bool{"v3r4": true, "v3r5": true, "v4": true, "V3R4": true, "v3": false, "v3r3": false, "v2": false, "": false, "analog": false} {
		if got := hasUSBAHost(rev); got != want {
			t.Errorf("hasUSBAHost(%q) = %v, want %v", rev, got, want)
		}
	}
	cases := []struct{ flag, rev, want string }{
		{"", "v3r4", "usb_a"}, {"", "v3", "usb_c"}, {"", "", "usb_c"},
		{"usb_c", "v3r4", "usb_c"}, {"USB_A", "v3r4", "usb_a"}, {"usb_c", "v3", "usb_c"},
	}
	for _, c := range cases {
		got, err := chooseUSBHostPort(c.flag, c.rev)
		if err != nil || got != c.want {
			t.Errorf("choose(%q, %q) = %q, %v; want %q", c.flag, c.rev, got, err, c.want)
		}
	}
	if _, err := chooseUSBHostPort("usb_a", "v3r3"); err == nil || exitCode(err) != exitUsage {
		t.Errorf("usb_a on an old pod: %v", err)
	}
	if _, err := chooseUSBHostPort("usb_b", "v3r4"); err == nil {
		t.Error("want an error for usb_b")
	}
}

// ── file formats ────────────────────────────────────────────────────────────

func hexRec(typ byte, addr uint16, data ...byte) string {
	rec := append([]byte{byte(len(data)), byte(addr >> 8), byte(addr), typ}, data...)
	var sum byte
	for _, b := range rec {
		sum += b
	}
	rec = append(rec, -sum)
	return fmt.Sprintf(":%X\r\n", rec)
}

func TestParseIntelHex(t *testing.T) {
	src := hexRec(0x04, 0, 0x08, 0x00) + hexRec(0, 0x0000, 1, 2, 3, 4) + hexRec(0, 0x0008, 9, 9) +
		hexRec(0x05, 0, 0x08, 0, 0, 1) + hexRec(0x01, 0)
	bin, addr, err := parseIntelHex([]byte(src))
	if err != nil || addr != 0x08000000 {
		t.Fatalf("addr 0x%x, err %v", addr, err)
	}
	want := []byte{1, 2, 3, 4, 0xFF, 0xFF, 0xFF, 0xFF, 9, 9}
	if !bytes.Equal(bin, want) {
		t.Errorf("binary %v, want %v", bin, want)
	}
	// Extended segment addressing.
	bin, addr, err = parseIntelHex([]byte(hexRec(0x02, 0, 0x10, 0x00) + hexRec(0, 0x0010, 7) + hexRec(0x01, 0)))
	if err != nil || addr != 0x10010 || len(bin) != 1 {
		t.Errorf("segment: 0x%x %v %v", addr, bin, err)
	}
}

func TestParseIntelHexErrors(t *testing.T) {
	good := hexRec(0, 0, 1, 2)
	for name, src := range map[string]string{
		"no eof":       good,
		"bad checksum": ":020000000102FF\n" + hexRec(1, 0),
		"no colon":     "00000001FF\n",
		"bad length":   ":0300000001FA\n" + hexRec(1, 0),
		"empty":        hexRec(1, 0),
		"far apart":    hexRec(0, 0, 1) + hexRec(0x04, 0, 0x40, 0) + hexRec(0, 0, 1) + hexRec(1, 0),
	} {
		if _, _, err := parseIntelHex([]byte(src)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// dfuFile builds a DfuSe .dfu file with one target and the given elements, or a plain one.
func dfuFile(elems ...usbSegment) []byte {
	var targ bytes.Buffer
	for _, e := range elems {
		_ = binary.Write(&targ, binary.LittleEndian, e.addr)
		_ = binary.Write(&targ, binary.LittleEndian, uint32(len(e.data)))
		targ.Write(e.data)
	}
	var b bytes.Buffer
	b.WriteString("DfuSe")
	b.Write([]byte{1, 0, 0, 0, 0, 1}) // version, size (patched), targets
	b.WriteString("Target")
	b.Write([]byte{0})
	b.Write([]byte{0, 0, 0, 0})
	b.Write(make([]byte, 255))
	_ = binary.Write(&b, binary.LittleEndian, uint32(targ.Len()))
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(elems)))
	b.Write(targ.Bytes())
	return withDFUSuffix(b.Bytes())
}

func withDFUSuffix(body []byte) []byte {
	out := append([]byte(nil), body...)
	out = append(out, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x1A, 0x01, 'U', 'F', 'D', 16)
	crc := dfuCRC(out)
	return binary.LittleEndian.AppendUint32(out, crc)
}

func TestParseDFUFile(t *testing.T) {
	data, addr, has, err := parseDFUFile(dfuFile(usbSegment{0x08000000, []byte{1, 2}}, usbSegment{0x08000004, []byte{3}}))
	if err != nil || !has || addr != 0x08000000 || !bytes.Equal(data, []byte{1, 2, 0xFF, 0xFF, 3}) {
		t.Fatalf("DfuSe: %v %v 0x%x %v", data, has, addr, err)
	}
	// A plain DFU file: the payload, no address.
	data, _, has, err = parseDFUFile(withDFUSuffix([]byte("payload")))
	if err != nil || has || string(data) != "payload" {
		t.Fatalf("plain: %q %v %v", data, has, err)
	}
	bad := dfuFile(usbSegment{0x08000000, []byte{1}})
	bad[12] ^= 0xFF
	if _, _, _, err := parseDFUFile(bad); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Errorf("bad CRC: %v", err)
	}
	if _, _, _, err := parseDFUFile([]byte("not a dfu file at all")); err == nil {
		t.Error("want an error without a suffix")
	}
}

func TestFAT83Name(t *testing.T) {
	for in, want := range map[string]string{
		"firmware.bin": "FIRMWARE.BIN", "/tmp/My Long Firmware.bin": "MYLONGFI.BIN", "app.hex": "APP.HEX",
		"noext": "NOEXT", "a.firmware": "A.FIR", "weird name!.bin": "WEIRDNAM.BIN", "...": "FIRMWARE",
	} {
		if got := fat83Name(in); got != want {
			t.Errorf("fat83Name(%q) = %q, want %q", in, got, want)
		}
	}
	if !validFAT83("FIRMWARE.BIN") || validFAT83("TOOLONGNAME.BIN") || validFAT83("lower.bin") || validFAT83("A.LONG") {
		t.Error("validFAT83")
	}
}

// ── mode selection ──────────────────────────────────────────────────────────

func TestChooseUSBFlashMode(t *testing.T) {
	esp := []byte{0xE9, 1, 2}
	bin := []byte{0x00, 0x20, 1, 2}
	msc := usbIface(8, 6, 80, "msc")
	cdc := []usbInterface{usbIface(2, 2, 0, "cdc"), usbIface(10, 0, 0, "cdc_data")}
	cases := []struct {
		name string
		file string
		data []byte
		dev  *usbDevice
		want string
	}{
		{"uf2 file on a drive", "app.uf2", bin, usbDev(0x2E8A, 3, "Pico", msc), "uf2"},
		{"uf2 file, no device needed", "app.uf2", bin, nil, "uf2"},
		{"teensy", "app.hex", bin, usbDev(0x16C0, 0x0478, "Teensy", usbIface(3, 0, 0, "hid")), "teensy"},
		{"stm32 rom dfu", "app.bin", bin, usbDev(0x0483, 0xDF11, "DFU in FS Mode", usbIface(0xFE, 1, 2, "")), "dfu"},
		{"dfu by id alone", "app.bin", bin, usbDev(0x0483, 0xDF11, "STM", nil...), "dfu"},
		{"dfu interface", "app.bin", bin, usbDev(0x1209, 1, "boot", usbIface(0xFE, 1, 2, "")), "dfu"},
		{"leonardo", "app.hex", bin, usbDev(0x2341, 0x0036, "Leonardo", cdc...), "avr"},
		{"pro micro", "app.hex", bin, usbDev(0x1B4F, 0x9205, "Pro Micro", cdc...), "avr"},
		{"zero", "app.bin", bin, usbDev(0x2341, 0x004D, "Zero", cdc...), "samd"},
		{"nano 33 iot sketch id", "app.bin", bin, usbDev(0x2341, 0x8057, "Nano 33 IoT", cdc...), "samd"},
		{"esp32-s3 own usb", "app.bin", bin, usbDev(0x303A, 0x1001, "ESP32-S3", cdc...), "esp"},
		{"esp image behind CP210x", "app.bin", esp, usbDev(0x10C4, 0xEA60, "CP2102", usbIface(0xFF, 0, 0, "vendor")), "esp"},
		{"daplink", "app.bin", bin, usbDev(0x0D28, 0x0204, "DAPLink", msc, usbIface(3, 0, 0, "hid"), cdc[0]), "file"},
	}
	for _, c := range cases {
		got, why, err := chooseUSBFlashMode(c.file, c.data, c.dev)
		if err != nil || got != c.want || why == "" {
			t.Errorf("%s: mode %q (%s), %v; want %q", c.name, got, why, err, c.want)
		}
	}
	// Not decidable: a USB-serial chip with a plain binary (AVR/SAMD/ESP all possible), unknown device.
	for name, d := range map[string]*usbDevice{
		"ftdi with a binary": usbDev(0x0403, 0x6001, "FT232", usbIface(0xFF, 0, 0, "vendor")),
		"unknown":            usbDev(0x1234, 0x5678, "Thing", usbIface(3, 0, 0, "hid")),
	} {
		_, _, err := chooseUSBFlashMode("app.bin", bin, d)
		if err == nil || !strings.Contains(err.Error(), "--mode") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := chooseUSBFlashMode("app.bin", bin, nil); err == nil {
		t.Error("no device and no uf2: want an error")
	}
}

func TestPlanUSBFlash(t *testing.T) {
	hexFile := []byte(hexRec(0x04, 0, 0x08, 0x00) + hexRec(0, 0x1000, 1, 2, 3) + hexRec(1, 0))
	dfuse := usbDev(0x0483, 0xDF11, "DFU", usbIface(0xFE, 1, 2, ""))
	plain := usbDev(0x1209, 1, "boot", usbIface(0xFE, 1, 1, ""))
	leo := usbDev(0x2341, 0x0036, "Leonardo")
	zero := usbDev(0x2341, 0x004D, "Zero")
	teensy4 := usbDev(0x16C0, 0x0478, "Teensy")

	p, err := planUSBFlash(usbFlashOpts{file: "app.bin"}, []byte{1, 2, 3}, dfuse)
	if err != nil || p.Mode != "dfu" || !p.HasAddr || p.Address != 0x08000000 {
		t.Errorf("DfuSe .bin: %+v %v", p, err)
	}
	p, _ = planUSBFlash(usbFlashOpts{file: "app.bin", address: "0x08010000"}, []byte{1}, dfuse)
	if p.Address != 0x08010000 {
		t.Errorf("--address: 0x%x", p.Address)
	}
	p, _ = planUSBFlash(usbFlashOpts{file: "app.bin"}, []byte{1}, plain)
	if p.Mode != "dfu" || p.HasAddr {
		t.Errorf("plain DFU takes no address: %+v", p)
	}
	p, err = planUSBFlash(usbFlashOpts{file: "app.hex"}, hexFile, dfuse)
	if err != nil || p.Address != 0x08001000 || !bytes.Equal(p.Data, []byte{1, 2, 3}) {
		t.Errorf("DfuSe .hex: %+v %v", p, err)
	}
	p, _ = planUSBFlash(usbFlashOpts{file: "app.hex"}, []byte(hexRec(0, 0, 5)+hexRec(1, 0)), leo)
	if p.Mode != "avr" || p.Address != 0 || len(p.Data) != 1 {
		t.Errorf("avr .hex: %+v", p)
	}
	p, _ = planUSBFlash(usbFlashOpts{file: "app.bin"}, []byte{1}, zero)
	if p.Mode != "samd" || p.Address != 0x2000 || !strings.Contains(p.Note, "0x4000") {
		t.Errorf("samd default: %+v", p)
	}
	t4 := []byte(hexRec(0x04, 0, 0x60, 0x00) + hexRec(0, 0, 1, 2) + hexRec(1, 0))
	p, _ = planUSBFlash(usbFlashOpts{file: "blink.hex"}, t4, teensy4)
	if p.Mode != "teensy" || p.Address != 0 {
		t.Errorf("teensy 4 hex: %+v", p)
	}
	dfuFileData := dfuFile(usbSegment{0x08004000, []byte{7, 8}})
	p, _ = planUSBFlash(usbFlashOpts{file: "x.dfu"}, dfuFileData, dfuse)
	if p.Address != 0x08004000 || !bytes.Equal(p.Data, []byte{7, 8}) {
		t.Errorf(".dfu: %+v", p)
	}
	// Drive modes send the file as it is, and name it as 8.3.
	raw := []byte("rawhex")
	p, _ = planUSBFlash(usbFlashOpts{file: "/x/firmware-v1.2.hex"}, raw, usbDev(0x0D28, 0x0204, "DAPLink", usbIface(8, 6, 80, "msc")))
	if p.Mode != "file" || p.Name != "FIRMWARE.HEX" || !bytes.Equal(p.Data, raw) || p.HasAddr {
		t.Errorf("file mode: %+v", p)
	}
	p, _ = planUSBFlash(usbFlashOpts{file: "a.bin", name: "APP.BIN", mode: "file"}, raw, nil)
	if p.Name != "APP.BIN" {
		t.Errorf("--name: %+v", p)
	}
	if _, err := planUSBFlash(usbFlashOpts{file: "bad.hex"}, []byte("garbage"), leo); err == nil {
		t.Error("want a hex error")
	}
}

func TestValidateUSBFlashFlags(t *testing.T) {
	for _, o := range []usbFlashOpts{{mode: "bogus"}, {verify: true, mode: "dfu"}, {checkFail: true, mode: "uf2"}, {address: "zz"}, {name: "toolongname.bin"}, {port: "usb_b"}} {
		o := o
		if err := validateUSBFlashFlags(&o); err == nil || exitCode(err) != exitUsage {
			t.Errorf("%+v: %v", o, err)
		}
	}
	o := usbFlashOpts{mode: " DFU ", name: "app.bin", address: "0x10"}
	if err := validateUSBFlashFlags(&o); err != nil || o.mode != "dfu" || o.name != "APP.BIN" {
		t.Errorf("%+v %v", o, err)
	}
}

// ── usb-host ────────────────────────────────────────────────────────────────

func TestRunUSBHostOnAndStatus(t *testing.T) {
	fastUSB(t)
	pod := newFakeUSBPod(t)
	pod.device = usbDev(0x2E8A, 3, "Pico", usbIface(2, 2, 0, "cdc"))
	pod.device.Manufacturer, pod.device.Serial = "Raspberry Pi", "E661"
	var out bytes.Buffer
	if err := runUSBHost(context.Background(), pod, "pod", usbHostOpts{action: "on", timeout: 90 * time.Second}, &out); err != nil {
		t.Fatal(err)
	}
	on := pod.sent("usb_host")[0]
	if on["enabled"] != true || on["port"] != "usb_a" || on["timeout_s"] != 90 {
		t.Errorf("on request %v", on)
	}
	for _, want := range []string{"on, port usb_a", "2E8A:0003 Raspberry Pi Pico", "E661", "cdc"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	// An older pod defaults to usb_c; --port wins.
	old := newFakeUSBPod(t)
	old.status = okStatus("4.0.3", "v3", 14)
	if err := runUSBHost(context.Background(), old, "pod", usbHostOpts{action: "on", timeout: time.Minute}, &out); err != nil || old.sent("usb_host")[0]["port"] != "usb_c" {
		t.Errorf("v3 pod: %v %v", err, old.sent("usb_host"))
	}
	// off, and the raw JSON of a status.
	out.Reset()
	if err := runUSBHost(context.Background(), pod, "pod", usbHostOpts{action: "off"}, &out); err != nil || !strings.Contains(out.String(), "off") {
		t.Errorf("off: %v %q", err, out.String())
	}
	out.Reset()
	if err := runUSBHost(context.Background(), pod, "pod", usbHostOpts{action: "status", asJSON: true}, &out); err != nil || !strings.HasPrefix(out.String(), `{"mode":"device"`) {
		t.Errorf("json: %v %q", err, out.String())
	}
	// Timeout out of range.
	if err := runUSBHost(context.Background(), pod, "pod", usbHostOpts{action: "on", timeout: 2 * time.Hour}, &out); err == nil || exitCode(err) != exitUsage {
		t.Errorf("2h: %v", err)
	}
}

func TestRunUSBHostWiringHints(t *testing.T) {
	var out bytes.Buffer
	printUSBHost(&out, "pod", &usbHostReport{Mode: "host", Port: "usb_a", LineSpeed: "none", Enumeration: "none"})
	if !strings.Contains(out.String(), "no device pull-up") || !strings.Contains(out.String(), "none yet") {
		t.Errorf("nothing connected:\n%s", out.String())
	}
	out.Reset()
	printUSBHost(&out, "pod", &usbHostReport{Mode: "host", Port: "usb_c", LineSpeed: "low", Connected: true, Enumeration: "none"})
	if !strings.Contains(out.String(), "swapped") {
		t.Errorf("low speed:\n%s", out.String())
	}
}

func TestUSBFeatureGate(t *testing.T) {
	for name, status := range map[string]string{
		"old firmware": `{"version":"4.0.2","board_rev":"v3r4"}`,
		"rev 13":       okStatus("4.0.2", "v3r4", 13),
	} {
		pod := newFakeUSBPod(t)
		pod.status = status
		err := runUSBHost(context.Background(), pod, "pod", usbHostOpts{action: "status"}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "needs firmware 4.0.3") || !strings.Contains(err.Error(), "4.0.2") {
			t.Errorf("%s: %v", name, err)
		}
		if len(pod.sent("usb_host")) != 0 {
			t.Errorf("%s: usb_host was sent", name)
		}
		err = runUSBFlash(context.Background(), pod, usbFlashOpts{file: "a.uf2"}, []byte{1}, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "needs firmware 4.0.3") {
			t.Errorf("%s flash: %v", name, err)
		}
	}
}

func TestUSBHostRefusals(t *testing.T) {
	// BP0076: over the pod's own USB link; the catalog's text is shown.
	err := usbHostErr("usb-host", &tcpclient.PodError{Cmd: "usb_host", Code: "BP0076", Reason: "x"})
	if err == nil || !strings.Contains(err.Error(), "BP0076") {
		t.Errorf("BP0076: %v", err)
	}
	err = usbHostErr("usb-host", &tcpclient.PodError{Cmd: "usb_host", Code: "BP0002", Reason: "unknown command"})
	if err == nil || !strings.Contains(err.Error(), "needs firmware 4.0.3") {
		t.Errorf("unknown command: %v", err)
	}
	err = usbHostErr("usb-host", &tcpclient.PodError{Cmd: "usb_host", Code: "BP0077", Args: map[string]any{"port": "usb_a"}, Reason: "x"})
	if err == nil || !strings.Contains(err.Error(), "BP0077") {
		t.Errorf("BP0077: %v", err)
	}
	if err := usbHostErr("usb-host", errors.New("boom")); err == nil || !strings.Contains(err.Error(), "usb-host: boom") {
		t.Errorf("plain: %v", err)
	}
}

// ── usb-flash ───────────────────────────────────────────────────────────────

func runFlashFake(t *testing.T, pod *fakeUSBPod, o usbFlashOpts, data []byte) (string, string, error) {
	t.Helper()
	fastUSB(t)
	var out, prog bytes.Buffer
	o.hostSecs = 360
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runUSBFlash(ctx, pod, o, data, &out, &prog)
	return out.String(), prog.String(), err
}

func TestRunUSBFlashStartsHostAndFlashesUF2(t *testing.T) {
	pod := newFakeUSBPod(t)
	pod.device = usbDev(0x2E8A, 3, "Pico", usbIface(8, 6, 80, "msc"))
	pod.enumAfter = 3
	pod.flashRuns = 5
	pod.flashResult = usbFlashStatus{Mode: "uf2", Phase: "done", Done: 5, Total: 5, Result: "ok", OK: true, MS: 1800}
	data := []byte("uf2 data")
	out, prog, err := runFlashFake(t, pod, usbFlashOpts{file: "/x/app.uf2"}, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prog, "USB host mode was off: starting it on usb_a") || !strings.Contains(prog, "waiting for a USB device") ||
		!strings.Contains(prog, "uf2 mode (a .uf2 file)") || !strings.Contains(prog, "write: ") {
		t.Errorf("progress:\n%s", prog)
	}
	if !strings.Contains(out, "Flashed app.uf2 to 2E8A:0003 Pico over USB, uf2 mode") {
		t.Errorf("result: %q", out)
	}
	if !bytes.Equal(pod.staged, data) {
		t.Errorf("staged %q", pod.staged)
	}
	start := pod.sent("usb_flash")[0]
	if start["mode"] != "uf2" || start["len"] != len(data) || start["address"] != nil || start["name"] != nil || start["leave"] != nil {
		t.Errorf("start request %v", start)
	}
	if on := pod.sent("usb_host")[1]; on["enabled"] != true || on["timeout_s"] != 360 {
		t.Errorf("host start %v", on)
	}
}

func TestRunUSBFlashDFUWithHexNoLeave(t *testing.T) {
	pod := newFakeUSBPod(t)
	pod.mode, pod.port, pod.remain = "host", "usb_a", 3000 // already on: not restarted
	pod.device = usbDev(0x0483, 0xDF11, "DFU", usbIface(0xFE, 1, 2, ""))
	pod.flashResult = usbFlashStatus{Mode: "dfu", Phase: "done", Result: "ok", OK: true}
	hexFile := []byte(hexRec(0x04, 0, 0x08, 0x00) + hexRec(0, 0, 1, 2, 3) + hexRec(1, 0))
	out, prog, err := runFlashFake(t, pod, usbFlashOpts{file: "app.hex", noLeave: true}, hexFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prog, "was off") {
		t.Errorf("host mode was on already:\n%s", prog)
	}
	start := pod.sent("usb_flash")[0]
	if start["mode"] != "dfu" || start["address"] != "0x08000000" || start["leave"] != false || start["len"] != 3 {
		t.Errorf("start %v", start)
	}
	if !bytes.Equal(pod.staged, []byte{1, 2, 3}) || !strings.Contains(out, "stays in its bootloader") {
		t.Errorf("staged %v, out %q", pod.staged, out)
	}
	for _, c := range pod.sent("usb_host") {
		if c["enabled"] == true {
			t.Errorf("host mode restarted: %v", c)
		}
	}
}

func TestRunUSBFlashRenewsShortHostMode(t *testing.T) {
	pod := newFakeUSBPod(t)
	pod.mode, pod.port, pod.remain = "host", "usb_c", 20
	pod.device = usbDev(0x0D28, 0x0204, "DAPLink", usbIface(8, 6, 80, "msc"))
	pod.flashResult = usbFlashStatus{Mode: "file", Phase: "done", Result: "ok", OK: true}
	_, _, err := runFlashFake(t, pod, usbFlashOpts{file: "fw.bin"}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	renew := pod.sent("usb_host")[1]
	if renew["enabled"] != true || renew["port"] != "usb_c" || renew["timeout_s"] != 360 {
		t.Errorf("renew %v", renew)
	}
	if st := pod.sent("usb_flash")[0]; st["mode"] != "file" || st["name"] != "FW.BIN" {
		t.Errorf("file start %v", st)
	}
}

func TestRunUSBFlashFailureShowsPhaseAndReason(t *testing.T) {
	pod := newFakeUSBPod(t)
	pod.device = usbDev(0x0483, 0xDF11, "DFU", usbIface(0xFE, 1, 2, ""))
	pod.flashResult = usbFlashStatus{Mode: "dfu", Phase: "erase", Done: 2, Total: 9,
		Result: "the image does not fit the loader's flash layout"}
	_, _, err := runFlashFake(t, pod, usbFlashOpts{file: "app.bin"}, []byte{1})
	if err == nil || !strings.Contains(err.Error(), "erase phase") || !strings.Contains(err.Error(), "does not fit the loader's flash layout") ||
		!strings.Contains(err.Error(), "--address") {
		t.Fatalf("err = %v", err)
	}
	if exitCode(err) == exitOK {
		t.Error("exit code must be non-zero")
	}
}

func TestUSBFlashFailureMessages(t *testing.T) {
	cases := map[string]string{
		"the file is not a UF2 file":                "--mode file",
		"the board has no DFU interface":            "BOOT0",
		"no bootloader answered on the serial port": "bootloader",
		"the board is not a Teensy in its loader":   "button",
		"the image would overwrite the bootloader":  "--address",
		"aborted": "was aborted",
	}
	for reason, want := range cases {
		msg := usbFlashFailure("usb-flash", "f.bin", &usbFlashStatus{Phase: "write", Mode: "avr", Result: reason})
		if !strings.Contains(msg, want) {
			t.Errorf("%q: %s", reason, msg)
		}
	}
}

func TestRunUSBFlashRefusalsAndStageErrors(t *testing.T) {
	pod := newFakeUSBPod(t)
	pod.device = usbDev(0x2E8A, 3, "Pico", usbIface(8, 6, 80, "msc"))
	pod.startErr = &tcpclient.PodError{Cmd: "usb_flash", Code: "BP0084", Args: map[string]any{"why": "no drive mounted"}, Reason: "refused"}
	_, _, err := runFlashFake(t, pod, usbFlashOpts{file: "a.uf2"}, []byte{1})
	if err == nil || !strings.Contains(err.Error(), "BP0084") || !strings.Contains(err.Error(), "the pod refused") {
		t.Errorf("BP0084: %v", err)
	}
	pod = newFakeUSBPod(t)
	pod.device = usbDev(0x2E8A, 3, "Pico", usbIface(8, 6, 80, "msc"))
	pod.stageErr = errors.New("connection reset")
	if _, _, err = runFlashFake(t, pod, usbFlashOpts{file: "a.uf2"}, []byte{1}); err == nil || !strings.Contains(err.Error(), "uploading a.uf2") {
		t.Errorf("stage: %v", err)
	}
	if len(pod.sent("usb_flash")) != 0 {
		t.Error("flash started after a failed upload")
	}
	// A board the CLI cannot place, without --mode: nothing is staged.
	pod = newFakeUSBPod(t)
	pod.device = usbDev(0x1234, 5, "Thing", usbIface(3, 0, 0, "hid"))
	if _, _, err = runFlashFake(t, pod, usbFlashOpts{file: "a.bin"}, []byte{1}); err == nil || !strings.Contains(err.Error(), "--mode") || pod.staged != nil {
		t.Errorf("unknown device: %v", err)
	}
	// ... which --mode resolves.
	pod.flashResult = usbFlashStatus{Mode: "dfu", Phase: "done", Result: "ok", OK: true}
	if _, _, err = runFlashFake(t, pod, usbFlashOpts{file: "a.bin", mode: "dfu"}, []byte{1}); err != nil || pod.sent("usb_flash")[0]["mode"] != "dfu" {
		t.Errorf("--mode dfu: %v", err)
	}
}

func TestRunUSBFlashNoDeviceTimesOut(t *testing.T) {
	fastUSB(t)
	pod := newFakeUSBPod(t) // no device
	var out, prog bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pod.cancel, pod.cancelHostAt = cancel, 20 // ends the wait after a fixed number of polls, not after a time
	err := runUSBFlash(ctx, pod, usbFlashOpts{file: "a.uf2", hostSecs: 100}, []byte{1}, &out, &prog)
	if err == nil || !strings.Contains(err.Error(), "no USB device enumerated") || !strings.Contains(err.Error(), "pull-up") {
		t.Errorf("err = %v", err)
	}
	if pod.staged != nil {
		t.Error("staged without a device")
	}
}

func TestRunUSBFlashAbortsWhenCancelled(t *testing.T) {
	fastUSB(t)
	pod := newFakeUSBPod(t)
	pod.device = usbDev(0x2E8A, 3, "Pico", usbIface(8, 6, 80, "msc"))
	pod.flashRuns = 1 << 30
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pod.cancel, pod.cancelFlashAt = cancel, 20
	err := runUSBFlash(ctx, pod, usbFlashOpts{file: "a.uf2", hostSecs: 100}, []byte{1}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("err = %v", err)
	}
	aborted := false
	for _, c := range pod.sent("usb_flash") {
		aborted = aborted || c["abort"] == true
	}
	if !aborted {
		t.Error("no abort sent")
	}
}

func TestRunUSBFlashStuckEnumeration(t *testing.T) {
	fastUSB(t)
	pod := newFakeUSBPod(t)
	pod.mode, pod.port, pod.remain = "host", "usb_a", 3000
	// A pod whose usb_host always reports a stuck enumeration.
	stuck := &stuckPod{fakeUSBPod: pod}
	err := runUSBFlash(context.Background(), stuck, usbFlashOpts{file: "a.uf2", hostSecs: 100}, []byte{1}, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "did not finish enumerating") {
		t.Errorf("err = %v", err)
	}
}

type stuckPod struct{ *fakeUSBPod }

func (s *stuckPod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	if req["cmd"] == "usb_host" && req["enabled"] == nil {
		return json.RawMessage(`{"mode":"host","port":"usb_a","remaining_s":300,"attached":true,"enumeration":"stuck","connected":true,"line_speed":"full"}`), nil
	}
	return s.fakeUSBPod.Command(ctx, req)
}

func TestUSBCommandsAreRegistered(t *testing.T) {
	root := newRootCmd()
	for _, name := range []string{"usb-host", "usb-flash", "usb-check", "usb-hid"} {
		c, _, err := root.Find([]string{name})
		if err != nil || c.Name() != name {
			t.Errorf("%s not registered: %v", name, err)
		}
	}
}
