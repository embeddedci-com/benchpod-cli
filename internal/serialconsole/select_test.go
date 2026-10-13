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
		details = append(details, &enumerator.PortDetails{Name: name, IsUSB: true, VID: vid})
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
