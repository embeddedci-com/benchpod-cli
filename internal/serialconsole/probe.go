package serialconsole

import (
	"context"
	"strings"
	"time"

	"go.bug.st/serial"
)

// ── discovery over USB ──────────────────────────────────────────────────────

// SerialPod is a bench pod identified on a USB serial port by probing it with
// the firmware's `status` command. The parsed fields are best-effort: the
// console interleaves async log lines, so a missing field is left empty rather
// than treated as an error. Status keeps the raw text for display.
type SerialPod struct {
	Device   string // the /dev node or COM port the pod answered on
	Board    string // "bench-pod" (the board line, minus the firmware version)
	Firmware string // "v1.4.2", from the same line
	IP       string // the pod's address, "" or "0.0.0.0" when it has no lease
	MAC      string // wired MAC
	Status   string // raw `status` output

	// Registered reports whether the pod already holds a cloud provisioning — i.e. it was
	// claimed into an account, possibly by somebody else before it was shipped. This comes
	// from the console rather than the TCP/JSON `cloud_status`, so it is answerable over USB
	// alone: a pod straight out of its box has no network for the JSON API to answer over.
	// CloudKnown is false against firmware too old to print the line, which must read as
	// "unknown", never as "not registered".
	CloudKnown bool
	Registered bool
	CloudState string // "connected", "connecting", "backoff", ... when registered
	DeviceID   string // the provisioned device id, when registered

	// PublicKey is the pod's Ed25519 identity (base64url), the same key the pod advertises as
	// the mDNS TXT "id". It matches a USB pod to its network entry when the two report
	// different addresses (USB shows the wired IP, mDNS may answer on Wi-Fi) and the pod is not
	// registered yet, so there is no device id to match on. "" on firmware without `identity`.
	PublicKey string
}

// Addressed reports whether the pod holds a usable IP address (it has a DHCP
// lease on one of its interfaces), as opposed to no network at all.
func (p SerialPod) Addressed() bool {
	ip := strings.TrimSpace(p.IP)
	return ip != "" && ip != "0.0.0.0" && ip != "-"
}

// ProbeSerial enumerates USB serial ports and probes each for a bench-pod
// console, returning every pod found (not just the first, unlike OpenBenchpod)
// together with the ports that answered but were not bench pods.
//
// It exists for `discover`, which reports on a whole bench rather than opening
// one pod: a machine can legitimately have several pods plugged in, and the
// ports that were ruled out are worth showing when nothing was found at all.
// Each port is opened, probed and closed before moving on, so no port is left
// held. probeTimeout bounds each probe individually — a real pod answers in
// well under a second, so only non-pod ports run it out.
func ProbeSerial(preferred string, probeTimeout time.Duration) (pods []SerialPod, skipped []string, err error) {
	cands, err := candidatePorts()
	if err != nil {
		return nil, nil, err
	}
	// One physical port shows up as two /dev nodes on macOS — the call-out
	// /dev/cu.X and the dial-in /dev/tty.X — and both answer, so a single pod
	// would otherwise be reported twice. Both are still probed (either can be
	// the one that opens), but only the first answer per pod is kept.
	seen := map[string]bool{}
	for _, name := range preferFirst(strings.TrimSpace(preferred), cands) {
		port, oErr := serial.Open(name, &serial.Mode{BaudRate: baudRate})
		if oErr != nil {
			skipped = append(skipped, name)
			continue
		}
		c := newConsole(port)
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		raw, _ := c.Status(ctx)
		cancel()
		if !strings.Contains(strings.ToLower(raw), benchpodMarker) {
			_ = c.Close()
			skipped = append(skipped, name)
			continue
		}
		pod := parseSerialPod(name, raw)
		// Read-only: `identity` prints the pod's public key and changes nothing.
		ctx, cancel = context.WithTimeout(context.Background(), probeTimeout)
		if id, idErr := c.Identity(ctx); idErr == nil {
			pod.PublicKey = id.PublicKey
		}
		cancel()
		_ = c.Close()
		key := podIdentity(pod)
		if seen[key] {
			continue
		}
		seen[key] = true
		pods = append(pods, pod)
	}
	return pods, skipped, nil
}

// podIdentity keys a pod for de-duplication across the /dev nodes that lead to
// it. The MAC is the pod's own identity and is preferred; when the firmware is
// too old to report one, fall back to the port name with the OS's call-out /
// dial-in prefix stripped, which is what makes cu.usbmodemX and tty.usbmodemX
// collapse to one entry.
func podIdentity(p SerialPod) string {
	if mac := strings.TrimSpace(p.MAC); mac != "" && mac != "-" {
		return "mac:" + strings.ToLower(mac)
	}
	base := p.Device
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimPrefix(strings.TrimPrefix(base, "cu."), "tty.")
	return "dev:" + base
}

// parseSerialPod pulls the identifying fields out of `status` output. The board
// line carries both the board name and the firmware version — "bench-pod  fw
// v1.4.2" — so it is split on the "fw" token.
func parseSerialPod(device, raw string) SerialPod {
	p := SerialPod{
		Device: device,
		IP:     statusField(raw, "ip"),
		MAC:    statusField(raw, "mac"),
		Status: raw,
	}
	board := statusField(raw, "board")
	if i := strings.Index(board, "fw "); i >= 0 {
		p.Board = strings.TrimSpace(board[:i])
		p.Firmware = firstToken(board[i+len("fw "):])
	} else {
		p.Board = strings.TrimSpace(board)
	}
	parseCloudField(&p, statusField(raw, "cloud"))
	return p
}

// parseCloudField reads the console's cloud line into the pod's registration fields. The
// firmware prints either
//
//	cloud  : not registered
//	cloud  : registered  state=connected  device_id=<uuid>
//	cloud  : registered  state=backoff  device_id=<uuid>  last_error=<reason, may hold spaces>
//
// An empty value means the firmware predates the line, which is left as "unknown" rather than
// reported as unregistered — telling somebody their pod is unclaimed when we simply cannot
// tell would send them to re-register a pod that is already working.
func parseCloudField(p *SerialPod, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	p.CloudKnown = true
	if !strings.HasPrefix(value, "registered") {
		return
	}
	p.Registered = true
	for _, tok := range strings.Fields(value) {
		switch {
		case strings.HasPrefix(tok, "state="):
			p.CloudState = strings.TrimPrefix(tok, "state=")
		case strings.HasPrefix(tok, "device_id="):
			p.DeviceID = strings.TrimPrefix(tok, "device_id=")
		}
	}
}

// statusField reads one "label : value" line out of `status` output.
//
// It is deliberately not fieldValue: the console pads its labels into a column
// ("  ip     : 192.168.1.213"), so a "label:" prefix match never fires on this
// output. Splitting on the FIRST colon also keeps colon-bearing values intact,
// which is what makes the mac line parse.
func statusField(raw, label string) string {
	for _, line := range strings.Split(raw, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line[:i]), label) {
			return strings.TrimSpace(line[i+1:])
		}
	}
	return ""
}
