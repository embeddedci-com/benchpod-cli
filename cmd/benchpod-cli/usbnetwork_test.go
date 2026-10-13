package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// Over Ethernet/Wi-Fi the setting is read; changing it is refused before anything is sent.
func TestUSBNetworkOnTheLANReadsOnly(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"setting":"on"}}`)
	g := &globalFlags{connection: addr}
	var out bytes.Buffer
	if err := runUSBNetwork(g, "", &out); err != nil || !strings.Contains(out.String(), ": on") {
		t.Fatalf("show: %v %q", err, out.String())
	}
	if req := <-got; req != `{"cmd":"usb_network"}` {
		t.Fatalf("request %s", req)
	}
	err := runUSBNetwork(g, "off", &out)
	if err == nil || !strings.Contains(err.Error(), "only the device") || len(got) != 0 {
		t.Fatalf("set over the LAN: %v (sent %d)", err, len(got))
	}
}

// Over USB, `off` goes over the pod's USB link itself (no console); the pod's reply is printed.
func TestUSBNetworkOffOverTheUSBLink(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"setting":"off"}}`)
	old := localUSBPod
	t.Cleanup(func() { localUSBPod = old })
	localUSBPod = func(string) string { return addr }
	var out bytes.Buffer
	if err := runUSBNetwork(&globalFlags{connection: "usb"}, "off", &out); err != nil {
		t.Fatal(err)
	}
	if req := <-got; !strings.Contains(req, `"set":"off"`) || !strings.Contains(req, `"usb_network"`) {
		t.Fatalf("request %s", req)
	}
	if !strings.Contains(out.String(), "now off") || !strings.Contains(out.String(), "usb-network on") {
		t.Fatalf("output %q", out.String())
	}
}

// Older firmware: a clear "update the firmware".
func TestUSBNetworkOnOlderFirmware(t *testing.T) {
	addr, _ := fakeLANPod(t, `{"status":"error","message":"unknown cmd"}`)
	err := runUSBNetwork(&globalFlags{connection: addr}, "", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "3.9.0 or later") {
		t.Fatalf("err = %v", err)
	}
}

// The console's status line says why the adapter is off.
func TestParseUSBNetOffBySetting(t *testing.T) {
	u, err := serialconsole.ParseUSBNet("  usb    : net off (setting) pod 10.237.8.85 host 10.237.8.86 link down rx 1 (drop 0) tx 1 (full 0)\r\n")
	if err != nil || u.On || !u.OffBySetting {
		t.Fatalf("%+v %v", u, err)
	}
	u, _ = serialconsole.ParseUSBNet("  usb    : net off (Wi-Fi configured) pod 10.237.8.85 host 10.237.8.86 link down\r\n")
	if u.On || u.OffBySetting {
		t.Fatalf("wifi: %+v", u)
	}
	if got := describeUSB(podUSB{Net: "off", OffReason: "setting"}); !strings.Contains(got, "usb-network on") {
		t.Fatalf("status row %q", got)
	}
}
