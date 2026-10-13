package serialconsole

import (
	"errors"
	"testing"
)

func TestParseUSBNet(t *testing.T) {
	on := "  device : benchpod\r\n  ip     : 192.168.1.220\r\n" +
		"  usb    : net on pod 10.237.8.85 host 10.237.8.86 link up rx 2946 (drop 0) tx 5759 (full 2357)\r\n"
	u, err := ParseUSBNet(on)
	if err != nil || u != (USBNet{On: true, Link: true, PodIP: "10.237.8.85", HostIP: "10.237.8.86", LANIP: "192.168.1.220"}) {
		t.Fatalf("on: %+v %v", u, err)
	}
	off := "  usb    : net off (Wi-Fi configured) pod 10.237.8.85 host 10.237.8.86 link down rx 0 (drop 0) tx 0 (full 0)\n"
	u, err = ParseUSBNet(off)
	if err != nil || u.On || u.Link || u.PodIP != "10.237.8.85" {
		t.Fatalf("off: %+v %v", u, err)
	}
	// Pre-release builds print no net field: the adapter is there and on.
	if u, err = ParseUSBNet("  usb    : pod 10.237.8.85 host 10.237.8.86 link up rx 1 (drop 0) tx 1 (full 0)"); err != nil || !u.Link || !u.On {
		t.Fatalf("no net field: %+v %v", u, err)
	}
	if _, err := ParseUSBNet("  device : benchpod\n  ip     : 0.0.0.0\n"); !errors.Is(err, ErrNoUSBNet) {
		t.Fatalf("old firmware: err = %v, want ErrNoUSBNet", err)
	}
	if _, err := ParseUSBNet("  usb    : net on pod ? host ?"); err == nil || errors.Is(err, ErrNoUSBNet) {
		t.Fatalf("garbled line: err = %v", err)
	}
}
