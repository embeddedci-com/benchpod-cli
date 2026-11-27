package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// stubLAVoltageProfile records any save to embeddedci.com (la voltage makes none: the pod keeps
// the I/O voltage in its own wiring profile).
func stubLAVoltageProfile(t *testing.T, _ string, _ bool, _ error) *[]int {
	t.Helper()
	saved := saveLaMVToProfile
	t.Cleanup(func() { saveLaMVToProfile = saved })
	var saves []int
	saveLaMVToProfile = func(_ string, mv int) (bool, error) { saves = append(saves, mv); return true, nil }
	return &saves
}

func TestLAVoltageLAN(t *testing.T) {
	stubLAVoltageProfile(t, "", false, nil)
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"mv":3300,"st":1,"readback_mv":3300}}`)
	g := &globalFlags{connection: addr}
	var out, warn bytes.Buffer
	if err := runLAVoltage(g, 3300, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if req := <-got; req != `{"cmd":"la_voltage","mv":3300}` {
		t.Fatalf("request = %s", req)
	}
	if want := "LA voltage of " + addr + " is now 3.3 V\n"; !strings.HasPrefix(out.String(), want) || warn.Len() != 0 {
		t.Fatalf("out = %q, warn = %q", out.String(), warn.String())
	}

	addr, got = fakeLANPod(t, `{"status":"ok","data":{"mv":0,"st":0,"readback_mv":0}}`)
	out.Reset()
	if err := runLAVoltage(&globalFlags{connection: addr}, 0, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if req := <-got; req != `{"cmd":"la_voltage"}` {
		t.Fatalf("request = %s", req)
	}
	if !strings.Contains(out.String(), ": not set") {
		t.Fatalf("out = %q", out.String())
	}

	// A mux that disagrees is worth a warning.
	addr, _ = fakeLANPod(t, `{"status":"ok","data":{"mv":1800,"st":1,"readback_mv":3300}}`)
	out.Reset()
	if err := runLAVoltage(&globalFlags{connection: addr}, 0, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), ": 1.8 V\n") || !strings.Contains(warn.String(), "reports 3.3 V, not 1.8 V") {
		t.Fatalf("out = %q, warn = %q", out.String(), warn.String())
	}
}

func TestLAVoltageLANRefusals(t *testing.T) {
	for _, reply := range unknownCmdReplies {
		addr, _ := fakeLANPod(t, reply)
		if err := runLAVoltage(&globalFlags{connection: addr}, 0, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || err.Error() != laVoltageMissing {
			t.Fatalf("old firmware: %v", err)
		}
	}
	addr, _ := fakeLANPod(t, `{"status":"error","code":"BP0040","args":{"cmd":"la_voltage"}}`)
	err := runLAVoltage(&globalFlags{connection: addr}, 3300, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.HasPrefix(err.Error(), "la voltage: the pod refused: BP0040: The pod's LAN is locked: la_voltage needs the cloud or USB; run this over USB") {
		t.Fatalf("locked: %v", err)
	}
	if code := exitCode(err); code != exitRefused {
		t.Fatalf("exit %d", code)
	}
}

// Over USB la voltage goes over the pod's USB network adapter; without it, it says why.
func TestLAVoltageUSB(t *testing.T) {
	stubLAVoltageProfile(t, "", false, nil)
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"mv":1800,"st":1,"readback_mv":1800}}`)
	var out bytes.Buffer
	if err := runLAVoltage(&globalFlags{connection: "usb", usbAddr: addr}, 1800, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "LA voltage of "+addr+" is now 1.8 V\n") {
		t.Fatalf("out = %q", out.String())
	}
	if req := <-got; !strings.Contains(req, `"cmd":"la_voltage"`) || !strings.Contains(req, `"mv":1800`) {
		t.Fatalf("request %s", req)
	}
	old := usbNetwork
	usbNetwork = func(*globalFlags, string) (serialconsole.USBNet, error) {
		return serialconsole.USBNet{PodIP: "10.237.8.85", HostIP: "10.237.8.86", OffBySetting: true}, nil
	}
	t.Cleanup(func() { usbNetwork = old })
	err := runLAVoltage(&globalFlags{connection: "usb"}, 0, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "usb-network on") {
		t.Fatalf("no USB network: %v", err)
	}
}

func TestLAVoltageArgIsAUsageError(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	if code := run([]string{"la", "voltage", "2.5V", "--connection", "127.0.0.1:1"}); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
}

// A pod that keeps its wiring profiles itself (3.10.8+) applies the profile's I/O voltage at DUT
// power-on: setting the bank saves nothing to embeddedci.com.
func TestLAVoltageSavesNothingWhenThePodStoresProfiles(t *testing.T) {
	addr, _ := fakeLANPod(t, `{"status":"ok","data":{"mv":3300,"st":1,"readback_mv":3300}}`)
	saves := stubLAVoltageProfile(t, "dev-1", true, nil)
	var out, warn bytes.Buffer
	if err := runLAVoltage(&globalFlags{connection: addr}, 3300, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if len(*saves) != 0 || !strings.Contains(out.String(), "Not saved: the pod uses the I/O voltage in its wiring profile") {
		t.Fatalf("saves %v out %q", *saves, out.String())
	}
}

// Digital board v4: 5 V mode shows VREF OUT, does not warn that the bank (3.3 V) differs, and
// warns when VREF OUT sits above its setting. Older boards send no vref_out and print as before.
func TestLAVoltageVrefOut(t *testing.T) {
	stubLAVoltageProfile(t, "", false, nil)
	addr, _ := fakeLANPod(t, `{"status":"ok","data":{"mv":5000,"st":1,"readback_mv":3300,`+
		`"vref_out":{"source":"5v","set_mv":5000,"mv":4920,"state":"ok","up":true}}}`)
	var out, warn strings.Builder
	if err := runLAVoltage(&globalFlags{connection: addr}, 5000, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "is now 5 V") || !strings.Contains(out.String(), "I/O reference (VREF OUT): 4.92 V, set to 5 V (ok)") {
		t.Errorf("out = %q", out.String())
	}
	if warn.String() != "" {
		t.Errorf("warn = %q", warn.String())
	}

	addr, _ = fakeLANPod(t, `{"status":"ok","data":{"mv":3300,"st":1,"readback_mv":3300,`+
		`"vref_out":{"source":"bank","set_mv":3300,"mv":4700,"state":"high","up":true}}}`)
	out.Reset()
	warn.Reset()
	if err := runLAVoltage(&globalFlags{connection: addr}, 0, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "above its setting") || !strings.Contains(warn.String(), "la voltage 5.0") {
		t.Errorf("warn = %q", warn.String())
	}

	addr, _ = fakeLANPod(t, `{"status":"ok","data":{"mv":3300,"st":1,"readback_mv":3300}}`)
	out.Reset()
	warn.Reset()
	if err := runLAVoltage(&globalFlags{connection: addr}, 0, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "VREF OUT") || warn.String() != "" {
		t.Errorf("older board: out %q warn %q", out.String(), warn.String())
	}
}
