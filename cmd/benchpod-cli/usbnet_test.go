package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

func withUSBNet(t *testing.T, u serialconsole.USBNet, uerr, dialErr error) *[]string {
	t.Helper()
	var dialed []string
	oldNet, oldDial, oldNote := usbNetwork, dialUSBPod, usbNote
	usbNetwork = func(*globalFlags, string) (serialconsole.USBNet, error) { return u, uerr }
	dialUSBPod = func(addr string) error { dialed = append(dialed, addr); return dialErr }
	usbNote = io.Discard
	t.Cleanup(func() { usbNetwork, dialUSBPod, usbNote = oldNet, oldDial, oldNote })
	return &dialed
}

// Firmware 3.9.0 with Wi-Fi configured: no USB network, so the pod's network address.
func TestNetworkTargetFallsBackToThePodsNetworkAddress(t *testing.T) {
	var note bytes.Buffer
	withUSBNet(t, serialconsole.USBNet{PodIP: "10.237.8.85", HostIP: "10.237.8.86", LANIP: "192.168.1.221"}, nil, nil)
	usbNote = &note
	spec, err := (&globalFlags{connection: "usb"}).networkTarget("la voltage")
	if err != nil || spec.Addr != "192.168.1.221:8080" {
		t.Fatalf("spec %+v err %v", spec, err)
	}
	if !strings.Contains(note.String(), "192.168.1.221:8080") || !strings.Contains(note.String(), "Wi-Fi is configured") {
		t.Fatalf("note %q", note.String())
	}
	// The network address does not answer either (no route, or the external LAN policy is off).
	withUSBNet(t, serialconsole.USBNet{PodIP: "10.237.8.85", LANIP: "192.168.1.221"}, nil, errors.New("refused"))
	_, err = (&globalFlags{connection: "usb"}).networkTarget("la voltage")
	if err == nil || !strings.Contains(err.Error(), "192.168.1.221:8080") || !strings.Contains(err.Error(), "clear-wifi") ||
		!strings.Contains(err.Error(), "policy") {
		t.Fatalf("err = %v", err)
	}
}

func TestNetworkTargetUsesTheUSBNetworkAdapter(t *testing.T) {
	dialed := withUSBNet(t, serialconsole.USBNet{On: true, Link: true, PodIP: "10.237.8.85", HostIP: "10.237.8.86"}, nil, nil)
	for _, conn := range []string{"usb", "/dev/cu.usbmodem1"} {
		g := &globalFlags{connection: conn}
		spec, err := g.networkTarget("capture")
		if err != nil {
			t.Fatalf("%s: %v", conn, err)
		}
		if !spec.IsNetwork() || spec.Addr != "10.237.8.85:8080" {
			t.Fatalf("%s: spec = %+v, want the network transport on 10.237.8.85:8080", conn, spec)
		}
	}
	if len(*dialed) != 2 {
		t.Fatalf("dialed %v, want the pod checked once per resolve", *dialed)
	}
}

func TestNetworkTargetLeavesOtherConnectionsAlone(t *testing.T) {
	withUSBNet(t, serialconsole.USBNet{}, errors.New("must not be asked"), nil)
	for conn, network := range map[string]bool{"192.168.1.5": true, "embeddedci:bench-1": false} {
		g := &globalFlags{connection: conn}
		spec, err := g.networkTarget("capture")
		if err != nil || spec.IsNetwork() != network {
			t.Fatalf("%s: spec %+v err %v", conn, spec, err)
		}
	}
}

func TestNetworkTargetSaysWhyUSBNetworkIsUnavailable(t *testing.T) {
	cases := []struct {
		name    string
		u       serialconsole.USBNet
		uerr    error
		dialErr error
		want    string
	}{
		{"old firmware", serialconsole.USBNet{}, serialconsole.ErrNoUSBNet, nil, "3.9.0"},
		{"wifi configured", serialconsole.USBNet{PodIP: "10.237.8.85", HostIP: "10.237.8.86"}, nil, nil, "clear-wifi"},
		{"link down", serialconsole.USBNet{On: true, PodIP: "10.237.8.85", HostIP: "10.237.8.86"}, nil, nil, "usb-reattach"},
		{"no answer", serialconsole.USBNet{On: true, Link: true, PodIP: "10.237.8.85", HostIP: "10.237.8.86"}, nil, errors.New("timeout"), "10.237.8.86"},
		{"no console", serialconsole.USBNet{}, errors.New("no pod found"), nil, "no pod found"},
	}
	for _, c := range cases {
		withUSBNet(t, c.u, c.uerr, c.dialErr)
		g := &globalFlags{connection: "usb"}
		_, err := g.networkTarget("capture")
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "benchpod discover") {
			t.Errorf("%s: err = %v, want it to mention %q and the discover way out", c.name, err, c.want)
		}
	}
}

func TestPreferredPodIPPicksTheUSBLink(t *testing.T) {
	lan, usb := net.ParseIP("192.168.1.221"), net.ParseIP("10.237.8.85")
	if got := preferredPodIP([]net.IP{lan, usb}); !got.Equal(usb) {
		t.Fatalf("got %v, want the USB link address", got)
	}
	if got := preferredPodIP([]net.IP{lan}); !got.Equal(lan) {
		t.Fatalf("got %v, want the LAN address when there is no USB link", got)
	}
	if preferredPodIP(nil) != nil {
		t.Fatal("no addresses: want nil")
	}
}

func TestDescribeUSBWarnsAboutDefaultPower(t *testing.T) {
	ma := 500
	got := describeUSB(podUSB{Net: "on", Link: true, IP: "10.237.8.85", HostIP: "10.237.8.86", Source: "default", SourceMA: &ma})
	if !strings.Contains(got, "10.237.8.85") || !strings.Contains(got, "500 mA") || !strings.Contains(got, "1.5 A") {
		t.Fatalf("got %q", got)
	}
	if got := describeUSB(podUSB{Net: "on", Source: "3.0A"}); strings.Contains(got, "500 mA") || !strings.Contains(got, "power 3.0A") {
		t.Fatalf("3.0A: %q", got)
	}
	if got := describeUSB(podUSB{Net: "off"}); !strings.Contains(got, "Wi-Fi") {
		t.Fatalf("off: %q", got)
	}
}

func TestDescribePowerDraw(t *testing.T) {
	total := 373300
	d := powerDraw{Internal: powerRail{OK: true, BusMV: 5008, CurrentUA: 142300}, External: powerRail{OK: true}}
	d.Pod = &struct {
		powerRail
		TotalUA *int `json:"total_ua"`
	}{powerRail{OK: true, BusMV: 5012, CurrentUA: 231000}, &total}
	tp := &targetPower{}
	tp.Efuse1.Enabled = 1
	got := describePowerDraw(d, tp)
	for _, want := range []string{"internal 5 V rail 142 mA", "pod 231 mA", "total from USB 373 mA"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "external") {
		t.Errorf("an off rail is listed: %q", got)
	}
	if got := describePowerDraw(powerDraw{}, nil); got != "" {
		t.Errorf("nothing readable: %q", got)
	}
}
