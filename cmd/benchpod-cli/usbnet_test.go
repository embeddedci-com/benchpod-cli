package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

func withUSBNet(t *testing.T, u serialconsole.USBNet, uerr, dialErr error) *[]string {
	t.Helper()
	var dialed []string
	oldNet, oldDial := usbNetwork, dialUSBPod
	usbNetwork = func(*globalFlags, string) (serialconsole.USBNet, error) { return u, uerr }
	dialUSBPod = func(addr string) error { dialed = append(dialed, addr); return dialErr }
	t.Cleanup(func() { usbNetwork, dialUSBPod = oldNet, oldDial })
	return &dialed
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
