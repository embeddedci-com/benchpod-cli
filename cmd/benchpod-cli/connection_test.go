package main

import (
	"strings"
	"testing"
)

func TestClassifyConnection(t *testing.T) {
	tests := []struct {
		in       string
		wantWifi bool
		addr     string // expected ConnSpec.Addr (wifi)
		device   string // expected ConnSpec.Device (serial)
	}{
		{"192.168.1.5", true, "192.168.1.5:8080", ""},
		{"192.168.1.5:9000", true, "192.168.1.5:9000", ""},
		{"benchpod.local", true, "benchpod.local:8080", ""},
		{"  10.0.0.2  ", true, "10.0.0.2:8080", ""},
		{"serial", false, "", ""},
		{"USB", false, "", ""},
		{"/dev/tty.usbmodem1101", false, "", "/dev/tty.usbmodem1101"},
		{"COM3", false, "", "COM3"},
		{`\\.\COM12`, false, "", `\\.\COM12`},
	}
	for _, tt := range tests {
		spec, err := classifyConnection(tt.in)
		if err != nil {
			t.Errorf("classifyConnection(%q): unexpected error %v", tt.in, err)
			continue
		}
		if spec.IsWifi() != tt.wantWifi {
			t.Errorf("classifyConnection(%q).IsWifi() = %v, want %v", tt.in, spec.IsWifi(), tt.wantWifi)
		}
		if spec.Addr != tt.addr {
			t.Errorf("classifyConnection(%q).Addr = %q, want %q", tt.in, spec.Addr, tt.addr)
		}
		if spec.Device != tt.device {
			t.Errorf("classifyConnection(%q).Device = %q, want %q", tt.in, spec.Device, tt.device)
		}
	}
}

func TestClassifyConnectionErrors(t *testing.T) {
	// Empty value, and bare transport keywords with no address, are errors.
	for _, in := range []string{"", "   ", "wifi", "tcp", "WiFi"} {
		if _, err := classifyConnection(in); err == nil {
			t.Errorf("classifyConnection(%q) = nil error, want error", in)
		}
	}
	// The address-less keyword error points the user at the address form.
	if _, err := classifyConnection("wifi"); err == nil || !strings.Contains(err.Error(), "--connection 192") {
		t.Errorf("wifi keyword error should suggest an address: %v", err)
	}
}

func TestRequireWifi(t *testing.T) {
	wifi, _ := classifyConnection("192.168.1.5")
	if err := wifi.RequireWifi("ping"); err != nil {
		t.Errorf("RequireWifi on wifi transport: unexpected error %v", err)
	}
	// "serial" is still accepted as an input spelling, so both forms must land
	// on the same USB transport and the same guidance.
	for _, conn := range []string{"usb", "serial", "/dev/tty.usbmodem1101"} {
		spec, _ := classifyConnection(conn)
		err := spec.RequireWifi("ping")
		if err == nil {
			t.Fatalf("RequireWifi(%q) = nil, want error", conn)
		}
		for _, want := range []string{"ping", "USB", "benchpod discover"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("RequireWifi(%q) error = %q, want it to mention %q", conn, err, want)
			}
		}
	}
}

func TestIsSerialDevice(t *testing.T) {
	for _, dev := range []string{"/dev/ttyACM0", "/dev/cu.usbmodem1", "COM3", "com12", `\\.\COM7`} {
		if !isSerialDevice(dev) {
			t.Errorf("isSerialDevice(%q) = false, want true", dev)
		}
	}
	for _, notDev := range []string{"192.168.1.5", "192.168.1.5:8080", "benchpod.local", "serial"} {
		if isSerialDevice(notDev) {
			t.Errorf("isSerialDevice(%q) = true, want false", notDev)
		}
	}
}

func TestDescribeConnSpeaksUSBNotSerial(t *testing.T) {
	// The keyword the user types and the wording the CLI echoes back must agree:
	// "serial" is accepted as a legacy input spelling but is never presented.
	for _, conn := range []string{"usb", "serial"} {
		spec, err := classifyConnection(conn)
		if err != nil {
			t.Fatalf("classifyConnection(%q): %v", conn, err)
		}
		if got := describeConn(spec); got != "usb (auto-detect)" {
			t.Errorf("describeConn(%q) = %q, want %q", conn, got, "usb (auto-detect)")
		}
	}
	spec, _ := classifyConnection("/dev/ttyACM0")
	if got := describeConn(spec); got != "usb /dev/ttyACM0" {
		t.Errorf("describeConn(device) = %q", got)
	}
	spec, _ = classifyConnection("192.168.1.5")
	if got := describeConn(spec); got != "network/TCP 192.168.1.5:8080" {
		t.Errorf("describeConn(addr) = %q", got)
	}
}
