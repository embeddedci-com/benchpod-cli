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

func TestProgImages(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "a.bin")
	_ = os.WriteFile(bin, []byte{1, 2, 3}, 0o644)
	hexf := filepath.Join(dir, "a.hex")
	_ = os.WriteFile(hexf, []byte(":0400100001020304E2\n:00000001FF\n"), 0o644)
	r, err := progImages(bin, "0x100")
	if err != nil || len(r) != 1 || r[0].addr != 0x100 || len(r[0].data) != 3 {
		t.Fatalf("bin: %v %+v", err, r)
	}
	r, err = progImages(hexf, "")
	if err != nil || len(r) != 1 || r[0].addr != 0x10 || len(r[0].data) != 4 {
		t.Fatalf("hex: %v %+v", err, r)
	}
	if _, err := progImages("", ""); err == nil || !strings.Contains(err.Error(), "--file") {
		t.Errorf("want the --file error, got %v", err)
	}
	if _, err := progImages(filepath.Join(dir, "x.elf"), ""); err == nil {
		t.Error("an .elf must be refused")
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
	p := &progFlashFlags{}
	pins, err := p.progPins("updi", w)
	if err != nil || pins["data"] != 7 || len(pins) != 1 {
		t.Fatalf("updi from the profile: %v %v", pins, err)
	}
	if _, err := (&progFlashFlags{}).progPins("c2", nil); err == nil || !strings.Contains(err.Error(), "--data") {
		t.Fatalf("want --data error, got %v", err)
	}
	if _, err := (&progFlashFlags{data: "3"}).progPins("c2", nil); err == nil || !strings.Contains(err.Error(), "--clk") {
		t.Fatalf("want --clk error, got %v", err)
	}
	pins, err = (&progFlashFlags{clk: "la3", data: "4"}).progPins("pic", nil)
	if err != nil || pins["clk"] != 3 || pins["data"] != 4 || pins["reset"] != "nrst" {
		t.Fatalf("pic flags: %v %v", pins, err)
	}
	pins, _ = (&progFlashFlags{clk: "3", data: "4", reset: "9"}).progPins("tpi", nil)
	if pins["reset"] != 9 {
		t.Fatalf("tpi reset pin: %v", pins)
	}
	w = &podWiring{}
	sig(w, "MCLR", 8)
	pins, _ = (&progFlashFlags{clk: "3", data: "4"}).progPins("pic", w)
	if pins["reset"] != 8 {
		t.Fatalf("MCLR signal: %v", pins)
	}
	if _, err := (&progFlashFlags{clk: "3", data: "4", reset: "5"}).progPins("c2", nil); err == nil {
		t.Fatal("--reset on c2 must be refused")
	}
	if _, err := (&progFlashFlags{clk: "3", data: "3"}).progPins("c2", nil); err == nil {
		t.Fatal("clk == data must be refused")
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
	if req["cmd"] != "prog_flash" {
		return errReply("unknown cmd")
	}
	switch req["op"] {
	case "info", "start":
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

func TestFlashProgStart(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "fw.bin")
	_ = os.WriteFile(img, make([]byte, 100), 0o644)
	pod := &progPod{id: "1e9422", result: "ok", polls: 2}
	g := progTestSetup(t, pod)
	f := &flashFlags{target: "updi", file: img, prog: progFlashFlags{device: "attiny1614", data: "5"}}
	out, err := runProg(t, g, f)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.Join(pod.ops(), ","); !strings.HasPrefix(got, "load_bin,prog_flash:start,prog_flash:status") {
		t.Fatalf("calls: %s", got)
	}
	r := pod.req("start")
	if r["proto"] != "updi" || r["data"] != 5.0 || r["len"] != 100.0 || r["page"] != 64.0 ||
		r["expect_id"] != "1e9422" || r["erase"] != "page" || r["mem"] != "flash" || r["run"] != true {
		t.Fatalf("start request: %v", r)
	}
	if !strings.Contains(out, "verified") || !strings.Contains(out, "100 bytes") {
		t.Errorf("output: %s", out)
	}
}

func TestFlashProgInfo(t *testing.T) {
	pod := &progPod{id: "1e9422", result: "ok"}
	g := progTestSetup(t, pod)
	out, err := runProg(t, g, &flashFlags{target: "avr-updi", prog: progFlashFlags{info: true, data: "5"}})
	if err != nil || !strings.Contains(out, "1e9422") || !strings.Contains(out, "attiny1614") {
		t.Fatalf("%v: %s", err, out)
	}
	for _, o := range pod.ops() {
		if o == "load_bin" || o == "prog_flash:start" {
			t.Fatalf("--info wrote: %v", pod.ops())
		}
	}
}

func TestFlashProgFindsDevice(t *testing.T) {
	img := filepath.Join(t.TempDir(), "fw.bin")
	_ = os.WriteFile(img, make([]byte, 64), 0o644)
	pod := &progPod{id: "1e9422", result: "ok"}
	g := progTestSetup(t, pod)
	_, err := runProg(t, g, &flashFlags{target: "updi", file: img, prog: progFlashFlags{data: "5"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pod.ops(), ","); !strings.HasPrefix(got, "prog_flash:info") {
		t.Fatalf("expected an info first: %s", got)
	}
	if r := pod.req("start"); r["page"] != 64.0 {
		t.Fatalf("page from the device found by ID: %v", r)
	}
	pod2 := &progPod{id: "ffff", result: "ok"}
	g = progTestSetup(t, pod2)
	if _, err = runProg(t, g, &flashFlags{target: "updi", file: img, prog: progFlashFlags{data: "5"}}); err == nil || !strings.Contains(err.Error(), "--device") {
		t.Fatalf("an unknown ID must ask for --device, got %v", err)
	}
}

func TestFlashProgErrors(t *testing.T) {
	img := filepath.Join(t.TempDir(), "fw.bin")
	_ = os.WriteFile(img, make([]byte, 64), 0o644)
	for res, fix := range map[string]string{
		"no answer (wiring, power or I/O voltage)": "check the wiring",
		"locked (erase the chip to unlock it)":     "--erase chip",
		"wrong chip (the ID is not expect_id)":     "--info",
	} {
		pod := &progPod{id: "1e9422", result: res}
		g := progTestSetup(t, pod)
		_, err := runProg(t, g, &flashFlags{target: "updi", file: img, prog: progFlashFlags{device: "attiny1614", data: "5"}})
		if err == nil || !strings.Contains(err.Error(), res) || !strings.Contains(err.Error(), fix) {
			t.Errorf("%q: got %v", res, err)
		}
	}
	// An image larger than the chip's flash is refused before anything is staged.
	big := filepath.Join(t.TempDir(), "big.bin")
	_ = os.WriteFile(big, make([]byte, 1<<20), 0o644)
	pod := &progPod{id: "1e9422", result: "ok"}
	g := progTestSetup(t, pod)
	if _, err := runProg(t, g, &flashFlags{target: "updi", file: big, prog: progFlashFlags{device: "attiny1614", data: "5"}}); err == nil || !strings.Contains(err.Error(), "past the") {
		t.Errorf("want too-big error, got %v", err)
	}
	if len(pod.ops()) != 0 {
		t.Errorf("the pod was touched: %v", pod.ops())
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
