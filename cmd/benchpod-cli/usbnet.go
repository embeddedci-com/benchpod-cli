package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// The pod's USB network adapter (firmware 3.9.0+): the USB device that carries the console is
// also a network adapter, with the pod's full network API on its fixed address. A command that
// needs the network and is given a USB connection uses it; the console stays for the commands
// that only the console runs and as the fallback.

// usbNetProbeTimeout bounds the TCP check of the pod's address on the USB link.
const usbNetProbeTimeout = 2 * time.Second

// usbNetwork reads the USB network adapter's state from the pod's console. A variable so tests
// can replace it.
var usbNetwork = func(g *globalFlags, device string) (serialconsole.USBNet, error) {
	console, _, err := g.openBenchpodSerial(device, 2*time.Second)
	if err != nil {
		return serialconsole.USBNet{}, err
	}
	defer console.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return console.USBNetwork(ctx)
}

// dialUSBPod checks the pod answers on its USB address. A variable so tests can replace it.
var dialUSBPod = func(addr string) error {
	c, err := net.DialTimeout("tcp", addr, usbNetProbeTimeout)
	if err != nil {
		return err
	}
	return c.Close()
}

// pingLANPod checks the pod answers its API on a network address: a JSON ping, not just a TCP
// connect, because with the external LAN policy off the pod accepts the connection and resets it.
// A variable so tests can replace it.
var pingLANPod = func(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), usbNetProbeTimeout)
	defer cancel()
	_, err := (&tcpclient.Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "ping"})
	return err
}

// usbNetAddr returns the pod's API address on its USB network adapter (podIP:8080), or why there
// is none, phrased to follow "over USB, ".
func (g *globalFlags) usbNetAddr(device string) (string, error) {
	u, err := usbNetwork(g, device)
	if err != nil {
		if errors.Is(err, tcpclient.ErrUnreachable) {
			return "", err // no pod (or not that one) on USB: the console's own message says which
		}
		if errors.Is(err, serialconsole.ErrNoUSBNet) {
			return "", errors.New("the pod's firmware has no USB network adapter (it needs 3.9.0 or later: `benchpod flash-self`)")
		}
		return "", fmt.Errorf("the pod's console did not report its USB network: %w", err)
	}
	addr := net.JoinHostPort(u.PodIP, "8080")
	var why error
	switch {
	case !u.On:
		why = errors.New("USB networking is off because Wi-Fi is configured on the pod (`benchpod clear-wifi` turns it back on)")
	case !u.Link:
		why = fmt.Errorf("the pod's USB network adapter (%s) is not up on this computer; "+
			"macOS ignores a new adapter while the screen is locked: unlock and run `benchpod usb-reattach`", u.PodIP)
	default:
		if err := dialUSBPod(addr); err != nil {
			why = fmt.Errorf("the pod did not answer on its USB network address %s (%v); this computer should have %s on that adapter, "+
				"run `benchpod usb-reattach` if it does not", addr, err, u.HostIP)
		}
	}
	if why == nil {
		return addr, nil
	}
	// Firmware 3.9.0+ keeps only the console commands that need physical presence, so without
	// the USB network the way in is the pod's Ethernet or Wi-Fi address, when this computer can
	// reach it (the external LAN policy applies there).
	if u.LANIP == "" {
		return "", why
	}
	lan := net.JoinHostPort(u.LANIP, "8080")
	if err := pingLANPod(lan); err != nil {
		return "", fmt.Errorf("%v; its network address %s did not answer either (%v; the pod's external LAN policy "+
			"`off` refuses it there)", why, lan, err)
	}
	fmt.Fprintf(usbNote, "usb: using the pod's network address %s: %v. Its external LAN policy applies there.\n", lan, why)
	return lan, nil
}

// preferUSBNetwork turns a USB spec into the pod's address on its USB network adapter when that
// is up (on 3.9.0+ with USB networking off, its Ethernet or Wi-Fi address when that answers), for a
// command that works over both; any other spec, or a USB pod without a way in over the network
// (older firmware, nothing answers), is returned unchanged and the command uses the console.
func (g *globalFlags) preferUSBNetwork(spec ConnSpec, cmd string) ConnSpec {
	if !spec.IsSerial() {
		return spec
	}
	if net, err := g.networkTarget(cmd); err == nil && net.IsNetwork() {
		return net
	}
	return spec
}

// usbNote is where networkTarget says which way it took (stderr; tests capture it).
var usbNote io.Writer = os.Stderr

// usbLinkAddr is the pod's API address on its USB network link when that works (firmware 3.9.0+,
// no Wi-Fi configured, adapter up), else "": never the Ethernet or Wi-Fi fallback. For the settings
// only the USB link may change over the network (the company CA and the cloud proxy).
func (g *globalFlags) usbLinkAddr(device string) string {
	if g.usbAddr != "" {
		if host, _, err := net.SplitHostPort(g.usbAddr); err == nil && usbLinkNet.Contains(net.ParseIP(host)) {
			return g.usbAddr
		}
		return ""
	}
	u, err := usbNetwork(g, device)
	if err != nil || !u.On || !u.Link {
		return ""
	}
	addr := net.JoinHostPort(u.PodIP, "8080")
	if dialUSBPod(addr) != nil {
		return ""
	}
	return addr
}

// networkTarget is resolveTarget for a command that needs the pod's network API: a USB connection
// becomes the pod's address on its USB network adapter, or the standard "needs the network"
// error with the reason that is not possible.
func (g *globalFlags) networkTarget(cmd string) (ConnSpec, error) {
	spec, err := g.resolveTarget()
	if err != nil || !spec.IsSerial() {
		return spec, err
	}
	if g.usbAddr != "" {
		return ConnSpec{Kind: connNetwork, Addr: g.usbAddr}, nil
	}
	addr, uerr := g.usbAddr, g.usbErr
	if uerr == nil {
		addr, uerr = g.usbNetAddr(spec.Device)
		g.usbErr = uerr
	}
	if errors.Is(uerr, tcpclient.ErrUnreachable) {
		return spec, uerr
	}
	if uerr != nil {
		return spec, fmt.Errorf("%w. Here: %v", spec.RequireNetwork(cmd), uerr)
	}
	g.usbAddr = addr
	return ConnSpec{Kind: connNetwork, Addr: addr}, nil
}
