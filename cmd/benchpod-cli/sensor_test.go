package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/embeddedci-com/benchpod-cli/internal/sensors"
)

// fakeSensorPod plays the draft sensor commands on a LAN port, one JSON command per connection.
type fakeSensorPod struct {
	mu       sync.Mutex
	caps     []string
	requests []map[string]any
	uploads  map[string][]byte
	reply    func(req map[string]any) (any, string) // override: data, error message
}

func newFakeSensorPod(t *testing.T, caps ...string) (*fakeSensorPod, *globalFlags) {
	t.Helper()
	p := &fakeSensorPod{caps: caps, uploads: map[string][]byte{}}
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
			line, _ := bufio.NewReader(conn).ReadString('\n')
			var req map[string]any
			_ = json.Unmarshal([]byte(line), &req)
			data, msg := p.handle(req)
			var out []byte
			if msg != "" {
				out, _ = json.Marshal(map[string]any{"status": "error", "message": msg})
			} else {
				out, _ = json.Marshal(map[string]any{"status": "ok", "data": data})
			}
			_, _ = conn.Write(append(out, '\n'))
			conn.Close()
		}
	}()
	old := loadPodWiring
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring { return nil }
	t.Cleanup(func() { loadPodWiring = old })
	return p, &globalFlags{connection: ln.Addr().String(), configFile: filepath.Join(t.TempDir(), "config.json")}
}

func (p *fakeSensorPod) handle(req map[string]any) (any, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.reply != nil {
		if d, msg := p.reply(req); d != nil || msg != "" {
			return d, msg
		}
	}
	switch req["cmd"] {
	case "status":
		return map[string]any{"board": "stm32h563", "caps": p.caps}, ""
	case "sensor_define", "sensor_stimulus":
		key, _ := req["instance"].(string)
		chunk, _ := base64.StdEncoding.DecodeString(req["data_b64"].(string))
		if req["offset"].(float64) == 0 {
			p.uploads[key] = nil
		}
		p.uploads[key] = append(p.uploads[key], chunk...)
		if len(p.uploads[key]) < int(req["total"].(float64)) {
			return map[string]any{"received": len(p.uploads[key])}, ""
		}
		var doc map[string]any
		_ = json.Unmarshal(p.uploads[key], &doc)
		return map[string]any{"name": doc["name"], "kind": doc["kind"], "program_bytes": 640, "params": doc["params"]}, ""
	case "sensor_start":
		return map[string]any{"instance": req["instance"], "slot": "i2c0", "image": "emulation", "image_loaded": true,
			"unavailable": []string{"dac_control_loop", "deep_replay"}}, ""
	case "sensor_status":
		// Two pages: the pod ends a page with "next" when the next instance would not fit.
		if req["first"] == nil {
			return map[string]any{"image": "emulation", "stimulus": map[string]any{"loaded": true, "running": true, "t_s": 1.5},
				"instances": []any{map[string]any{"instance": "env", "kind": "i2c", "name": "bme280", "slot": "i2c0", "running": true,
					"address": "0x76", "pins": map[string]any{"sda": 2, "scl": 1}, "values": map[string]any{"temperature_c": 25},
					"faults": []any{map[string]any{"type": "no_response", "left_s": 1.25}},
					"bus":    map[string]any{"writes": 14, "transfers": nil}}}, "next": 1}, ""
		}
		return map[string]any{"image": "emulation", "instances": []any{map[string]any{"instance": "ntc", "kind": "analog",
			"name": "ntc10k", "slot": "analog0", "running": true, "pins": map[string]any{"output": "dac"}, "output": 1.65,
			"faults": []any{map[string]any{"type": "open", "left_s": nil}}}}}, ""
	case "sensor_stop":
		if req["all"] == true {
			return map[string]any{"stopped": []string{"env", "ntc"}}, ""
		}
		return map[string]any{"stopped": []any{req["instance"]}}, ""
	case "fpga_image":
		img := map[string]float64{"loop": 0, "deep": 1, "emulation": 2}
		n := 2.0
		if s, ok := req["image"].(string); ok {
			n = img[s]
		}
		return map[string]any{"image": n, "name": []string{"loop", "deep", "emulation"}[int(n)], "version": 54, "features": 4}, ""
	}
	return map[string]any{}, ""
}

func (p *fakeSensorPod) last(cmd string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.requests) - 1; i >= 0; i-- {
		if p.requests[i]["cmd"] == cmd {
			return p.requests[i]
		}
	}
	return nil
}

func (p *fakeSensorPod) count(cmd string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.requests {
		if r["cmd"] == cmd {
			n++
		}
	}
	return n
}

func TestSensorStartFromTheLibrary(t *testing.T) {
	pod, g := newFakeSensorPod(t, "sensors")
	var out bytes.Buffer
	err := runSensorStart(g, "bme280", sensorStartFlags{instance: "env", pins: []string{"sda=2", "scl=la1"},
		address: "0x77", values: []string{"temperature_c=30"}}, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if pod.count("sensor_define") < 2 {
		t.Fatalf("expected a chunked define, got %d", pod.count("sensor_define"))
	}
	want, _ := sensors.Bundled().Load("bme280")
	wantJSON, _ := want.JSON()
	if !bytes.Equal(pod.uploads["env"], wantJSON) {
		t.Fatalf("uploaded %s", pod.uploads["env"])
	}
	start, _ := json.Marshal(pod.last("sensor_start"))
	if string(start) != `{"address":"0x77","cmd":"sensor_start","instance":"env","pins":{"scl":1,"sda":2},"values":{"temperature_c":30}}` {
		t.Fatalf("start %s", start)
	}
	s := out.String()
	for _, part := range []string{"Started env (i2c sensor bme280)", "scl LA1, sda LA2", "slot i2c0",
		"emulation FPGA image", "temperature_c", "-100..150 C (default 25)"} {
		if !strings.Contains(s, part) {
			t.Errorf("output lacks %q:\n%s", part, s)
		}
	}
}

func TestSensorStartPinsFromTheWiringProfile(t *testing.T) {
	pod, g := newFakeSensorPod(t, "sensors")
	sda, scl := 5, 6
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring { return &podWiring{I2cSda: &sda, I2cScl: &scl} }
	if err := runSensorStart(g, "bmp280", sensorStartFlags{}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if pins, _ := json.Marshal(pod.last("sensor_start")["pins"]); string(pins) != `{"scl":6,"sda":5}` {
		t.Fatalf("pins %s", pins)
	}
	// Wiring names resolve here: the pod takes LA channels only.
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring {
		w := &podWiring{I2cSda: &sda, I2cScl: &scl}
		w.Signals = append(w.Signals, struct {
			Name string `json:"name"`
			LA   int    `json:"la"`
		}{"DRDY", 7})
		return w
	}
	if err := runSensorStart(g, "bmp280", sensorStartFlags{pins: []string{"drdy=drdy", "scl=i2c_sda"}}, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if pins, _ := json.Marshal(pod.last("sensor_start")["pins"]); string(pins) != `{"drdy":7,"scl":5,"sda":5}` {
		t.Fatalf("pins %s", pins)
	}
	if err := runSensorStart(g, "bmp280", sensorStartFlags{pins: []string{"drdy=READY"}}, nil, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "not an LA channel") {
		t.Fatalf("err = %v", err)
	}
	err := runSensorStart(g, "gps_nmea", sensorStartFlags{}, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "needs pins tx") {
		t.Fatalf("err = %v", err)
	}
}

func TestSensorCommandsOnOldFirmware(t *testing.T) {
	pod, g := newFakeSensorPod(t, "dac")
	err := runSensorStart(g, "bmp280", sensorStartFlags{pins: []string{"sda=2", "scl=1"}}, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "this pod's firmware is too old for sensor emulation; update the firmware") {
		t.Fatalf("err = %v", err)
	}
	if pod.count("sensor_define") != 0 {
		t.Fatal("a define reached the old pod")
	}
}

func TestSensorValidateOfflineAndOnThePod(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "probe.yaml")
	_ = os.WriteFile(file, []byte("format: 1\nkind: i2c\nname: probe\nparams:\n  - { key: level, min: 0, max: 9, default: 1 }\n"+
		"i2c: { address: 0x40 }\nregisters:\n  - { at: 0, u8: level }\n"), 0o644)
	// No connection: the header only.
	off := &globalFlags{configFile: filepath.Join(dir, "none.json")}
	t.Setenv("BENCHPOD_CONNECTION", "")
	var out bytes.Buffer
	if err := runSensorValidate(off, file, "", false, nil, &out); err != nil || !strings.Contains(out.String(), "header ok") {
		t.Fatalf("%v %s", err, out.String())
	}
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("format: 1\nkind: can\nname: x\n"), 0o644)
	if err := runSensorValidate(off, bad, "", false, nil, io.Discard); err == nil || !strings.Contains(err.Error(), `kind: "can"`) {
		t.Fatalf("err = %v", err)
	}
	// With a pod: a dry-run define, and the pod's compile error comes back.
	pod, g := newFakeSensorPod(t, "sensors")
	out.Reset()
	if err := runSensorValidate(g, file, "", false, nil, &out); err != nil || !strings.Contains(out.String(), "compiles on") {
		t.Fatalf("%v %s", err, out.String())
	}
	if pod.last("sensor_define")["dry_run"] != true {
		t.Fatal("no dry_run")
	}
	pod.reply = func(req map[string]any) (any, string) {
		if req["cmd"] == "sensor_define" {
			return nil, "invalid definition: registers[0].u8: unknown name 'levl' at 0"
		}
		return nil, ""
	}
	err := runSensorValidate(g, file, "", false, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refused") || !strings.Contains(err.Error(), "unknown name 'levl'") {
		t.Fatalf("err = %v", err)
	}
	// stdin
	out.Reset()
	if err := runSensorValidate(off, "-", "", true, strings.NewReader(`{"format":1,"kind":"uart","name":"u"}`), &out); err != nil ||
		!strings.Contains(out.String(), "uart sensor u") {
		t.Fatalf("%v %s", err, out.String())
	}
}

func runCmd(t *testing.T, g *globalFlags, args ...string) (string, error) {
	t.Helper()
	cmd := newSensorCmd(g)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestSensorSetFaultStimulusStopStatus(t *testing.T) {
	pod, g := newFakeSensorPod(t, "sensors")
	if _, err := runCmd(t, g, "set", "env", "temperature_c=31.5", "humidity_pct=60"); err != nil {
		t.Fatal(err)
	}
	if v, _ := json.Marshal(pod.last("sensor_set")); string(v) != `{"cmd":"sensor_set","instance":"env","values":{"humidity_pct":60,"temperature_c":31.5}}` {
		t.Fatal(string(v))
	}
	if _, err := runCmd(t, g, "fault", "env", "corrupt", "--register", "0xF9", "--mask", "0x01", "--duration", "2s"); err != nil {
		t.Fatal(err)
	}
	if v, _ := json.Marshal(pod.last("sensor_fault")); string(v) != `{"at":249,"cmd":"sensor_fault","duration_s":2,"instance":"env","mask":1,"type":"corrupt"}` {
		t.Fatal(string(v))
	}
	if _, err := runCmd(t, g, "fault", "env", "melt"); err == nil {
		t.Fatal("unknown type accepted")
	}
	if _, err := runCmd(t, g, "fault", "env", "clear"); err != nil || pod.last("sensor_fault_clear")["instance"] != "env" {
		t.Fatal(err)
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "pts.csv"), []byte("0,1\n2,3\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "s.yaml"), []byte("format: 1\nkind: stimulus\nname: warm\nsignals:\n"+
		"  temperature_c:\n    - { shape: data, file: pts.csv }\n"), 0o644)
	if _, err := runCmd(t, g, "stimulus", "load", filepath.Join(dir, "s.yaml")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pod.uploads[""]), `"points":[[0,1],[2,3]]`) {
		t.Fatalf("stimulus %s", pod.uploads[""])
	}
	if _, err := runCmd(t, g, "stimulus", "start", "--at", "1.5s"); err != nil || pod.last("sensor_stimulus_start")["at_s"] != 1.5 {
		t.Fatal(err, pod.last("sensor_stimulus_start"))
	}
	if _, err := runCmd(t, g, "stimulus", "stop"); err != nil || pod.last("sensor_stimulus_stop") == nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, g, "stop"); err != nil || pod.last("sensor_stop")["all"] != true || !strings.Contains(out, "Stopped env, ntc") {
		t.Fatal(err, out)
	}
	if out, err := runCmd(t, g, "stop", "env"); err != nil || pod.last("sensor_stop")["instance"] != "env" || !strings.Contains(out, "Stopped env") {
		t.Fatal(err, out)
	}
	if _, err := runCmd(t, g, "set", strings.Repeat("a", 17), "x=1"); err == nil || !strings.Contains(err.Error(), "1 to 16 characters") {
		t.Fatal(err)
	}
	g.outputFilename = filepath.Join(dir, "status.txt")
	if _, err := runCmd(t, g, "status"); err != nil {
		t.Fatal(err)
	}
	st, _ := os.ReadFile(g.outputFilename)
	for _, part := range []string{"FPGA image emulation", "Stimulus: running, t = 1.5 s",
		"env: i2c bme280, running, scl LA1, sda LA2, slot i2c0, address 0x76",
		"values: temperature_c=25", "faults: no_response 1.25 s left", `bus: {"writes":14}`,
		"ntc: analog ntc10k, running, output dac, slot analog0", "output: 1.65", "faults: open until cleared"} {
		if !strings.Contains(string(st), part) {
			t.Errorf("status lacks %q:\n%s", part, st)
		}
	}
	if pod.count("sensor_status") != 2 || pod.last("sensor_status")["first"] != 1.0 {
		t.Fatalf("status pages: %d, last %v", pod.count("sensor_status"), pod.last("sensor_status"))
	}
	g.outputFilename = ""
	if out, err := runCmd(t, g, "image"); err != nil || !strings.Contains(out, "runs the emulation FPGA image (image 2, gateware v54)") {
		t.Fatal(err, out)
	}
	if out, err := runCmd(t, g, "image", "loop"); err != nil || pod.last("fpga_image")["image"] != "loop" || !strings.Contains(out, "switched to the loop") {
		t.Fatal(err, out)
	}
	if _, err := runCmd(t, g, "image", "turbo"); err == nil {
		t.Fatal("unknown image accepted")
	}
}

func TestSensorLibraryExportAndSave(t *testing.T) {
	t.Setenv(sensors.LibraryEnv, "")
	t.Setenv(sensors.CacheEnv, t.TempDir())
	dir := t.TempDir()
	g := &globalFlags{configFile: filepath.Join(dir, "config.json")}
	g.outputFilename = filepath.Join(dir, "list.txt")
	if _, err := runCmd(t, g, "library"); err != nil {
		t.Fatal(err)
	}
	list, _ := os.ReadFile(g.outputFilename)
	if !strings.Contains(string(list), "Sensor library 0.1.0 (bundled)") || !regexp.MustCompile(`gps_nmea +uart`).Match(list) ||
		!regexp.MustCompile(`temperature_ramp_fault +stimulus`).Match(list) {
		t.Fatal(string(list))
	}
	g.outputFilename = ""
	out, err := runCmd(t, g, "export", "sht4x")
	if err != nil || !strings.HasPrefix(out, "format: 1\nkind: i2c\nname: sht4x") {
		t.Fatalf("%v %q", err, out)
	}
	file := filepath.Join(dir, "my.yaml")
	if _, err := runCmd(t, g, "export", "bmp280", "-o", file); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); !strings.Contains(string(b), "name: bmp280") {
		t.Fatal(string(b))
	}
	if out, _ := runCmd(t, g, "export", "x", "--format-doc"); !strings.HasPrefix(out, "# BenchPod sensor definition format") {
		t.Fatal(out[:40])
	}
	if _, err := runCmd(t, g, "export", "nope"); err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatal(err)
	}
	// --save stores the version for the CLI and the SDK; default clears it.
	if _, err := runCmd(t, g, "library", "--version", "0.1.0", "--save"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := benchpodconfig.Load(g.configFile)
	if cfg.SensorLibrary != "0.1.0" {
		t.Fatalf("saved %q", cfg.SensorLibrary)
	}
	if _, err := runCmd(t, g, "library", "--version", "default", "--save"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = benchpodconfig.Load(g.configFile); cfg.SensorLibrary != "" {
		t.Fatalf("saved %q", cfg.SensorLibrary)
	}
	if _, err := runCmd(t, g, "library", "--version", "../x"); err == nil {
		t.Fatal("bad version accepted")
	}
}
