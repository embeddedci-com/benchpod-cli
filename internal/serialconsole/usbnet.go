package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// USBNet is the pod's USB network adapter as its console `status` reports it (firmware 3.9.0+):
//
//	usb    : net on pod 10.237.8.85 host 10.237.8.86 link up rx 2946 (drop 0) tx 5759 (full 2357)
//
// The link is a point-to-point /30: the pod's API is on PodIP:8080, and the pod's DHCP server
// gives the computer HostIP. On is false while Wi-Fi credentials are stored on the pod (USB
// networking and Wi-Fi are one or the other); Link is true once the computer has the adapter up.
type USBNet struct {
	On     bool
	Link   bool
	PodIP  string
	HostIP string
}

// ErrMovedToNetwork is a console command firmware 3.9.0 moved to the JSON API: the console answers
// "<cmd>: moved to the network API (JSON <name>) ..." instead of running it.
var ErrMovedToNetwork = errors.New("firmware 3.9.0 moved this command from the USB console to the network API")

// movedLine is the marker of that answer.
const movedLine = "moved to the network API"

// ErrNoUSBNet means the pod's console reports no USB network adapter: firmware older than 3.9.0.
var ErrNoUSBNet = errors.New("the pod's firmware has no USB network adapter (needs firmware 3.9.0 or later)")

// ParseUSBNet reads the `usb` line of the console's `status` output. It returns ErrNoUSBNet when
// there is none.
func ParseUSBNet(status string) (USBNet, error) {
	for _, ln := range strings.Split(strings.ReplaceAll(status, "\r", "\n"), "\n") {
		k, v, ok := strings.Cut(ln, ":")
		if !ok || strings.TrimSpace(k) != "usb" {
			continue
		}
		u := USBNet{On: true} // the line without a net field (pre-release builds): on
		f := strings.Fields(v)
		for i := 0; i+1 < len(f); i++ {
			switch f[i] {
			case "net":
				u.On = f[i+1] == "on"
			case "pod":
				u.PodIP = f[i+1]
			case "host":
				u.HostIP = f[i+1]
			case "link":
				u.Link = f[i+1] == "up"
			}
		}
		if net.ParseIP(u.PodIP).To4() == nil || net.ParseIP(u.HostIP).To4() == nil {
			return USBNet{}, fmt.Errorf("unexpected usb status line %q", strings.TrimSpace(ln))
		}
		return u, nil
	}
	return USBNet{}, ErrNoUSBNet
}

// USBNetwork runs `status` and returns the USB network adapter's state.
func (c *Console) USBNetwork(ctx context.Context) (USBNet, error) {
	out, err := c.sendCommand(ctx, "status")
	if err != nil {
		return USBNet{}, err
	}
	return ParseUSBNet(out)
}

// USBReattach makes the pod drop off the USB bus for half a second and connect again, so the
// computer enumerates the console and the network adapter anew. The console closes; reopen it.
func (c *Console) USBReattach(ctx context.Context) error {
	out, err := c.sendCommand(ctx, "usb-reattach")
	switch {
	case strings.Contains(out, "usb-reattach ok"), errors.Is(err, errPortVanished):
		return nil // the port may close before the prompt arrives
	case strings.Contains(strings.ToLower(out), "unknown command"):
		return ErrNoUSBNet
	case err != nil:
		return err
	}
	return fmt.Errorf("unexpected reply to usb-reattach: %q", strings.TrimSpace(cleanConsoleOutput(out, "usb-reattach")))
}
