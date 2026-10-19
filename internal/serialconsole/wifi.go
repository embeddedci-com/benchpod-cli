package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// WifiSetResult reports what the firmware did with submitted credentials.
type WifiSetResult struct {
	Persisted bool   // saw "[cfg] credentials written to flash"
	Joined    bool   // saw "[wifi] join OK"
	IP        string // parsed from "[wifi] join OK  ip=<ip>"
	Raw       string // full captured output (password already masked)
}

// WifiSet stores credentials and (re)joins the AP. ssid/password are quoted for
// the firmware shell. A "[wifi] join failed" marker (without a join OK) yields
// an error alongside a result with Persisted reflecting whether creds were saved.
func (c *Console) WifiSet(ctx context.Context, ssid, password string) (WifiSetResult, error) {
	qSSID, err := quoteArg(ssid)
	if err != nil {
		return WifiSetResult{}, fmt.Errorf("ssid: %w", err)
	}
	qPass, err := quoteArg(password)
	if err != nil {
		return WifiSetResult{}, fmt.Errorf("password: %w", err)
	}
	// Send the real command but show a masked form in logs/errors; the firmware
	// echoes typed chars, so `password` is also redacted from any captured output.
	display := "wifi-set " + qSSID + ` "***"`
	out, err := c.sendCommandRedacted(ctx, "wifi-set "+qSSID+" "+qPass, display, password)
	res := WifiSetResult{
		Persisted: strings.Contains(out, "[cfg] credentials written to flash"),
		Raw:       maskPassword(out, password),
	}
	// wifi-set delegates to the esp32-reconnect ladder, which reports success as
	// "[wifi] connected  ip=<ip>". Accept the older direct-join "[wifi] join OK"
	// marker too so a mixed firmware/CLI still works.
	joinMarker := ""
	switch {
	case strings.Contains(out, "[wifi] connected"):
		joinMarker = "[wifi] connected"
	case strings.Contains(out, "[wifi] join OK"):
		joinMarker = "[wifi] join OK"
	}
	if joinMarker != "" {
		res.Joined = true
		res.IP = parseIPAfter(out, joinMarker)
	}
	c.logln("wifi-set result: persisted=%t joined=%t ip=%s", res.Persisted, res.Joined, res.IP)
	if err != nil {
		return res, err // already masked by sendCommandRedacted (secret = password)
	}
	// "  wifi-set: <why>": nothing was saved (too long, config save failed, or the Wi-Fi bridge
	// refused or did not answer).
	if !res.Persisted {
		if why := wifiSetError(out); why != "" {
			return res, fmt.Errorf("wifi-set: %s (nothing was saved)", why)
		}
	}
	if !res.Joined && (strings.Contains(out, "[wifi] connect failed") ||
		strings.Contains(out, "[wifi] join failed") ||
		strings.Contains(out, "ESP32 still unreachable")) {
		return res, errors.New("wifi join failed (credentials were saved; check the password/SSID and signal)")
	}
	return res, nil
}

// wifiSetError is the reason in a "wifi-set: <why>" reply line, "" when there is none.
func wifiSetError(out string) string {
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", "\n"), "\n") {
		if t := strings.TrimSpace(ln); strings.HasPrefix(t, "wifi-set: ") {
			return strings.TrimPrefix(t, "wifi-set: ")
		}
	}
	return ""
}

// BringupLines returns the WiFi / TCP / ESP32 bring-up log lines from captured
// console output (e.g. WifiSetResult.Raw), so callers can show the connection
// result and assigned IP without the lower-level AT/boot noise.
func BringupLines(raw string) []string {
	var out []string
	for _, ln := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[wifi]") || strings.HasPrefix(t, "[tcp]") ||
			strings.HasPrefix(t, "[esp32]") || strings.HasPrefix(t, "ESP32 ") {
			out = append(out, t)
		}
	}
	return out
}

// WifiStatus is the parsed result of show-network (firmware: wifi-show).
type WifiStatus struct {
	SSID  string
	State string
	IP    string
	RSSI  string
	// Via is set when a Wi-Fi bridge on the pod's Ethernet port carries Wi-Fi (firmware with
	// bridge support): "wifi-bridge <mac> fw <ver> hw <rev>, paired". State is then the bridge's
	// ("unconfigured", "wait-pod", "joining", "connected", "auth-failed", "no-ap") and IP the
	// pod's Ethernet lease, which it has whether or not the bridge has joined.
	Via     string
	Problem string // what the pod sees wrong with the bridge, "" if nothing
	Raw     string
}

// ViaBridge reports whether a Wi-Fi bridge carries the pod's Wi-Fi.
func (s WifiStatus) ViaBridge() bool { return strings.HasPrefix(s.Via, "wifi-bridge") }

// BridgeJoined reports whether the bridge has joined its network.
func (s WifiStatus) BridgeJoined() bool { return s.ViaBridge() && s.State == "connected" }

// BridgeJoinFailure is why the bridge cannot join, "" while it is joining or has joined.
func (s WifiStatus) BridgeJoinFailure() string {
	if !s.ViaBridge() {
		return ""
	}
	switch s.State {
	case "auth-failed":
		return "the Wi-Fi bridge could not join: wrong password (or the network refused it)"
	case "no-ap":
		return "the Wi-Fi bridge could not join: it does not see that network"
	}
	return ""
}

// WifiShow reports stored SSID, WiFi state, IP, and RSSI. Missing fields are
// left empty rather than treated as errors, since async log lines may displace
// them in a given capture window.
//
// While Wi-Fi retries, the firmware logs "[wifi] ...", "[esp] ..." and
// "[cloud] ..." lines every few seconds from other tasks, and those are written
// in chunks, so the reply's lines can land in the middle of one. The read waits
// for the last line ("  rssi:") before it trusts a prompt, and the fields are
// matched anywhere in a line (see wifiField).
func (c *Console) WifiShow(ctx context.Context) (WifiStatus, error) {
	out, err := c.sendCommandUntil(ctx, "wifi-show", "wifi-show", "", wifiShowComplete)
	if err != nil {
		return WifiStatus{Raw: out}, err
	}
	return parseWifiShow(out), nil
}

// parseWifiShow extracts the wifi-show fields from a captured reply.
func parseWifiShow(out string) WifiStatus {
	return WifiStatus{
		SSID:    wifiField(out, "ssid"),
		State:   wifiField(out, "state"),
		IP:      wifiField(out, "ip"),
		RSSI:    wifiField(out, "rssi"),
		Via:     wifiField(out, "via"),
		Problem: wifiField(out, "problem"),
		Raw:     out,
	}
}

// wifiShowComplete reports whether the prompt has arrived after the reply's
// last line. The firmware always prints "  rssi:" last, even with no value.
func wifiShowComplete(acc string) bool {
	i := strings.LastIndex(strings.ToLower(acc), "rssi:")
	return i >= 0 && promptSeen(acc[i:], defaultPrompt)
}

// wifiField returns the value of a wifi-show "<label>:" line. A line that
// starts with the label wins (the last one, so a stale earlier reply in the
// same capture loses). Failing that, it looks for "  <label>:" inside a line
// that starts with "[": a reply line that landed in the middle of an async log
// line, e.g. "[wifi] association failed  ssid: Net". The two-space indent is
// the firmware's, so log text such as "[wifi] connected  ip=..." never matches.
func wifiField(s, label string) string {
	s = strings.ReplaceAll(s, "\r", "\n")
	if v, ok := lastFieldValue(s, label); ok {
		return v
	}
	needle := "  " + strings.ToLower(label) + ":"
	val, found := "", false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		if i := strings.LastIndex(strings.ToLower(line), needle); i >= 0 {
			val, found = strings.TrimSpace(line[i+len(needle):]), true
		}
	}
	if found {
		return val
	}
	return ""
}

// lastFieldValue is fieldValue that keeps the last matching line and reports
// whether any line matched, so an empty value ("  ssid: ") still counts.
func lastFieldValue(s, label string) (string, bool) {
	label = strings.ToLower(label)
	val, found := "", false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		for _, sep := range []string{":", "="} {
			if prefix := label + sep; strings.HasPrefix(lower, prefix) {
				val, found = strings.TrimSpace(trimmed[len(prefix):]), true
				break
			}
		}
	}
	return val, found
}

// WifiClear erases the stored credentials and takes Wi-Fi down. Firmware after 3.7.0 also erases
// the ESP32-C3's own copy before it answers, which takes a few seconds and up to about 30 s when
// the C3's ROM loader is slow to answer: give ctx a deadline well past that.
func (c *Console) WifiClear(ctx context.Context) error {
	_, err := c.sendCommand(ctx, "wifi-clear")
	return err
}

// WifiNetwork is one network from WifiScan.
type WifiNetwork struct {
	SSID    string
	RSSI    int // dBm
	Channel int
	Auth    string // "open", "wpa2", "wpa2/wpa3", ...
}

// ErrNoWifiScan is what WifiScan returns for firmware that cannot scan with the Wi-Fi this pod
// has: firmware before 3.9.4 (no wifi-scan, or a development build that scans only through a
// Wi-Fi bridge). Whether the pod has Wi-Fi at all is a separate question (WifiShow).
var ErrNoWifiScan = errors.New("this pod's firmware cannot scan for Wi-Fi networks (scanning needs firmware 3.9.4 or later)")

// WifiScan lists the networks the pod's Wi-Fi sees, strongest first: its built-in Wi-Fi
// (firmware 3.9.4+), or a Wi-Fi bridge on its Ethernet port (development builds). Firmware
// wifi-scan answers "scan <rssi> <channel> <auth> <ssid>" lines, then "scan done <n>"; the
// built-in Wi-Fi prints them a few seconds after the prompt, when the scan ends, so this waits
// for "scan done" (or an error) rather than the first prompt.
func (c *Console) WifiScan(ctx context.Context) ([]WifiNetwork, error) {
	out, err := c.sendCommandWait(ctx, "wifi-scan", "wifi-scan", "", wifiScanComplete, 0)
	if err != nil {
		return nil, err
	}
	return parseWifiScan(out)
}

func wifiScanComplete(acc string) bool {
	i := -1
	for _, mark := range []string{"scan done", "wifi-scan error", "unknown command 'wifi-scan'"} {
		if j := strings.LastIndex(acc, mark); j > i {
			i = j
		}
	}
	return i >= 0 && promptSeen(acc[i:], defaultPrompt)
}

func parseWifiScan(out string) ([]WifiNetwork, error) {
	nets := []WifiNetwork{}
	done := false
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", "\n"), "\n") {
		t := strings.TrimLeft(ln, " \t") // not the right: an SSID may end in a space
		switch {
		case strings.HasPrefix(t, "wifi-scan error ") && strings.Contains(t, "no Wi-Fi bridge"):
			// A development build from before 3.9.4: it scans only through a bridge.
			return nil, ErrNoWifiScan
		case strings.HasPrefix(t, "wifi-scan error "):
			return nil, fmt.Errorf("wifi-scan: %s", strings.TrimSpace(strings.TrimPrefix(t, "wifi-scan error ")))
		case strings.Contains(t, "unknown command 'wifi-scan'"):
			return nil, ErrNoWifiScan
		case strings.HasPrefix(t, "scan done"):
			done = true
		case strings.HasPrefix(t, "scan "):
			// The SSID is last and may hold spaces.
			f := strings.SplitN(strings.TrimPrefix(t, "scan "), " ", 4)
			if len(f) < 3 {
				continue
			}
			rssi, err1 := strconv.Atoi(f[0])
			ch, err2 := strconv.Atoi(f[1])
			if err1 != nil || err2 != nil {
				continue
			}
			n := WifiNetwork{RSSI: rssi, Channel: ch, Auth: f[2]}
			if len(f) == 4 {
				n.SSID = f[3]
			}
			nets = append(nets, n)
		}
	}
	if !done {
		return nil, fmt.Errorf("wifi-scan: the pod did not finish the list (got %q)", tail([]byte(out), 200))
	}
	return nets, nil
}
