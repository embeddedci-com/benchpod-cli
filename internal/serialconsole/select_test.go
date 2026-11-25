package serialconsole

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.bug.st/serial/enumerator"
)

// withPods makes candidate ports out of ports (name -> pod short id; "" = not a pod) and opens
// each as a fake console answering status and identity.
func withPods(t *testing.T, ports map[string]string, vid string) {
	t.Helper()
	var details []*enumerator.PortDetails
	for name := range ports {
		details = append(details, &enumerator.PortDetails{Name: name, IsUSB: true, VID: vid, PID: "EC10"})
	}
	withLister(t, func() ([]*enumerator.PortDetails, error) { return details, nil })
	withGlobber(t)
	saved := openPort
	openPort = func(name string) (*Console, error) {
		id, ok := ports[name]
		if !ok {
			return nil, errors.New("no such port")
		}
		fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
			if id == "-" { // not a pod: silent
				return
			}
			out.WriteString(line + "\r\n")
			switch strings.Fields(line)[0] {
			case "status":
				out.WriteString("  device : benchpod\r\n")
			case "identity":
				fmt.Fprintf(out, "identity %s KEY\r\n", id)
			}
			out.WriteString("> ")
		}}
		return newConsole(fc), nil
	}
	t.Cleanup(func() { openPort = saved })
}

func TestNormalizePodName(t *testing.T) {
	for _, in := range []string{"baea06", "benchpod-baea06", "BenchPod baea06", "BENCHPOD-BAEA06", "benchpod-baea06.local"} {
		if got := NormalizePodName(in); got != "baea06" {
			t.Errorf("NormalizePodName(%q) = %q", in, got)
		}
	}
}

func TestOpenBenchpodSelectOnePod(t *testing.T) {
	// One pod seen twice (macOS cu/tty twins) is still one pod.
	withPods(t, map[string]string{"/dev/cu.usbmodemA": "aaaaaa", "/dev/cu.usbmodemA2": "aaaaaa"}, "1209")
	_, port, err := OpenBenchpodSelect("", "", "", 300*time.Millisecond)
	if err != nil || !strings.HasPrefix(port, "/dev/cu.usbmodemA") {
		t.Fatalf("port %q err %v", port, err)
	}
}

func TestOpenBenchpodSelectTwoPods(t *testing.T) {
	withPods(t, map[string]string{"/dev/cu.usbmodemA": "aaaaaa", "/dev/cu.usbmodemB": "bbbbbb"}, "1209")
	_, _, err := OpenBenchpodSelect("", "", "", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "usb:<name>") || !strings.Contains(err.Error(), "benchpod-bbbbbb") {
		t.Fatalf("two pods without a name: err = %v", err)
	}
	_, port, err := OpenBenchpodSelect("", "", "benchpod-bbbbbb", 300*time.Millisecond)
	if err != nil || port != "/dev/cu.usbmodemB" {
		t.Fatalf("by name: port %q err %v", port, err)
	}
	_, _, err = OpenBenchpodSelect("", "", "cccccc", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no pod named cccccc") {
		t.Fatalf("unknown name: err = %v", err)
	}
}

// podFake is a console answering like a pod, for the openPort seam.
func podFake(out *bytes.Buffer, line string) {
	out.WriteString(line + "\r\n")
	switch strings.Fields(line)[0] {
	case "status":
		out.WriteString("  device : benchpod\r\n")
	case "identity":
		out.WriteString("identity aaaaaa KEY\r\n")
	}
	out.WriteString("> ")
}

// withRecordingOpen stubs openPort so that every opened name is recorded; every port answers as a pod.
func withRecordingOpen(t *testing.T) *[]string {
	t.Helper()
	var opened []string
	saved := openPort
	openPort = func(name string) (*Console, error) {
		opened = append(opened, name)
		return newConsole(&fakeConsole{onWrite: func(line string, out *bytes.Buffer) { podFake(out, line) }}), nil
	}
	t.Cleanup(func() { openPort = saved })
	return &opened
}

// Detection only opens ports that identify as a pod: instruments sharing the ST VID are never written to.
func TestDetectionOpensOnlyPodPorts(t *testing.T) {
	list := []*enumerator.PortDetails{
		{Name: "/dev/cu.usbmodemTINY", IsUSB: true, VID: "0483", PID: "5740", Manufacturer: "tinysa.org", Product: "tinySA4"},
		{Name: "/dev/cu.usbmodemLINK", IsUSB: true, VID: "0483", PID: "374B", Product: "STM32 STLink"},
		{Name: "/dev/cu.usbmodemNUC", IsUSB: true, VID: "0483", PID: "5740", Product: "STM32 Virtual ComPort"},
		{Name: "/dev/cu.usbmodemRP", IsUSB: true, VID: "2E8A", PID: "000A"},
		{Name: "/dev/cu.usbmodemPOD", IsUSB: true, VID: "1209", PID: "EC10", Manufacturer: "EmbeddedCI", Product: "BenchPod"},
	}
	globbed := []string{"/dev/cu.usbmodemPOD", "/dev/tty.usbmodemPOD", "/dev/tty.usbmodemTINY", "/dev/cu.usbserial-NOINFO", "/dev/tty.usbmodemNUC"}
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"OpenBenchpodSelect", func() error {
			_, port, err := OpenBenchpodSelect("", "", "", 300*time.Millisecond)
			if err == nil && port != "/dev/cu.usbmodemPOD" {
				t.Errorf("port %q", port)
			}
			return err
		}},
		{"OpenBenchpod", func() error {
			_, port, err := OpenBenchpod("", "/dev/tty.usbmodemTINY", 300*time.Millisecond) // even a cached hint is filtered
			if err == nil && port != "/dev/cu.usbmodemPOD" {
				t.Errorf("port %q", port)
			}
			return err
		}},
		{"ProbeSerial", func() error {
			pods, _, err := ProbeSerial("", 300*time.Millisecond)
			if err == nil && len(pods) != 1 {
				t.Errorf("pods %v", pods)
			}
			return err
		}},
	} {
		withLister(t, func() ([]*enumerator.PortDetails, error) { return list, nil })
		withGlobber(t, globbed...)
		opened := withRecordingOpen(t)
		if err := c.run(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, n := range *opened {
			if n != "/dev/cu.usbmodemPOD" && n != "/dev/tty.usbmodemPOD" {
				t.Errorf("%s opened %s", c.name, n)
			}
		}
		if len(*opened) == 0 {
			t.Errorf("%s opened nothing", c.name)
		}
	}
}

func TestDetectionFindsOldFirmwarePods(t *testing.T) {
	for _, product := range []string{"BenchPod", "bench-pod STM32H563 CDC"} {
		withLister(t, func() ([]*enumerator.PortDetails, error) {
			return []*enumerator.PortDetails{
				{Name: "/dev/cu.usbmodemTINY", IsUSB: true, VID: "0483", PID: "5740", Product: "tinySA4"},
				{Name: "/dev/cu.usbmodemOLD", IsUSB: true, VID: "0483", PID: "5740", Manufacturer: "EmbeddedCI", Product: product},
			}, nil
		})
		withGlobber(t)
		opened := withRecordingOpen(t)
		_, port, err := OpenBenchpodSelect("", "", "", 300*time.Millisecond)
		if err != nil || port != "/dev/cu.usbmodemOLD" || len(*opened) != 1 {
			t.Fatalf("%q: port %q opened %v err %v", product, port, *opened, err)
		}
	}
}

// Nothing allowed: nothing is opened, and the error states the rule.
func TestDetectionNoPodOpensNothing(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		return []*enumerator.PortDetails{{Name: "/dev/cu.usbmodemTINY", IsUSB: true, VID: "0483", PID: "5740", Product: "tinySA4"}}, nil
	})
	withGlobber(t, "/dev/cu.usbmodemTINY", "/dev/cu.usbmodemNOINFO") // globbed-only: no USB info
	opened := withRecordingOpen(t)
	_, _, err := OpenBenchpodSelect("", "", "", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "1209:EC10") || !strings.Contains(err.Error(), "--connection") {
		t.Fatalf("err = %v", err)
	}
	if len(*opened) != 0 {
		t.Fatalf("opened %v", *opened)
	}
}

// An explicit device is used as given, enumeration and filter bypassed.
func TestExplicitDeviceNotFiltered(t *testing.T) {
	withLister(t, func() ([]*enumerator.PortDetails, error) {
		t.Error("enumerated for an explicit device")
		return nil, nil
	})
	if got, err := DetectPort("/dev/cu.usbmodemTINY"); err != nil || got != "/dev/cu.usbmodemTINY" {
		t.Fatalf("DetectPort: %q %v", got, err)
	}
	_, _, err := OpenBenchpodSelect("/dev/does-not-exist", "", "", 100*time.Millisecond)
	if err == nil || strings.Contains(err.Error(), "1209:EC10") {
		t.Fatalf("explicit open should fail on the device itself: %v", err)
	}
}
