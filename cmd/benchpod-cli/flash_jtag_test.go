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
