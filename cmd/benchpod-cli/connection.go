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
	connCloud
)

// ConnSpec is a fully resolved connection target. A single --connection value
// (or stored default) carries both where and how to connect; the transport is
// inferred from the value's shape — there is no separate address flag.
//
//	192.168.1.5[:port], host        -> wifi/TCP   (Addr set)
//	/dev/tty..., COM3, \\.\COM3      -> serial     (Device set)
//	serial / usb                    -> serial, auto-detect (Device == "")
//	embeddedci:<name>               -> cloud (Name set), only where a command can work over it
type ConnSpec struct {
	Kind   connKind
	Addr   string // TCP host:port for the wifi transport
	Device string // serial device path; "" means auto-detect (USB VID 2E8A)
	Name   string // the pod's device name on embeddedci.com, for the cloud form
}

// cloudConnPrefix is the SDK's and MCP's connection form for a pod on embeddedci.com.
const cloudConnPrefix = "embeddedci:"

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
// ConnSpec for a command that talks to the pod itself, over the network or USB.
// It errors on an empty value, on a bare transport keyword that carries no
// address, and on the cloud form, which those commands cannot use.
func classifyConnection(raw string) (ConnSpec, error) {
	spec, err := parseTarget(raw)
	if err == nil && spec.IsCloud() {
		// Parsed as host:port it would be dialed (host "embeddedci", port "<name>")
		// and retried until the timeout, so say what it is instead.
		return ConnSpec{}, fmt.Errorf("--connection %s names a pod on embeddedci.com; the benchpod CLI talks to a pod over the network or USB. "+
			"Pass the pod's address (`benchpod discover` finds it) or `usb`. Drive a cloud pod from the web app, the Python SDK or the MCP server (they take %s<name>)",
			strings.TrimSpace(raw), cloudConnPrefix)
	}
	return spec, err
}

// parseTarget is the one parser of a target (--connection value or stored default),
// inferring the transport from its shape. Unlike classifyConnection it accepts the
// cloud form, embeddedci:<name>; only the commands that can work over embeddedci.com
// use it directly (see globalFlags.resolveTarget).
func parseTarget(raw string) (ConnSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ConnSpec{}, fmt.Errorf("no connection set; pass --connection <addr|device|usb> or run `benchpod set-connection <addr|device|usb>`")
	}
	switch strings.ToLower(raw) {
	// "usb" is the name this keyword goes by everywhere the CLI speaks to a
	// user — it names what the person plugged in. "serial" is the older
	// spelling, still accepted so saved configs and BENCHPOD_CONNECTION values
	// written before the rename keep working; it is not advertised anywhere.
	case "usb", "serial":
		return ConnSpec{Kind: connSerial}, nil
	case "wifi", "tcp":
		return ConnSpec{}, fmt.Errorf("--connection %s needs an address, e.g. --connection 192.168.1.5", strings.ToLower(raw))
	}
	// The SDK's and MCP's cloud form.
	if strings.HasPrefix(strings.ToLower(raw), cloudConnPrefix) {
		name := strings.TrimSpace(raw[len(cloudConnPrefix):])
		if name == "" {
			return ConnSpec{}, fmt.Errorf("--connection %s needs the pod's device name, e.g. %sbenchpod-a1b2c3", raw, cloudConnPrefix)
		}
		return ConnSpec{Kind: connCloud, Name: name}, nil
	}
	if isSerialDevice(raw) {
		return ConnSpec{Kind: connSerial, Device: raw}, nil
	}
	return ConnSpec{Kind: connWifi, Addr: benchpodconfig.EnsurePort(raw)}, nil
}

func (c ConnSpec) IsWifi() bool   { return c.Kind == connWifi }
func (c ConnSpec) IsSerial() bool { return c.Kind == connSerial }
func (c ConnSpec) IsCloud() bool  { return c.Kind == connCloud }

// describeConn is a short human phrase for a resolved connection, used in
// confirmation messages.
func describeConn(c ConnSpec) string {
	if c.IsWifi() {
		return "network/TCP " + c.Addr
	}
	if c.IsCloud() {
		return "embeddedci.com device " + c.Name
	}
	if c.Device != "" {
		return "usb " + c.Device
	}
	return "usb (auto-detect)"
}

// RequireWifi guards the TCP/JSON commands that the firmware's serial console
// does not implement: it errors unless the network transport is selected.
//
// The message names the way out rather than only the restriction, because a
// caller hitting this is usually part-way through first-time setup and does not
// yet know the pod's address — `discover` is what finds it.
func (c ConnSpec) RequireWifi(cmd string) error {
	if c.IsWifi() {
		return nil
	}
	return fmt.Errorf("%s needs the pod on the network; it is not available over a USB connection. "+
		"Run `benchpod discover` to find the pod's address, then pass --connection <address>. "+
		"If it has no address yet, plug in Ethernet or run `benchpod set-wifi --ssid <ssid>`", cmd)
}
