package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.bug.st/serial"
)

// NormalizePodName turns the ways a pod is named into its identity short id: "baea06",
// "benchpod-baea06" (its embeddedci.com and mDNS host name) and "BenchPod baea06" (its mDNS
// instance) all give "baea06".
func NormalizePodName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, p := range []string{"benchpod-", "benchpod "} {
		s = strings.TrimPrefix(s, p)
	}
	return strings.TrimSuffix(s, ".local")
}

// foundPod is a pod console found while probing.
type foundPod struct {
	c    *Console
	port string
	id   string // identity short id, "" when it has none
}

// openPort is a seam for tests: open a port as a console.
var openPort = func(name string) (*Console, error) {
	port, err := serial.Open(name, &serial.Mode{BaudRate: baudRate})
	if err != nil {
		return nil, err
	}
	c := newConsole(port)
	c.logf = serialLogf
	return c, nil
}

// OpenBenchpodSelect opens the console of one pod on USB when several may be plugged in. With
// name ("baea06", "benchpod-baea06") it opens the pod with that identity; without, the only pod,
// and it errors listing them when there are several (one pod's console shows up twice on macOS,
// as /dev/cu.* and /dev/tty.*; the identity tells those apart from a second pod). The cached
// hint is probed first. explicit != "" opens that device as OpenBenchpod does.
func OpenBenchpodSelect(explicit, preferred, name string, probeTimeout time.Duration) (*Console, string, error) {
	if strings.TrimSpace(explicit) != "" {
		return OpenBenchpod(explicit, preferred, probeTimeout)
	}
	want := NormalizePodName(name)
	cands, vid, err := candidatePortsVID()
	if err != nil {
		return nil, "", err
	}
	cands = preferFirst(strings.TrimSpace(preferred), cands)
	if len(cands) == 0 {
		return nil, "", noPod(errors.New("no USB ports found; plug in the pod or pass --connection <device>"))
	}
	var found []foundPod
	var denied []string
	closeAll := func(keep string) {
		for _, f := range found {
			if f.port != keep {
				_ = f.c.Close()
			}
		}
	}
	for _, port := range cands {
		// Once one pod is found, only the ports with a pod's USB VID can hold another.
		if len(found) > 0 && !isBenchPodVID(vid[port]) {
			continue
		}
		serialLogf("probing %s for a bench-pod console...", port)
		c, err := openPort(port)
		if err != nil {
			serialLogf("  %s: open failed: %v", port, err)
			if isPermissionDenied(err) {
				denied = append(denied, port)
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		ok := c.IsBenchpod(ctx)
		cancel()
		if !ok {
			_ = c.Close()
			continue
		}
		ctx, cancel = context.WithTimeout(context.Background(), probeTimeout)
		st, _ := c.Identity(ctx)
		cancel()
		f := foundPod{c: c, port: port, id: st.ShortID}
		serialLogf("  %s: pod %q", port, f.id)
		if want != "" && f.id == want {
			closeAll("")
			return c, port, nil
		}
		dup := false
		for _, g := range found {
			if g.id != "" && g.id == f.id {
				dup = true // the same pod on its second device node
			}
		}
		if dup {
			_ = c.Close()
			continue
		}
		found = append(found, f)
	}
	if want != "" {
		closeAll("")
		return nil, "", noPod(fmt.Errorf("no pod named %s on USB (found: %s)", want, listFound(found)))
	}
	switch len(found) {
	case 0:
		return nil, "", noPod(fmt.Errorf("no bench-pod console found on USB%s; pass --connection <device> to force one", deniedHint(denied)))
	case 1:
		return found[0].c, found[0].port, nil
	}
	closeAll("")
	return nil, "", fmt.Errorf("%d pods on USB (%s): pick one with --connection usb:<name>, e.g. usb:benchpod-%s",
		len(found), listFound(found), found[0].id)
}

func listFound(found []foundPod) string {
	if len(found) == 0 {
		return "none"
	}
	var parts []string
	for _, f := range found {
		id := f.id
		if id == "" {
			id = "no identity"
		} else {
			id = "benchpod-" + id
		}
		parts = append(parts, fmt.Sprintf("%s on %s", id, f.port))
	}
	return strings.Join(parts, ", ")
}
