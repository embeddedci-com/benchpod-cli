package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ── status ───────────────────────────────────────────────────────────────────

func newStatusCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the pod's firmware, I/O voltage, target power, network and cloud state",
		Long: "Show a short summary of the pod: firmware and gateware version (and whether a\n" +
			"newer firmware release exists), the board I/O (LA) voltage, target power, the\n" +
			"network address, whether it is registered with embeddedci.com and, when this\n" +
			"machine is signed in, whether it is on your account, and a cloud job's lease.\n\n" +
			"--json prints the pod's raw `status` reply instead (network and embeddedci.com).\n" +
			"Over USB (--connection usb) this prints the console's `status` text. With\n" +
			"--connection embeddedci:<name> it asks the pod through embeddedci.com.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			spec, err := g.resolveTarget()
			if err != nil {
				return err
			}
			if spec.IsSerial() {
				return runStatusSerial(g, spec.Device)
			}

			ctx, cancel, pod, err := g.podClient("status", 30*time.Second)
			if err != nil {
				return err
			}
			defer cancel()

			data, err := pod.client.Command(ctx, map[string]any{"cmd": "status"})
			if err != nil {
				return fmt.Errorf("status: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("status: open output: %w", err)
			}
			defer closeOut()
			if asJSON {
				printJSON(out, data)
				return nil
			}
			var st podStatus
			if err := json.Unmarshal(data, &st); err != nil {
				printJSON(out, data) // not the shape we know: show it as it is
				return nil
			}
			sum := statusSummary{addr: pod.label, st: st, cloudID: pod.deviceID}
			gatherStatusExtras(ctx, pod.client, &sum)
			printStatusSummary(out, sum)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the pod's raw status JSON instead of the summary")
	return cmd
}

// runStatusSerial runs the firmware's `status` command over the USB serial
// console and prints its (plain-text) output.
func runStatusSerial(g *globalFlags, device string) error {
	console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(15*time.Second))
	if err != nil {
		return err
	}
	defer cancel()
	defer console.Close()

	text, err := console.Status(ctx)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	out, closeOut, err := resolveOutput(g.outputFilename)
	if err != nil {
		return fmt.Errorf("status: open output: %w", err)
	}
	defer closeOut()
	fmt.Fprintln(out, text)
	return nil
}

// podStatus is the part of the firmware's `status` reply the summary shows.
type podStatus struct {
	Version          string `json:"version"`
	Board            string `json:"board"`
	BoardRev         string `json:"board_rev"`
	Net              string `json:"net"`
	IP               string `json:"ip"`
	RSSI             *int   `json:"rssi_dbm"`
	Wifi             string `json:"wifi"`
	Cloud            string `json:"cloud"`
	LAVccioMV        int    `json:"la_vccio_mv"`
	Gateware         int    `json:"gateware"`
	GatewareEmbedded int    `json:"gateware_embedded"`
	SafeMode         bool   `json:"safe_mode"`
	SafeReason       string `json:"safe_reason"`
	Lease            *struct {
		Held   bool   `json:"held"`
		Holder string `json:"holder"`
		LeftS  int    `json:"left_s"`
	} `json:"lease"`
}

// targetPower is the firmware's `target_status` reply: eFuse 1 is the internal 5 V rail, eFuse 2
// the external supply.
type targetPower struct {
	Efuse1 struct {
		Enabled int `json:"enabled"`
		Fault   int `json:"fault"`
	} `json:"efuse1"`
	Efuse2 struct {
		Enabled int `json:"enabled"`
		Fault   int `json:"fault"`
	} `json:"efuse2"`
}

// statusSummary is everything the summary prints; the extras are optional (nil/"" = unknown).
type statusSummary struct {
	addr    string // the pod's address, or "<name> on embeddedci.com"
	cloudID string // the pod's id on embeddedci.com when status went through it
	st      podStatus
	cloud   *cloudState
	power   *targetPower
	account map[string]string // signed-in account's devices by id, nil = not signed in / unknown
	latest  string            // latest firmware release tag, "" = unknown
}

// gatherStatusExtras asks the pod for its registration and target power, and (cheaply, never
// failing the command) whether it is on the signed-in account and the latest firmware release.
func gatherStatusExtras(ctx context.Context, client podCommander, sum *statusSummary) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if raw, err := client.Command(cctx, map[string]any{"cmd": "cloud_status"}); err == nil {
		var c cloudState
		if json.Unmarshal(raw, &c) == nil {
			c.known = true
			sum.cloud = &c
		}
	}
	cancel()
	cctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	if raw, err := client.Command(cctx, map[string]any{"cmd": "target_status"}); err == nil {
		var p targetPower
		if json.Unmarshal(raw, &p) == nil {
			sum.power = &p
		}
	}
	cancel()
	switch {
	case sum.cloudID != "" && sum.cloud != nil:
		// Reached through embeddedci.com by name: the pod is on the account that asked.
		name := strings.TrimSuffix(sum.addr, " on embeddedci.com")
		sum.account = map[string]string{sum.cloudID: name, strings.TrimSpace(sum.cloud.DeviceID): name}
	case sum.cloud != nil && sum.cloud.Configured && strings.TrimSpace(sum.cloud.DeviceID) != "":
		sum.account = accountDevices()
	}
	sum.latest = latestFirmwareRelease()
}

// printStatusSummary writes the readable status.
func printStatusSummary(out io.Writer, s statusSummary) {
	st := s.st
	row := func(k, v string) { fmt.Fprintf(out, "  %-13s %s\n", k, v) }

	if s.cloudID != "" {
		fmt.Fprintf(out, "BenchPod %s\n", s.addr)
	} else {
		fmt.Fprintf(out, "BenchPod at %s\n", s.addr)
	}
	fw := valueOrDash(st.Version)
	if s.latest != "" && st.Version != "" {
		if versionLess(st.Version, s.latest) {
			fw += fmt.Sprintf(" (%s is available: install it from the web app or with `benchpod flash-self`)", s.latest)
		} else {
			fw += " (latest)"
		}
	}
	row("Firmware", fw)
	if st.Gateware != 0 {
		gw := strconv.Itoa(st.Gateware)
		if st.GatewareEmbedded != 0 && st.GatewareEmbedded != st.Gateware {
			gw += fmt.Sprintf(" (the firmware carries %d; it is installed on the next boot)", st.GatewareEmbedded)
		}
		row("Gateware", gw)
	}
	if board := strings.TrimSpace(st.Board + " " + st.BoardRev); board != "" {
		row("Board", board)
	}
	if st.LAVccioMV != 0 {
		row("I/O voltage", formatMV(st.LAVccioMV))
	} else {
		row("I/O voltage", "not set (run `benchpod la voltage 3.3V` or 1.8V; LA, UART and flash need it)")
	}
	if s.power != nil {
		row("Target power", describeTargetPower(*s.power))
	}
	row("Network", describeNetwork(st))
	row("Cloud", describeStatusCloud(st, s.cloud, s.account))
	if st.Lease != nil && st.Lease.Held {
		row("Lease", fmt.Sprintf("held by cloud job %s (%d s left); the LAN is read-only until it ends",
			valueOrDash(st.Lease.Holder), st.Lease.LeftS))
	}
	if st.SafeMode {
		row("Safe mode", valueOrDash(st.SafeReason))
	}
}

func describeTargetPower(p targetPower) string {
	rail := func(name string, enabled, fault int) string {
		switch {
		case fault != 0:
			return name + " FAULT"
		case enabled != 0:
			return name + " on"
		}
		return ""
	}
	var on []string
	for _, r := range []string{rail("internal 5 V", p.Efuse1.Enabled, p.Efuse1.Fault), rail("external", p.Efuse2.Enabled, p.Efuse2.Fault)} {
		if r != "" {
			on = append(on, r)
		}
	}
	if len(on) == 0 {
		return "off"
	}
	return strings.Join(on, ", ")
}

func describeNetwork(st podStatus) string {
	out := strings.TrimSpace(st.IP)
	if out == "" || out == "0.0.0.0" {
		out = "no address"
	}
	if n := strings.TrimSpace(st.Net); n != "" {
		out += " (" + n + ")"
	}
	if w := strings.TrimSpace(st.Wifi); w != "" {
		out += ", Wi-Fi " + w
		if st.RSSI != nil {
			out += fmt.Sprintf(" %d dBm", *st.RSSI)
		}
	}
	return out
}

// describeStatusCloud says whether the pod is registered and connected, and whose account it is
// on when that is known.
func describeStatusCloud(st podStatus, c *cloudState, account map[string]string) string {
	if c == nil {
		if st.Cloud != "" {
			return st.Cloud
		}
		return "unknown"
	}
	if !c.Configured {
		return "not registered (`benchpod setup`, or `benchpod login` then `benchpod register`)"
	}
	out := "registered"
	if state := strings.ToLower(strings.TrimSpace(c.State)); state != "" {
		out += ", " + state
	}
	if account != nil {
		if name, ok := account[strings.TrimSpace(c.DeviceID)]; ok {
			out += ", on your account as " + name
		} else {
			out += ", on a different account than the one this machine is signed in to"
		}
	}
	return out
}

// latestFirmwareRelease returns the tag of the latest public firmware release, or "" when it is
// not known within a short deadline. It reads the redirect of GitHub's releases/latest page
// rather than downloading anything. A variable so tests never touch the network.
var latestFirmwareRelease = func() string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		"https://github.com/"+defaultFirmwareRepo+"/releases/latest", nil)
	if err != nil {
		return ""
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(loc[i+len("/tag/"):])
}

// versionLess compares dotted versions ("3.8.0", "v3.9.1", "3.8.0-rc1" counts as 3.8.0).
// Anything unparsable compares as not less, so the summary never claims an update it is unsure of.
func versionLess(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
