package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// stubLAVoltageProfile replaces the embeddedci.com side of `la voltage` for one test: id is the
// device the connection resolves to on the signed-in account ("" = not on it), and the returned
// slice records each save.
func stubLAVoltageProfile(t *testing.T, id string, changed bool, err error) *[]int {
	t.Helper()
	savedAcct, savedSave := accountDeviceForConnection, saveLaMVToProfile
	t.Cleanup(func() { accountDeviceForConnection, saveLaMVToProfile = savedAcct, savedSave })
	var saves []int
	accountDeviceForConnection = func(*globalFlags) string { return id }
	saveLaMVToProfile = func(dev string, mv int) (bool, error) {
		if dev != id {
			t.Errorf("saved to %q, want %q", dev, id)
		}
		saves = append(saves, mv)
		return changed, err
	}
	return &saves
}

func TestLAVoltageSavesToTheProfile(t *testing.T) {
	addr, _ := fakeLANPod(t, `{"status":"ok","data":{"mv":3300,"st":1,"readback_mv":3300}}`)
	saves := stubLAVoltageProfile(t, "dev-1", true, nil)
	var out, warn bytes.Buffer
	if err := runLAVoltage(&globalFlags{connection: addr}, 3300, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if len(*saves) != 1 || (*saves)[0] != 3300 || !strings.Contains(out.String(), "Saved to the pod's wiring profile on embeddedci.com") {
		t.Fatalf("saves %v out %q", *saves, out.String())
	}
	// A failed save warns; the command still succeeds.
	addr, _ = fakeLANPod(t, `{"status":"ok","data":{"mv":3300,"st":1,"readback_mv":3300}}`)
	stubLAVoltageProfile(t, "dev-1", false, errors.New("403 forbidden"))
	out.Reset()
	if err := runLAVoltage(&globalFlags{connection: addr}, 3300, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "could not save it to the pod's wiring profile on embeddedci.com: 403 forbidden") {
		t.Fatalf("warn %q", warn.String())
	}
	// Showing the voltage never writes the profile.
	addr, _ = fakeLANPod(t, `{"status":"ok","data":{"mv":3300,"st":1,"readback_mv":3300}}`)
	saves = stubLAVoltageProfile(t, "dev-1", true, nil)
	if err := runLAVoltage(&globalFlags{connection: addr}, 0, &bytes.Buffer{}, &bytes.Buffer{}); err != nil || len(*saves) != 0 {
		t.Fatalf("err %v saves %v", err, *saves)
	}
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
	if want := "LA voltage of " + addr + " is now 3.3 V\n"; out.String() != want || warn.Len() != 0 {
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
	addr, _ := fakeLANPod(t, `{"status":"error","message":"unknown cmd"}`)
	if err := runLAVoltage(&globalFlags{connection: addr}, 0, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || err.Error() != laVoltageMissing {
		t.Fatalf("old firmware: %v", err)
	}
	msg := fwrefusals.Example("lan_locked")
	addr, _ = fakeLANPod(t, `{"status":"error","message":"`+msg+`"}`)
	err := runLAVoltage(&globalFlags{connection: addr}, 3300, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "la voltage: the pod refused: "+msg+" ("+lockedUSBHint+")" {
		t.Fatalf("locked: %v", err)
	}
	if code := exitCode(err); code != exitRefused {
		t.Fatalf("exit %d", code)
	}
}

type fakeLAVoltageConsole struct {
	mv  int
	err error
	set []int
}

func (f *fakeLAVoltageConsole) LAVoltage(_ context.Context, mv int) (int, error) {
	f.set = append(f.set, mv)
	if f.err != nil {
		return 0, f.err
	}
	if mv != 0 {
		f.mv = mv
	}
	return f.mv, nil
}
func (f *fakeLAVoltageConsole) Close() error { return nil }

func TestLAVoltageUSB(t *testing.T) {
	stubLAVoltageProfile(t, "", false, nil)
	fc := &fakeLAVoltageConsole{}
	saved := openLAVoltageConsole
	t.Cleanup(func() { openLAVoltageConsole = saved })
	openLAVoltageConsole = func(_ *globalFlags, device string, _ time.Duration) (laVoltageConsole, string, context.Context, context.CancelFunc, error) {
		return fc, "/dev/cu.usbmodem1", context.Background(), func() {}, nil
	}
	var out bytes.Buffer
	if err := runLAVoltage(&globalFlags{connection: "usb"}, 1800, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "LA voltage of the pod on /dev/cu.usbmodem1 is now 1.8 V\n" || len(fc.set) != 1 || fc.set[0] != 1800 {
		t.Fatalf("out = %q, set = %v", out.String(), fc.set)
	}

	fc.err = &serialconsole.CommandError{Cmd: "la-voltage", Reason: "1.8 V needs a v3 pod; this board is v2 (its TPS2116 has no 1.8 V setting)"}
	err := runLAVoltage(&globalFlags{connection: "usb"}, 1800, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || err.Error() != "la voltage: the pod refused: "+fc.err.(*serialconsole.CommandError).Reason {
		t.Fatalf("refusal: %v", err)
	}
	fc.err = serialconsole.ErrUnknownCommand
	if err := runLAVoltage(&globalFlags{connection: "usb"}, 0, &bytes.Buffer{}, &bytes.Buffer{}); err == nil || err.Error() != laVoltageMissing {
		t.Fatalf("old firmware: %v", err)
	}
}

func TestLAVoltageArgIsAUsageError(t *testing.T) {
	t.Setenv("BENCHPOD_CONNECTION", "")
	if code := run([]string{"la", "voltage", "5V", "--connection", "127.0.0.1:1"}); code != exitUsage {
		t.Fatalf("exit %d", code)
	}
}
