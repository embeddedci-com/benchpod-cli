package ci

import (
	"strings"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const fullYAML = `version: 1
suite: process-node-hil
dut: process-node
wiring: process-node
require:
  pod_firmware: ">=3.10.8"
firmware:
  build: make -C firmware
  file: firmware/build/process-node.elf
  flash: {target: target/stm32f4x.cfg, verify: true}
env:
  BENCH_LA6_RESISTOR: "1"
  WHO: ${USER_NAME:-nobody}
baseline: main
timeout: 20m
keep: {traces: all}
cleanup: [dac_stop, power_off]
upload: false
steps:
  - id: boot
    power_cycle: {until: "APP_OK", timeout: 10s}
    required: true
    expect:
      boot_ms: {max: 600, drift: 20%}
  - name: Idle current
    measure_power: {duration: 2s}
    expect:
      mean_ma: {min: 5, max: 20}
      peak_ma: {}
  - id: hil
    pytest: {path: tests, args: [-m, hardware], python: "uv run python"}
    expect:
      "tests/test_heater.py::test_heater/settle_ms": {max: 900}
  - id: cloud-api
    run: go test ./hwe2e -run TestCloud -json -tags ${TAGS}
    results: gotest-json
  - wait: 2s
  - uart_expect: {until: "ready>", send: "status\n"}
`

func TestParseFull(t *testing.T) {
	cfg, err := Parse([]byte(fullYAML), "/x/benchpod-ci.yaml", envOf(map[string]string{"TAGS": "hw"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Suite != "process-node-hil" || cfg.DUT != "process-node" || cfg.Wiring != "process-node" {
		t.Fatalf("key: %+v", cfg)
	}
	if cfg.Dir != "/x" || cfg.SHA256 == "" {
		t.Errorf("dir %q sha %q", cfg.Dir, cfg.SHA256)
	}
	if cfg.Timeout != 20*time.Minute || cfg.Keep != "all" || cfg.Upload != "false" {
		t.Errorf("timeout %v keep %q upload %q", cfg.Timeout, cfg.Keep, cfg.Upload)
	}
	if strings.Join(cfg.Cleanup, ",") != "dac_stop,power_off" {
		t.Errorf("cleanup %v", cfg.Cleanup)
	}
	if !cfg.Require.Range.Allows("3.10.11") || cfg.Require.Range.Allows("3.10.7") {
		t.Errorf("require range wrong")
	}
	if cfg.Env["WHO"] != "nobody" || cfg.Env["BENCH_LA6_RESISTOR"] != "1" {
		t.Errorf("env %v", cfg.Env)
	}
	if cfg.Firmware.File != "firmware/build/process-node.elf" || cfg.Firmware.Flash.Target != "target/stm32f4x.cfg" ||
		cfg.Firmware.Flash.Verify == nil || !*cfg.Firmware.Flash.Verify {
		t.Errorf("firmware %+v", cfg.Firmware)
	}
	ids := []string{}
	for _, s := range cfg.Steps {
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, ","); got != "flash,boot,idle-current,hil,cloud-api,wait,uart-expect" {
		t.Fatalf("ids %s", got)
	}
	if !cfg.Steps[0].Implicit || cfg.Steps[0].Kind != KindFlash || !cfg.Steps[0].Required {
		t.Errorf("implicit flash %+v", cfg.Steps[0])
	}
	boot := cfg.Steps[1]
	if boot.PowerCycle.Until != "APP_OK" || boot.PowerCycle.Timeout != 10*time.Second || boot.PowerCycle.OffMS != 500 || !boot.Required {
		t.Errorf("boot %+v %+v", boot, boot.PowerCycle)
	}
	if len(boot.Expect) != 1 || *boot.Expect[0].Max != 600 || *boot.Expect[0].Drift != 0.2 {
		t.Errorf("boot expect %+v", boot.Expect)
	}
	if e := cfg.Steps[2].Expect; len(e) != 2 || e[1].Name != "peak_ma" || e[1].Min != nil || e[1].Max != nil {
		t.Errorf("idle expect %+v", e)
	}
	py := cfg.Steps[3].Pytest
	if py.Path != "tests" || strings.Join(py.Args, " ") != "-m hardware" || py.Python != "uv run python" {
		t.Errorf("pytest %+v", py)
	}
	if cfg.Steps[4].Run != "go test ./hwe2e -run TestCloud -json -tags hw" || cfg.Steps[4].Results != "gotest-json" {
		t.Errorf("run %+v", cfg.Steps[4])
	}
	if cfg.Steps[5].Wait != 2*time.Second {
		t.Errorf("wait %v", cfg.Steps[5].Wait)
	}
	if u := cfg.Steps[6].UARTExpect; u.Send != "status\n" || u.Timeout != 5*time.Second {
		t.Errorf("uart %+v", u)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"missing suite", "version: 1\ndut: x\nsteps: [{wait: 1s}]\n", "suite is required"},
		{"missing dut", "version: 1\nsuite: s\nsteps: [{wait: 1s}]\n", "dut is required"},
		{"bad version", "version: 2\nsuite: s\ndut: d\nsteps: [{wait: 1s}]\n", "version 2 is not supported"},
		{"unknown top key", "version: 1\nsuite: s\ndut: d\nstepz: []\n", `:4: unknown key "stepz"`},
		{"two kinds", "version: 1\nsuite: s\ndut: d\nsteps:\n  - wait: 1s\n    power_on: {}\n", "a step does exactly one thing"},
		{"no kind", "version: 1\nsuite: s\ndut: d\nsteps:\n  - id: x\n", "step x needs one of"},
		{"dup id", "version: 1\nsuite: s\ndut: d\nsteps:\n  - {id: a, wait: 1s}\n  - {id: a, power_on: {}}\n", `step id "a" is used twice`},
		{"bad duration", "version: 1\nsuite: s\ndut: d\nsteps:\n  - wait: soon\n", `:5: step 1 wait: "soon" is not a duration`},
		{"bad regex", "version: 1\nsuite: s\ndut: d\nsteps:\n  - uart_expect: {until: \"(\"}\n", "error parsing regexp"},
		{"unknown metric", "version: 1\nsuite: s\ndut: d\nsteps:\n  - measure_power: {}\n    expect: {avg_ma: {}}\n", `unknown metric "avg_ma"`},
		{"boot_ms needs until", "version: 1\nsuite: s\ndut: d\nsteps:\n  - power_cycle: {}\n    expect: {boot_ms: {}}\n", "boot_ms needs power_cycle.until"},
		{"pytest expect form", "version: 1\nsuite: s\ndut: d\nsteps:\n  - pytest: tests\n    expect: {settle_ms: {}}\n", `must be "<case id>/<measurement>"`},
		{"capture stub", "version: 1\nsuite: s\ndut: d\nsteps:\n  - capture: {la: {}}\n", "capture steps are not supported yet"},
		{"flash without firmware", "version: 1\nsuite: s\ndut: d\nsteps:\n  - flash: {}\n", "a flash step needs a firmware block"},
		{"unset var", "version: 1\nsuite: s\ndut: d\nsteps:\n  - run: echo ${NOPE}\n", "${NOPE} is not set"},
		{"results on pytest", "version: 1\nsuite: s\ndut: d\nsteps:\n  - pytest: tests\n    results: gotest-json\n", "results only applies to run steps"},
		{"bad results", "version: 1\nsuite: s\ndut: d\nsteps:\n  - run: x\n    results: tap\n", "results must be junit:<path> or gotest-json"},
		{"bad drift", "version: 1\nsuite: s\ndut: d\nsteps:\n  - measure_power: {}\n    expect: {mean_ma: {drift: lots}}\n", "must be a percentage"},
		{"min above max", "version: 1\nsuite: s\ndut: d\nsteps:\n  - measure_power: {}\n    expect: {mean_ma: {min: 5, max: 1}}\n", "min 5 is above max 1"},
		{"bad require", "version: 1\nsuite: s\ndut: d\nrequire: {pod_firmware: \">=three\"}\nsteps: [{wait: 1s}]\n", "is not a version"},
		{"firmware both", "version: 1\nsuite: s\ndut: d\nfirmware: {file: a.elf, build_id: x}\nsteps: [{wait: 1s}]\n", "not both"},
		{"build needs file", "version: 1\nsuite: s\ndut: d\nfirmware: {build: make, build_id: x}\nsteps: [{wait: 1s}]\n", "set file too"},
		{"bad keep", "version: 1\nsuite: s\ndut: d\nkeep: {traces: some}\nsteps: [{wait: 1s}]\n", "keep.traces must be all, failed or none"},
		{"bad cleanup", "version: 1\nsuite: s\ndut: d\ncleanup: [reboot]\nsteps: [{wait: 1s}]\n", `unknown action "reboot"`},
		{"bad id", "version: 1\nsuite: s\ndut: d\nsteps:\n  - {id: \"a b\", wait: 1s}\n", "must be 1-64 characters"},
		{"empty", "", "the file is empty"},
		{"not yaml", "version: [1\n", "benchpod-ci.yaml"},
		{"flash with settings", "version: 1\nsuite: s\ndut: d\nfirmware: a.elf\nsteps:\n  - flash: {target: x}\n", "flash settings go in firmware.flash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml), "benchpod-ci.yaml", noEnv)
			if err == nil {
				t.Fatalf("no error, want %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestParseCollectsAllErrors(t *testing.T) {
	_, err := Parse([]byte("version: 1\nsteps:\n  - wait: x\n  - power_on: {}\n    expect: {a: {}}\n"), "f.yaml", noEnv)
	es, ok := err.(Errors)
	if !ok {
		t.Fatalf("want Errors, got %T %v", err, err)
	}
	if len(es) < 4 {
		t.Fatalf("want at least 4 errors (suite, dut, wait, metric), got %d: %v", len(es), err)
	}
	if es[0].Line == 0 {
		t.Errorf("first error has no line: %v", es[0])
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nsuite: s\ndut: none\nsteps:\n  - power_on: {}\n  - power_on:\n  - power_cycle:\n  - pytest:\n"), "f.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Keep != "failed" || cfg.Upload != "auto" || cfg.Timeout != DefaultRunTimeout || len(cfg.Cleanup) != 4 {
		t.Errorf("defaults %+v", cfg)
	}
	var ids []string
	for _, s := range cfg.Steps {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "power-on,power-on-2,power-cycle,pytest" {
		t.Errorf("ids %v", ids)
	}
	if cfg.Steps[2].PowerCycle == nil || cfg.Steps[2].PowerCycle.Timeout != 10*time.Second {
		t.Errorf("power_cycle defaults %+v", cfg.Steps[2].PowerCycle)
	}
	if cfg.Steps[3].Pytest == nil {
		t.Errorf("pytest defaults")
	}
}

func TestImplicitFlashIDClash(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nsuite: s\ndut: d\nfirmware: {build_id: abc}\nsteps:\n  - {id: flash, wait: 1s}\n"), "f.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Steps[0].ID != "flash-firmware" || !cfg.Steps[0].Implicit || !cfg.Firmware.Uploaded() {
		t.Errorf("steps %+v", cfg.Steps[0])
	}
}

func TestExpand(t *testing.T) {
	env := envOf(map[string]string{"A": "1", "EMPTY": ""})
	cases := map[string]string{
		"x${A}y":          "x1y",
		"${B:-def}":       "def",
		"${EMPTY:-def}":   "def",
		"${EMPTY}":        "",
		"$A ${A}":         "$A 1",
		"$${A}":           "${A}",
		"no vars":         "no vars",
		"${A}${A:-z}${A}": "111",
	}
	for in, want := range cases {
		got, err := Expand(in, env)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"${B}", "${A", "${1x}"} {
		if _, err := Expand(bad, env); err == nil {
			t.Errorf("Expand(%q): no error", bad)
		}
	}
}

func TestEnvSeesYAMLEnv(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\nsuite: s-${X}\ndut: d\nenv: {X: from-yaml}\nsteps:\n  - run: echo ${X}\n"), "f.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Suite != "s-from-yaml" || cfg.Steps[0].Run != "echo from-yaml" {
		t.Errorf("%q %q", cfg.Suite, cfg.Steps[0].Run)
	}
}

func TestDurationsAndDrift(t *testing.T) {
	for in, want := range map[string]time.Duration{"10s": 10 * time.Second, "2m": 2 * time.Minute, "500ms": 500 * time.Millisecond, "1.5": 1500 * time.Millisecond, "1h30m": 90 * time.Minute} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-1s", "ten"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q): no error", bad)
		}
	}
	for in, want := range map[string]float64{"20%": 0.2, "0.15": 0.15, " 5 % ": 0.05} {
		if got, err := ParseDrift(in); err != nil || got != want {
			t.Errorf("ParseDrift(%q) = %v, %v", in, got, err)
		}
	}
}

func TestVersionRange(t *testing.T) {
	r, err := ParseVersionRange(">=3.10.8, <4")
	if err != nil {
		t.Fatal(err)
	}
	for v, want := range map[string]bool{"3.10.8": true, "3.10.12": true, "v3.11.0": true, "4.0.0": false, "3.9.9": false, "3.10.8-dev": true, "junk": false} {
		if got := r.Allows(v); got != want {
			t.Errorf("Allows(%q) = %v", v, got)
		}
	}
	eq, _ := ParseVersionRange("3.10.1")
	if !eq.Allows("3.10.1") || eq.Allows("3.10.2") {
		t.Error("== range")
	}
	var none VersionRange
	if !none.Allows("anything") {
		t.Error("empty range allows all")
	}
}

func TestSelect(t *testing.T) {
	cfg, err := Parse([]byte(fullYAML), "f.yaml", envOf(map[string]string{"TAGS": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Select([]string{"hil,boot"})
	if err != nil || len(got) != 2 || got[0].ID != "boot" || got[1].ID != "hil" {
		t.Fatalf("select %v %v", got, err)
	}
	if _, err := cfg.Select([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "no such step") {
		t.Errorf("unknown step: %v", err)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Idle current": "idle-current", "  Boot / UART  ": "boot-uart", "a_b.c": "a_b.c", "***": ""} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseUARTPortUSB(t *testing.T) {
	cfg, err := Parse([]byte(`version: 1
suite: s
dut: d
steps:
  - uart_expect: {port: usb, dtr: false, rts: true, baud: 460800, until: "ok"}
  - power_cycle: {port: usb, until: "boot"}
  - uart_expect: {port: pins, until: "ok"}
  - uart_expect: {until: "ok"}
`), "f.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Steps[0].UARTExpect.UART
	if !u.IsUSB() || u.DTR == nil || *u.DTR || u.RTS == nil || !*u.RTS || u.Baud != 460800 {
		t.Errorf("uart_expect %+v", u)
	}
	if pc := cfg.Steps[1].PowerCycle.UART; !pc.IsUSB() || pc.DTR != nil || pc.RTS != nil || pc.Baud != 0 {
		t.Errorf("power_cycle %+v", pc)
	}
	for i := 2; i < 4; i++ {
		if cfg.Steps[i].UARTExpect.UART.IsUSB() {
			t.Errorf("step %d is the pins", i)
		}
	}
}

func TestParseUARTPortErrors(t *testing.T) {
	for name, c := range map[string]struct{ step, want string }{
		"bad port":          {"uart_expect: {port: i2c, until: x}", "port must be usb"},
		"dtr without usb":   {"uart_expect: {dtr: true, until: x}", "dtr only applies with port: usb"},
		"rts with pins":     {"uart_expect: {port: pins, rts: false, until: x}", "rts only applies with port: usb"},
		"baud without usb":  {"uart_expect: {baud: 9600, until: x}", "baud only applies with port: usb"},
		"baud range":        {"uart_expect: {port: usb, baud: 100, until: x}", "baud must be 300 to 3000000"},
		"dtr not a bool":    {"uart_expect: {port: usb, dtr: maybe, until: x}", "must be true or false"},
		"power cycle no re": {"power_cycle: {port: usb}", "need until"},
	} {
		_, err := Parse([]byte("version: 1\nsuite: s\ndut: d\nsteps:\n  - "+c.step+"\n"), "f.yaml", noEnv)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
