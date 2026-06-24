package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
)

// connKind is the transport implied by the --connection value.
type connKind int

const (
	connWifi connKind = iota
	connSerial
)

// ConnSpec is a fully resolved connection target. A single --connection value
// (or stored default) carries both where and how to connect; the transport is
// inferred from the value's shape — there is no separate address flag.
//
//	192.168.1.5[:port], host        -> wifi/TCP   (Addr set)
//	/dev/tty..., COM3, \\.\COM3      -> serial     (Device set)
//	serial / usb                    -> serial, auto-detect (Device == "")
type ConnSpec struct {
	Kind   connKind
	Addr   string // TCP host:port for the wifi transport
	Device string // serial device path; "" means auto-detect (USB VID 2E8A)
}

// comPattern matches a Windows serial device name like COM3 / COM12.
var comPattern = regexp.MustCompile(`^(?i:COM)\d+$`)

// isSerialDevice reports whether raw looks like a serial device path rather than
// a network address: a Unix device path (/dev/...), a Windows COM name, or the
// \\.\COMx form.
func isSerialDevice(raw string) bool {
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\\.\`) {
		return true
	}
	return comPattern.MatchString(raw)
}

// classifyConnection turns a raw --connection value (or stored default) into a
// ConnSpec, inferring the transport from its shape. It errors on an empty value
// or on a bare transport keyword that carries no address.
func classifyConnection(raw string) (ConnSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ConnSpec{}, fmt.Errorf("no connection set; pass --connection <addr|device|serial> or run `benchpod set-connection <addr|device|serial>`")
	}
	switch strings.ToLower(raw) {
	case "serial", "usb":
		return ConnSpec{Kind: connSerial}, nil
	case "wifi", "tcp":
		return ConnSpec{}, fmt.Errorf("--connection %s needs an address, e.g. --connection 192.168.1.5", strings.ToLower(raw))
	}
	if isSerialDevice(raw) {
		return ConnSpec{Kind: connSerial, Device: raw}, nil
	}
	return ConnSpec{Kind: connWifi, Addr: benchpodconfig.EnsurePort(raw)}, nil
}

func (c ConnSpec) IsWifi() bool   { return c.Kind == connWifi }
func (c ConnSpec) IsSerial() bool { return c.Kind == connSerial }

// describeConn is a short human phrase for a resolved connection, used in
// confirmation messages.
func describeConn(c ConnSpec) string {
	if c.IsWifi() {
		return "wifi/TCP " + c.Addr
	}
	if c.Device != "" {
		return "serial " + c.Device
	}
	return "serial (auto-detect)"
}

// RequireWifi guards the TCP/JSON commands that the firmware's serial console
// does not implement: it errors (telling the user to point --connection at the
// bench pod's address) unless the wifi transport is selected.
func (c ConnSpec) RequireWifi(cmd string) error {
	if c.IsWifi() {
		return nil
	}
	return fmt.Errorf("%s is not available over a serial connection; use --connection <bench-pod-address>", cmd)
}
