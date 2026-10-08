package serialconsole

import (
	"context"
	"errors"
	"fmt"
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
	if !res.Joined && (strings.Contains(out, "[wifi] connect failed") ||
		strings.Contains(out, "[wifi] join failed") ||
		strings.Contains(out, "ESP32 still unreachable")) {
		return res, errors.New("wifi join failed (credentials were saved; check the password/SSID and signal)")
	}
	return res, nil
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
	Raw   string
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
		SSID:  wifiField(out, "ssid"),
		State: wifiField(out, "state"),
		IP:    wifiField(out, "ip"),
		RSSI:  wifiField(out, "rssi"),
		Raw:   out,
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

// WifiClear erases stored credentials. A reboot is needed to fully apply.
func (c *Console) WifiClear(ctx context.Context) error {
	_, err := c.sendCommand(ctx, "wifi-clear")
	return err
}
