package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/progdev"
)

func TestIsProgTarget(t *testing.T) {
	for _, s := range []string{"avr-updi", "updi", "PIC", "efm8", "c2", "attiny-tpi", "tpi", "xmega", "pdi"} {
		if !isProgTarget(s) {
			t.Errorf("%q should be a prog target", s)
		}
	}
	for _, s := range []string{"", "esp32", "target/stm32f4x.cfg"} {
		if isProgTarget(s) {
			t.Errorf("%q should not be a prog target", s)
		}
	}
	if progdev.Proto("efm8") != "c2" || progdev.Proto("xmega") != "pdi" {
		t.Error("alias mapping")
	}
}

func TestProgPins(t *testing.T) {
	sig := func(w *podWiring, name string, la int) {
		w.Signals = append(w.Signals, struct {
			Name string `json:"name"`
			LA   int    `json:"la"`
		}{name, la})
	}
	w := &podWiring{}
	sig(w, "UPDI", 7)
	pins, text, err := (&progFlashFlags{}).progPins("updi", w, false, false)
	if err != nil || pins["data"] != 7 || len(pins) != 1 || !strings.Contains(text, "LA7") {
		t.Fatalf("updi from the profile: %v %q %v", pins, text, err)
	}
	if _, _, err := (&progFlashFlags{clk: "2"}).progPins("updi", w, false, false); err == nil {
		t.Fatal("--clk on updi must be refused")
	}
	if _, _, err := (&progFlashFlags{}).progPins("c2", nil, false, false); err == nil || !strings.Contains(err.Error(), "--data") {
		t.Fatalf("want --data error, got %v", err)
	}
	// the profile's SWD pins are the last fallback, as in the SDK
	one, two := 1, 2
	pins, _, err = (&progFlashFlags{}).progPins("c2", &podWiring{SwdSwclk: &one, SwdSwdio: &two}, false, false)
	if err != nil || pins["clk"] != 1 || pins["data"] != 2 {
		t.Fatalf("swd fallback: %v %v", pins, err)
	}
	pins, _, err = (&progFlashFlags{clk: "la3", data: "4"}).progPins("pic", nil, false, false)
	if err != nil || pins["clk"] != 3 || pins["data"] != 4 || pins["reset"] != "nrst" {
		t.Fatalf("pic flags: %v %v", pins, err)
	}
	pins, _, _ = (&progFlashFlags{clk: "3", data: "4", reset: "9"}).progPins("tpi", nil, false, false)
	if pins["reset"] != 9 {
		t.Fatalf("tpi reset pin: %v", pins)
	}
	w = &podWiring{}
	sig(w, "MCLR", 8)
	if pins, _, _ = (&progFlashFlags{clk: "3", data: "4"}).progPins("pic", w, false, false); pins["reset"] != 8 {
		t.Fatalf("MCLR signal: %v", pins)
	}
	if pins, _, _ = (&progFlashFlags{clk: "3", data: "4"}).progPins("pic", w, true, true); pins["reset"] != "nrst" {
		t.Fatalf("--nreset wins over the MCLR signal: %v", pins)
	}
	if _, _, err := (&progFlashFlags{clk: "3", data: "4"}).progPins("pic", nil, false, true); err == nil {
		t.Fatal("--nreset=false with no MCLR signal must be refused")
	}
	if _, _, err := (&progFlashFlags{clk: "3", data: "4", reset: "5"}).progPins("c2", nil, false, false); err == nil {
		t.Fatal("--reset on c2 must be refused")
	}
	if _, _, err := (&progFlashFlags{clk: "3", data: "3"}).progPins("c2", nil, false, false); err == nil {
		t.Fatal("clk == data must be refused")
	}
	if _, _, err := (&progFlashFlags{clk: "3", data: "4", reset: "4"}).progPins("pic", nil, false, false); err == nil {
		t.Fatal("reset == data must be refused")
	}
}

// progPod is a pod's prog_flash API: load_bin psram, then start/info run for `polls` status calls.
type progPod struct {
	mu     sync.Mutex
	reqs   []map[string]any
	staged int
	id     string
	result string
	polls  int
	left   int
	phase  string
	mv     int  // la_voltage
	dirErr bool // BP0034 on info and start
	rev    int  // features_rev in status (0: the pod has no features table, so no prog_named)
}

func (p *progPod) start(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				var req map[string]any
				_ = json.Unmarshal([]byte(line), &req)
				if req["cmd"] == "load_bin" {
					n := int(req["total"].(float64))
					fmt.Fprintln(conn, okReply(map[string]any{}))
					_, _ = io.CopyN(io.Discard, r, int64(n))
					p.mu.Lock()
					p.staged = n
					p.reqs = append(p.reqs, req)
					p.mu.Unlock()
					fmt.Fprintln(conn, okReply(map[string]any{"total": n}))
					return
				}
				fmt.Fprintln(conn, p.handle(req))
			}()
		}
	}()
	return ln.Addr().String()
}

func (p *progPod) handle(req map[string]any) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	switch req["cmd"] {
	case "la_voltage":
		return okReply(map[string]any{"mv": p.mv})
	case "status":
		if p.rev == 0 {
			return errReply("unknown cmd")
		}
		return okReply(map[string]any{"features_rev": p.rev, "gateware": 68, "version": "4.0.6", "api": 2, "min_client_api": 2})
	case "fpga_image":
		return okReply(map[string]any{"image": 1, "name": "deep"})
	case "prog_flash":
	default:
		return errReply("unknown cmd")
	}
	switch req["op"] {
	case "info", "start":
		if p.dirErr {
			return codeReply("BP0034", map[string]any{"la": 5, "function": "prog_flash"})
		}
		p.left = p.polls
		p.phase = "write"
		return okReply(map[string]any{"started": true})
	case "status":
		if p.left > 0 {
			p.left--
			return okReply(map[string]any{"running": true, "phase": p.phase, "done": 1, "total": 2})
		}
		var res any = p.result
		return okReply(map[string]any{"running": false, "phase": p.phase, "done": 2, "total": 2, "id": p.id, "result": res})
	}
	return okReply(map[string]any{})
}

func (p *progPod) ops() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.reqs {
		c, _ := r["cmd"].(string)
		if op, ok := r["op"].(string); ok {
			c += ":" + op
		}
		out = append(out, c)
	}
	return out
}

func (p *progPod) allReqs() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.reqs...)
}

func (p *progPod) req(op string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.reqs {
		if r["op"] == op {
			return r
		}
	}
	return nil
}

func progTestSetup(t *testing.T, pod *progPod) *globalFlags {
	t.Helper()
	old := loadPodWiring
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring { return nil }
	t.Cleanup(func() { loadPodWiring = old })
	return &globalFlags{connection: pod.start(t)}
}

func runProg(t *testing.T, g *globalFlags, f *flashFlags) (string, error) {
	t.Helper()
	spec, err := g.resolveTarget()
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	out := captureStdout(t, func() { runErr = runFlashProg(g, f, spec) })
	return out, runErr
}

func tmpImage(t *testing.T, name string, data []byte) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func progFlags(target, file string, p progFlashFlags) *flashFlags {
	return &flashFlags{target: target, file: file, prog: p}
}

func TestFlashProgUPDI(t *testing.T) {
	img := tmpImage(t, "fw.bin", make([]byte, 100))
	pod := &progPod{id: "1e9422", result: "ok", polls: 1, mv: 3300}
	g := progTestSetup(t, pod)
	out, err := runProg(t, g, progFlags("updi", img, progFlashFlags{data: "5"}))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.Join(pod.ops(), ","); !strings.Contains(got, "prog_flash:info") || !strings.Contains(got, "load_bin,prog_flash:start") {
		t.Fatalf("calls: %s", got)
	}
	r := pod.req("start")
	// 100 bytes pad to a 64-byte page boundary: 128 bytes; chip erase, ID from the table, no baud
	if r["proto"] != "updi" || r["data"] != 5.0 || r["len"] != 128.0 || r["page"] != 64.0 || r["flags"] != 32768.0 ||
		r["expect_id"] != "1e9422" || r["erase"] != "chip" || r["mem"] != "flash" || r["addr"] != 0.0 || r["run"] != true {
		t.Fatalf("start request: %v", r)
	}
	if _, has := r["baud"]; has {
		t.Fatal("baud must not be sent")
	}
	if !strings.Contains(out, "ATtiny1614") || !strings.Contains(out, "verified") {
		t.Errorf("output: %s", out)
	}
}

func TestFlashProgHexRegions(t *testing.T) {
	// flash byte at 0 and a fuse byte at 0x820000 (avr-gcc layout): two regions, chip erase once
	img := tmpImage(t, "a.hex", []byte(hexRec(0, 0, 1)+hexRec(4, 0, 0x00, 0x82)+hexRec(0, 0, 5)+hexRec(1, 0)))
	pod := &progPod{id: "1e9422", result: "ok", mv: 3300}
	g := progTestSetup(t, pod)
	if out, err := runProg(t, g, progFlags("avr-updi", img, progFlashFlags{data: "5"})); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var starts []map[string]any
	for _, r := range pod.allReqs() {
		if r["op"] == "start" {
			starts = append(starts, r)
		}
	}
	if len(starts) != 2 || starts[0]["mem"] != "flash" || starts[0]["erase"] != "chip" || starts[0]["run"] != false ||
		starts[1]["mem"] != "config" || starts[1]["erase"] != "none" || starts[1]["run"] != true || starts[1]["len"] != 1.0 {
		t.Fatalf("starts: %v", starts)
	}
}

func TestFlashProgPICSendsCPAddr(t *testing.T) {
	img := tmpImage(t, "fw.bin", make([]byte, 64))
	pod := &progPod{id: "7030", result: "ok", mv: 3300}
	g := progTestSetup(t, pod)
	if out, err := runProg(t, g, progFlags("pic", img, progFlashFlags{clk: "3", data: "4"})); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	r := pod.req("start")
	if r["cp_addr"] != 65558.0 || r["flags"] != 1.0 || r["reset"] != "nrst" || r["clk"] != 3.0 {
		t.Fatalf("start request: %v", r)
	}
	if _, has := r["baud"]; has {
		t.Fatal("baud must not be sent")
	}
}

// A pod with the prog_named feature (features_rev 20, firmware 4.0.6) gets the chip's options as
// named fields; an older pod, and one that does not say, gets the packed flags.
func TestFlashProgNamedFieldsOnlyForPodsThatHaveThem(t *testing.T) {
	for _, c := range []struct {
		rev   int
		named bool
	}{{20, true}, {19, false}, {0, false}} {
		img := tmpImage(t, "fw.bin", make([]byte, 64))
		pod := &progPod{id: "7030", result: "ok", mv: 3300, rev: c.rev}
		g := progTestSetup(t, pod)
		if out, err := runProg(t, g, progFlags("pic", img, progFlashFlags{clk: "3", data: "4"})); err != nil {
			t.Fatalf("rev %d: %v\n%s", c.rev, err, out)
		}
		for _, r := range pod.allReqs() {
			if r["cmd"] != "prog_flash" || (r["op"] != "start" && r["op"] != "info") {
				continue
			}
			if r["op"] == "info" && r["cp_addr"] == nil && r["flags"] == nil && r["cmd_set"] == nil {
				continue // the first probe of a PIC without a name: the plain 6-bit style sends nothing
			}
			_, hasFlags := r["flags"]
			if c.named && r["op"] == "info" && r["cp_addr"] == nil { // a probe: the 6-bit style, named
				if hasFlags || r["cmd_set"] != "6bit" {
					t.Errorf("rev %d probe: %v", c.rev, r)
				}
				continue
			}
			if c.named {
				if hasFlags || r["cp_addr"] != 65558.0 || r["cmd_set"] != "8bit" || r["cp_bit"] != 0.0 {
					t.Errorf("rev %d %s: want named fields, got %v", c.rev, r["op"], r)
				}
			} else if r["flags"] != 1.0 || r["cp_addr"] != 65558.0 || r["cmd_set"] != nil {
				t.Errorf("rev %d %s: want flags 1, got %v", c.rev, r["op"], r)
			}
		}
	}
}

func TestFlashProgInfo(t *testing.T) {
	pod := &progPod{id: "1e9422", result: "ok", mv: 3300}
	g := progTestSetup(t, pod)
	out, err := runProg(t, g, progFlags("avr-updi", "", progFlashFlags{info: true, data: "5"}))
	if err != nil || !strings.Contains(out, "1e9422") || !strings.Contains(out, "ATtiny1614") || !strings.Contains(out, "nothing was written") {
		t.Fatalf("%v: %s", err, out)
	}
	for _, o := range pod.ops() {
		if o == "load_bin" || o == "prog_flash:start" {
			t.Fatalf("--info wrote: %v", pod.ops())
		}
	}
}

func TestFlashProgDeviceChecks(t *testing.T) {
	img := tmpImage(t, "fw.bin", make([]byte, 64))
	// an ID that is not in the table
	pod := &progPod{id: "ffffff", result: "ok", mv: 3300}
	g := progTestSetup(t, pod)
	if _, err := runProg(t, g, progFlags("updi", img, progFlashFlags{data: "5"})); err == nil || !strings.Contains(err.Error(), "not in the") {
		t.Fatalf("unknown ID: %v", err)
	}
	// --device that is another chip than the one answering
	pod = &progPod{id: "1e9422", result: "ok", mv: 3300}
	g = progTestSetup(t, pod)
	if _, err := runProg(t, g, progFlags("updi", img, progFlashFlags{data: "5", device: "AVR64DA28"})); err == nil || !strings.Contains(err.Error(), "which is not a") {
		t.Fatalf("wrong --device: %v", err)
	}
	if _, err := runProg(t, g, progFlags("updi", img, progFlashFlags{data: "5", device: "nonesuch"})); err == nil || !strings.Contains(err.Error(), "not in the") {
		t.Fatalf("unknown --device: %v", err)
	}
	// an image past the chip's memories is refused before anything is staged
	big := tmpImage(t, "big.bin", make([]byte, 1<<20))
	pod = &progPod{id: "1e9422", result: "ok", mv: 3300}
	g = progTestSetup(t, pod)
	if _, err := runProg(t, g, progFlags("updi", big, progFlashFlags{data: "5"})); err == nil || !strings.Contains(err.Error(), "not in any memory") {
		t.Fatalf("too big: %v", err)
	}
	for _, o := range pod.ops() {
		if o == "load_bin" {
			t.Fatal("staged an image that does not fit")
		}
	}
}

// The 5 V only TPI chips are refused at the pod's 1.8 and 3.3 V, before anything is written.
func TestFlashProgTPI5V(t *testing.T) {
	img := tmpImage(t, "fw.bin", make([]byte, 32))
	for _, mv := range []int{1800, 3300} {
		pod := &progPod{id: "1e9003", result: "ok", mv: mv}
		g := progTestSetup(t, pod)
		_, err := runProg(t, g, progFlags("tpi", img, progFlashFlags{clk: "3", data: "4"}))
		if err == nil || !strings.Contains(err.Error(), "ATtiny10") || !strings.Contains(err.Error(), "5 V") || !strings.Contains(err.Error(), "level shifter") {
			t.Fatalf("%d mV: %v", mv, err)
		}
		for _, o := range pod.ops() {
			if o == "load_bin" || o == "prog_flash:start" {
				t.Fatalf("wrote at %d mV: %v", mv, pod.ops())
			}
		}
	}
	// the ATtiny102 (1.8-5.5 V) is allowed
	pod := &progPod{id: "1e900c", result: "ok", mv: 3300}
	g := progTestSetup(t, pod)
	if out, err := runProg(t, g, progFlags("tpi", img, progFlashFlags{clk: "3", data: "4"})); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestFlashProgDirectionRefusal(t *testing.T) {
	pod := &progPod{id: "1e9422", result: "ok", mv: 3300, dirErr: true}
	g := progTestSetup(t, pod)
	_, err := runProg(t, g, progFlags("updi", "", progFlashFlags{info: true, data: "5"}))
	if err == nil || !strings.Contains(err.Error(), "LA5") || !strings.Contains(err.Error(), "bidirectional") || !strings.Contains(err.Error(), "fix the wiring") {
		t.Fatalf("BP0034: %v", err)
	}
}

func TestFlashProgFailures(t *testing.T) {
	img := tmpImage(t, "fw.bin", make([]byte, 64))
	for res, fix := range map[string]string{
		"no answer (wiring, power or I/O voltage)": "Check the wiring (data on LA5)",
		"locked (erase the chip to unlock it)":     "--erase chip",
		"wrong chip (the ID is not expect_id)":     "--device",
		"verify failed":                            "read back different",
	} {
		pod := &progPod{id: "1e9422", result: res, mv: 3300}
		g := progTestSetup(t, pod)
		_, err := runProg(t, g, progFlags("updi", img, progFlashFlags{data: "5"}))
		if err == nil || !strings.Contains(err.Error(), res) || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(fix)) {
			t.Errorf("%q: got %v", res, err)
		}
	}
	pod := &progPod{id: "1e9422", result: "ok", mv: 3300}
	g := progTestSetup(t, pod)
	if _, err := runProg(t, g, progFlags("updi", img, progFlashFlags{data: "5", erase: "sometimes"})); err == nil {
		t.Error("a bad --erase must be refused")
	}
	if _, err := runProg(t, g, progFlags("updi", "", progFlashFlags{data: "5"})); err == nil || !strings.Contains(err.Error(), "--file") {
		t.Errorf("want the --file error, got %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	return <-done
}

// ── MSP430 Spy-Bi-Wire ──────────────────────────────────────────────────────

func TestSBWTargetsAndPins(t *testing.T) {
	for _, s := range []string{"msp430", "SBW"} {
		if !isProgTarget(s) || progdev.Proto(s) != "sbw" {
			t.Errorf("%q should map to sbw", s)
		}
	}
	w := &podWiring{}
	for _, s := range []struct {
		n  string
		la int
	}{{"TEST", 9}, {"RST", 8}} {
		w.Signals = append(w.Signals, struct {
			Name string `json:"name"`
			LA   int    `json:"la"`
		}{s.n, s.la})
	}
	pins, text, err := (&progFlashFlags{}).progPins("sbw", w, false, false)
	if err != nil || pins["clk"] != 9 || pins["data"] != 8 || len(pins) != 2 || !strings.Contains(text, "LA9") {
		t.Fatalf("sbw pins from TEST and RST: %v %q %v", pins, text, err)
	}
	if _, ok := pins["reset"]; ok {
		t.Fatal("sbw has no reset pin")
	}
	if _, _, err := (&progFlashFlags{reset: "5", clk: "3", data: "4"}).progPins("sbw", nil, false, false); err == nil {
		t.Fatal("--reset on sbw must be refused")
	}
	if progPerByte("sbw") <= progPerByte("updi") || 60*time.Second+15360*progPerByte("sbw") < 300*time.Second {
		t.Fatal("sbw needs a longer per-byte allowance")
	}
}

func TestFlashProgSBW(t *testing.T) {
	img := tmpImage(t, "fw.bin", make([]byte, 32))
	pod := &progPod{id: "984082", result: "ok", polls: 1, mv: 3300}
	g := progTestSetup(t, pod)
	out, err := runProg(t, g, progFlags("msp430", img, progFlashFlags{clk: "4", data: "3"}))
	if err != nil || !strings.Contains(out, "MSP430FR2433") {
		t.Fatalf("%v: %s", err, out)
	}
	info, start := pod.req("info"), pod.req("start")
	if info["proto"] != "sbw" || info["clk"] != 4.0 || info["data"] != 3.0 {
		t.Fatalf("info: %v", info)
	}
	if start["proto"] != "sbw" || start["mem"] != "flash" || start["addr"] != 51200.0 || start["expect_id"] != "984082" {
		t.Fatalf("start: %v", start)
	}
	if _, ok := start["reset"]; ok {
		t.Fatalf("sbw start has a reset pin: %v", start)
	}
}

func TestFlashProgSBWLockedChip(t *testing.T) {
	img := tmpImage(t, "fw.bin", make([]byte, 32))
	pod := &progPod{id: "98", result: "ok", mv: 3300}
	g := progTestSetup(t, pod)
	_, err := runProg(t, g, progFlags("msp430", img, progFlashFlags{clk: "4", data: "3"}))
	if err == nil || !strings.Contains(err.Error(), "JTAG-locked") {
		t.Fatalf("locked chip without --device: %v", err)
	}
	pod = &progPod{id: "98", result: "ok", mv: 3300}
	g = progTestSetup(t, pod)
	if _, err := runProg(t, g, progFlags("msp430", img, progFlashFlags{clk: "4", data: "3", device: "MSP430FR2433"})); err != nil {
		t.Fatalf("locked chip with --device: %v", err)
	}
	if pod.req("start")["erase"] != "chip" {
		t.Fatalf("erase: %v", pod.req("start"))
	}
}
