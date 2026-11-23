package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

func TestJTAGOnlyTarget(t *testing.T) {
	for target, want := range map[string]bool{
		"target/esp32s2.cfg": true, "esp32": true, "target/gd32vf103.cfg": true, "target/fe310.cfg": true,
		"target/k210.cfg": true, "target/riscv.cfg": true, "riscv-custom": true, "TARGET/RISCV.CFG": true,
		"target/stm32f4x.cfg": false, "target/esp32s3.cfg": false, "": false, "target/nrf52.cfg": false,
	} {
		if got := jtagOnlyTarget(target); got != want {
			t.Errorf("jtagOnlyTarget(%q) = %v, want %v", target, got, want)
		}
	}
}

// The selection rule: JTAG iff all four JTAG pins are known and (no SWD pins or a JTAG-only target).
func TestUseJTAG(t *testing.T) {
	for _, tt := range []struct{ jtag, swd, only, want bool }{
		{false, false, false, false}, {false, true, false, false}, {false, true, true, false},
		{true, false, false, true}, {true, false, true, true},
		{true, true, false, false}, {true, true, true, true},
	} {
		if got := useJTAG(tt.jtag, tt.swd, tt.only); got != tt.want {
			t.Errorf("useJTAG(jtag=%v, swd=%v, only=%v) = %v", tt.jtag, tt.swd, tt.only, got)
		}
	}
}

func TestResolveDAPPins(t *testing.T) {
	swd := func(f *flashFlags) { f.swclk, f.swdio = "11", "12" }
	jtag := func(f *flashFlags) { f.tck, f.tms, f.tdi, f.tdo = "1", "2", "3", "4" }
	target := func(s string) func(*flashFlags) { return func(f *flashFlags) { f.target = s } }
	build := func(opts ...func(*flashFlags)) *flashFlags {
		f := &flashFlags{}
		for _, o := range opts {
			o(f)
		}
		return f
	}

	p, err := build(swd, jtag, target("target/stm32f4x.cfg")).resolveDAPPins()
	if err != nil || p.jtag || p.swclk != 11 || p.swdio != 12 {
		t.Errorf("both pin sets, ARM target: %+v %v", p, err)
	}
	p, err = build(swd, jtag, target("target/riscv.cfg")).resolveDAPPins()
	if err != nil || !p.jtag || p.jtagPins.TCK != 1 || p.jtagPins.TDO != 4 || p.jtagPins.TRST != 0 || p.transport() != "jtag" {
		t.Errorf("both pin sets, JTAG-only target: %+v %v", p, err)
	}
	p, err = build(jtag, target("target/stm32f4x.cfg"), func(f *flashFlags) { f.trst = "la5" }).resolveDAPPins()
	if err != nil || !p.jtag || p.jtagPins.TRST != 5 {
		t.Errorf("jtag only: %+v %v", p, err)
	}
	p, err = build(swd).resolveDAPPins()
	if err != nil || p.jtag {
		t.Errorf("swd only: %+v %v", p, err)
	}
	// A partial JTAG set is ignored when SWD can be used, an error otherwise.
	if p, err = build(swd, func(f *flashFlags) { f.tck = "1" }).resolveDAPPins(); err != nil || p.jtag {
		t.Errorf("partial jtag with swd: %+v %v", p, err)
	}
	if _, err = build(func(f *flashFlags) { f.tck, f.tms = "1", "2" }).resolveDAPPins(); err == nil || !strings.Contains(err.Error(), "--tdi, --tdo") {
		t.Errorf("partial jtag: %v", err)
	}
	if _, err = build().resolveDAPPins(); err == nil || !strings.Contains(err.Error(), "--swclk and --swdio are required") {
		t.Errorf("no pins: %v", err)
	}
	// Pin checks.
	if _, err = build(jtag, func(f *flashFlags) { f.trst = "2" }).resolveDAPPins(); err == nil || !strings.Contains(err.Error(), "--tms and --trst") {
		t.Errorf("duplicate jtag pin: %v", err)
	}
	if _, err = build(jtag, func(f *flashFlags) { f.tdi = "99" }).resolveDAPPins(); err == nil || !strings.Contains(err.Error(), "--tdi") {
		t.Errorf("bad pin: %v", err)
	}
	if _, err = build(func(f *flashFlags) { f.swclk, f.swdio = "3", "3" }).resolveDAPPins(); err == nil {
		t.Error("swclk == swdio accepted")
	}
	// Allowed overlaps: TCK = SWCLK, TMS = SWDIO. Others are refused.
	ok := build(jtag, target("target/riscv.cfg"), func(f *flashFlags) { f.swclk, f.swdio = "1", "2" })
	if _, err = ok.resolveDAPPins(); err != nil {
		t.Errorf("tck=swclk, tms=swdio: %v", err)
	}
	bad := build(jtag, target("target/riscv.cfg"), func(f *flashFlags) { f.swclk, f.swdio = "3", "12" })
	if _, err = bad.resolveDAPPins(); err == nil || !strings.Contains(err.Error(), "--tdi") {
		t.Errorf("tdi=swclk accepted: %v", err)
	}
	swapped := build(jtag, target("target/riscv.cfg"), func(f *flashFlags) { f.swclk, f.swdio = "2", "1" })
	if _, err = swapped.resolveDAPPins(); err == nil {
		t.Error("tms=swclk accepted")
	}
}

func TestFlashTakesJTAGFromWiringFlagsWin(t *testing.T) {
	old := loadPodWiring
	t.Cleanup(func() { loadPodWiring = old })
	pin := func(n int) *int { return &n }
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring {
		return &podWiring{JtagTck: pin(1), JtagTms: pin(2), JtagTdi: pin(3), JtagTdo: pin(4), JtagTrst: pin(5)}
	}
	f := &flashFlags{tdo: "9"}
	f.fillFromWiring(&globalFlags{})
	if f.tck != "1" || f.tms != "2" || f.tdi != "3" || f.tdo != "9" || f.trst != "5" || f.swclk != "" {
		t.Errorf("%+v", f)
	}
}

func TestPodWiringJTAGFields(t *testing.T) {
	var w podWiring
	js := `{"version":2,"swd_swclk":11,"swd_swdio":12,"jtag_tck":11,"jtag_tms":12,"jtag_tdi":13,"jtag_tdo":14}`
	if err := json.Unmarshal([]byte(js), &w); err != nil {
		t.Fatal(err)
	}
	if w.JtagTck == nil || *w.JtagTck != 11 || *w.JtagTdo != 14 || w.JtagTrst != nil {
		t.Errorf("%+v", w)
	}
	for name, want := range map[string]int{"jtag_tck": 11, "jtag_tms": 12, "jtag_tdi": 13, "jtag_tdo": 14} {
		if got, ok := w.channel(name); !ok || got != want {
			t.Errorf("channel(%s) = %d, %v", name, got, ok)
		}
	}
	if _, ok := w.channel("jtag_trst"); ok {
		t.Error("jtag_trst is not wired")
	}
}

type statusCmd struct{ reply string }

func (s statusCmd) Command(context.Context, map[string]any) (json.RawMessage, error) {
	return json.RawMessage(s.reply), nil
}

func TestRequireJTAGFeature(t *testing.T) {
	has := `{"features_rev":16,"gateware":61,"fpga_image":"loop","analog":true}`
	if err := requireJTAGFeature(context.Background(), statusCmd{has}); err != nil {
		t.Errorf("pod with jtag: %v", err)
	}
	for name, reply := range map[string]string{
		"old gateware": `{"features_rev":16,"gateware":60,"analog":true}`,
		"old table":    `{"features_rev":15,"gateware":61,"analog":true}`,
		"no features":  `{"gateware":61}`,
	} {
		err := requireJTAGFeature(context.Background(), statusCmd{reply})
		if err == nil || !strings.Contains(err.Error(), "no JTAG") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCloudFlashRequestJTAGJSON(t *testing.T) {
	b, _ := json.Marshal(serverapi.CloudFlashRequest{Target: "t", NReset: true, JTAGPins: jtagRequestPins(dapPins{jtag: true})})
	if !strings.Contains(string(b), `"tck":0`) || strings.Contains(string(b), "swclk") {
		t.Errorf("jtag request: %s", b)
	}
	b, _ = json.Marshal(serverapi.CloudFlashRequest{Target: "t", SWCLK: 11, SWDIO: 12})
	if !strings.Contains(string(b), `"swclk":11`) || strings.Contains(string(b), "tck") {
		t.Errorf("swd request: %s", b)
	}
}

// withProfile makes loadPodWiring return w for the test.
func withProfile(t *testing.T, w *podWiring) {
	t.Helper()
	old := loadPodWiring
	t.Cleanup(func() { loadPodWiring = old })
	loadPodWiring = func(*globalFlags, io.Writer) *podWiring { return w }
}

func bothPinsProfile(port string) *podWiring {
	pin := func(n int) *int { return &n }
	w := &podWiring{SwdSwclk: pin(11), SwdSwdio: pin(12), JtagTck: pin(11), JtagTms: pin(12), JtagTdi: pin(13), JtagTdo: pin(14)}
	if port != "" {
		w.DebugPort = &port
	}
	return w
}

// resolve fills the flags from the profile like runFlash does, then decides the port.
func resolve(t *testing.T, f *flashFlags) (dapPins, error) {
	t.Helper()
	f.fillFromWiring(&globalFlags{})
	return f.resolveDAPPins()
}

// Step 1: pin flags win over the profile's debug_port.
func TestDebugPortFlagsWin(t *testing.T) {
	withProfile(t, bothPinsProfile("swd"))
	// JTAG flag, no SWD flag: JTAG although the profile says swd; the rest fills from the profile.
	p, err := resolve(t, &flashFlags{tck: "1", target: "target/stm32f4x.cfg"})
	if err != nil || !p.jtag || p.jtagPins.TCK != 1 || p.jtagPins.TMS != 12 || p.jtagPins.TDO != 14 || !strings.Contains(p.why, "flags") {
		t.Errorf("jtag flag: %+v %v", p, err)
	}
	withProfile(t, bothPinsProfile("jtag"))
	// SWD flags, no JTAG flag: SWD although the profile says jtag.
	p, err = resolve(t, &flashFlags{swclk: "3", swdio: "4", target: "target/stm32f4x.cfg"})
	if err != nil || p.jtag || p.swclk != 3 || p.swdio != 4 || !strings.Contains(p.why, "flags") {
		t.Errorf("swd flags: %+v %v", p, err)
	}
	// A JTAG flag with an incomplete JTAG set (even though SWD pins are known) is the usage error.
	pin := func(n int) *int { return &n }
	withProfile(t, &podWiring{SwdSwclk: pin(11), SwdSwdio: pin(12)})
	if _, err = resolve(t, &flashFlags{tck: "1"}); err == nil || !strings.Contains(err.Error(), "JTAG needs all four") {
		t.Errorf("incomplete jtag flags: %v", err)
	}
	// Both kinds of flags: the profile's debug_port decides.
	withProfile(t, bothPinsProfile("jtag"))
	p, err = resolve(t, &flashFlags{tck: "1", swclk: "11", swdio: "12", target: "target/stm32f4x.cfg"})
	if err != nil || !p.jtag || !strings.Contains(p.why, "debug_port") {
		t.Errorf("both flag kinds: %+v %v", p, err)
	}
}

// Step 2: the profile's debug_port.
func TestDebugPortFromProfile(t *testing.T) {
	arm := "target/stm32f4x.cfg"
	withProfile(t, bothPinsProfile("jtag"))
	p, err := resolve(t, &flashFlags{target: arm})
	if err != nil || !p.jtag || p.jtagPins.TDO != 14 || p.why != "the wiring profile's debug_port" {
		t.Errorf("debug_port jtag, ARM target: %+v %v", p, err)
	}
	withProfile(t, bothPinsProfile("swd"))
	p, err = resolve(t, &flashFlags{target: "target/riscv.cfg"})
	if err != nil || p.jtag || p.swclk != 11 {
		t.Errorf("debug_port swd, JTAG-only target: %+v %v", p, err)
	}
	// Case and spaces do not matter.
	withProfile(t, bothPinsProfile(" JTAG "))
	if p, err = resolve(t, &flashFlags{target: arm}); err != nil || !p.jtag {
		t.Errorf("JTAG upper case: %+v %v", p, err)
	}
	// Incomplete pins of the chosen port: an error naming the profile fields.
	pin := func(n int) *int { return &n }
	jt := "jtag"
	withProfile(t, &podWiring{DebugPort: &jt, SwdSwclk: pin(11), SwdSwdio: pin(12), JtagTck: pin(11), JtagTms: pin(12), JtagTdi: pin(13)})
	if _, err = resolve(t, &flashFlags{target: arm}); err == nil || err.Error() != "debug_port is jtag but jtag_tdo is not set" {
		t.Errorf("missing tdo: %v", err)
	}
	withProfile(t, &podWiring{DebugPort: &jt, SwdSwclk: pin(11), SwdSwdio: pin(12)})
	if _, err = resolve(t, &flashFlags{target: arm}); err == nil || !strings.Contains(err.Error(), "jtag_tck, jtag_tms, jtag_tdi, jtag_tdo are not set") {
		t.Errorf("no jtag pins: %v", err)
	}
	sw := "swd"
	withProfile(t, &podWiring{DebugPort: &sw, JtagTck: pin(1), JtagTms: pin(2), JtagTdi: pin(3), JtagTdo: pin(4), SwdSwclk: pin(5)})
	if _, err = resolve(t, &flashFlags{target: arm}); err == nil || err.Error() != "debug_port is swd but swd_swdio is not set" {
		t.Errorf("missing swdio: %v", err)
	}
	// An invalid value.
	bad := "usb"
	withProfile(t, bothPinsProfile(bad))
	if _, err = resolve(t, &flashFlags{target: arm}); err == nil || !strings.Contains(err.Error(), "debug_port") {
		t.Errorf("invalid debug_port: %v", err)
	}
}

// Step 3: no flags, no debug_port: the automatic rule, unchanged (SWD for an ARM target with both sets).
func TestDebugPortAutomatic(t *testing.T) {
	withProfile(t, bothPinsProfile(""))
	p, err := resolve(t, &flashFlags{target: "target/stm32f4x.cfg"})
	if err != nil || p.jtag || p.why != "automatic" {
		t.Errorf("automatic ARM: %+v %v", p, err)
	}
	p, err = resolve(t, &flashFlags{target: "target/riscv.cfg"})
	if err != nil || !p.jtag || !strings.HasPrefix(p.why, "automatic") {
		t.Errorf("automatic riscv: %+v %v", p, err)
	}
}

func TestPodWiringDebugPort(t *testing.T) {
	var w podWiring
	if err := json.Unmarshal([]byte(`{"version":2,"debug_port":"jtag"}`), &w); err != nil || w.DebugPort == nil || *w.DebugPort != "jtag" {
		t.Errorf("%+v %v", w, err)
	}
	w = podWiring{}
	if err := json.Unmarshal([]byte(`{"version":2,"debug_port":null}`), &w); err != nil || w.DebugPort != nil {
		t.Errorf("null: %+v %v", w, err)
	}
	if validDebugPort("swd") != nil || validDebugPort("jtag") != nil || validDebugPort("") == nil || validDebugPort("both") == nil {
		t.Error("validDebugPort")
	}
}
