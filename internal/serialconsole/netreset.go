package serialconsole

import (
	"context"
	"errors"
	"strings"
)

// ErrNoNetReset is a console without `net-reset`: firmware before 4.0.
var ErrNoNetReset = errors.New("the pod's console has no net-reset (it needs firmware 4.0: `benchpod flash-self`)")

// NetReset runs the console's `net-reset` (firmware 4.0): the pod forgets its Wi-Fi credentials
// (its flash and the ESP32-C3's NVS, or the Wi-Fi bridge's), which turns USB networking back on,
// and puts Ethernet back to autonegotiation with a PHY reset and a new DHCP lease. It returns
// the pod's `net-reset ...` lines: first the Ethernet one, then the Wi-Fi outcome.
func (c *Console) NetReset(ctx context.Context) ([]string, error) {
	done := func(acc string) bool { return strings.Contains(acc, "net-reset wifi") || strings.Contains(acc, "net-reset error") }
	out, err := c.sendCommandUntil(ctx, "net-reset", "net-reset", "", done)
	var lines []string
	for _, ln := range consoleLines(out) {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "net-reset ") {
			lines = append(lines, ln)
		}
	}
	if strings.Contains(strings.ToLower(out), "unknown command") {
		return nil, ErrNoNetReset
	}
	for _, ln := range lines {
		if why, ok := strings.CutPrefix(ln, "net-reset error "); ok {
			return lines, &CommandError{Cmd: "net-reset", Reason: why}
		}
	}
	if err != nil {
		return lines, err
	}
	return lines, nil
}
