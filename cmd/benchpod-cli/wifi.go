package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── Wi-Fi over JSON ─────────────────────────────────────────────────────────
//
// set-wifi, show-network, scan-wifi and clear-wifi send the pod's JSON commands (wifi_set,
// wifi_status, wifi_scan, wifi_clear) over its USB network adapter (usbNetworkTarget): firmware
// 4.0 has no Wi-Fi commands on its console. Wi-Fi and USB networking are one or the other on the
// pod: wifi_set turns USB networking off (its reply still goes out over it), wifi_clear turns it
// back on. When no network reaches the pod at all, the console's `net-reset` (benchpod net-reset)
// forgets the Wi-Fi.

// wifiState is the part of the pod's wifi_status reply the CLI reads.
type wifiState struct {
	State      string `json:"state"`
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	SSID       string `json:"ssid"`
	Via        string `json:"via"` // "wifi-bridge" when a Wi-Fi bridge on the Ethernet port carries Wi-Fi
	RSSI       *int   `json:"rssi_dbm"`
	Bridge     *struct {
		Paired  bool    `json:"paired"`
		Problem *string `json:"problem"`
		RSSI    *int    `json:"rssi_dbm"`
	} `json:"bridge"`
}

func (s wifiState) viaBridge() bool { return s.Via == "wifi-bridge" }

// rssi is the signal of the link that carries Wi-Fi ("" when not joined).
func (s wifiState) rssi() string {
	r := s.RSSI
	if s.viaBridge() && s.Bridge != nil {
		r = s.Bridge.RSSI
	}
	if r == nil {
		return ""
	}
	return strconv.Itoa(*r) + " dBm"
}

// problem is what the pod sees wrong with its Wi-Fi bridge, "" if nothing.
func (s wifiState) problem() string {
	if s.Bridge != nil && s.Bridge.Problem != nil {
		return *s.Bridge.Problem
	}
	return ""
}

// bridgeJoinFailure is why the bridge cannot join, "" while it is joining or has joined.
func (s wifiState) bridgeJoinFailure() string {
	switch s.State {
	case "auth-failed":
		return "the Wi-Fi bridge could not join: wrong password (or the network refused it)"
	case "no-ap":
		return "the Wi-Fi bridge could not join: it does not see that network"
	}
	return ""
}

// joined says the pod (or its bridge) is joined to ssid.
func (s wifiState) joined(ssid string) bool {
	return (s.Connected || s.State == "connected") && strings.TrimSpace(s.SSID) == strings.TrimSpace(ssid)
}

func readWifi(ctx context.Context, c podCommander) (wifiState, error) {
	var st wifiState
	raw, err := c.Command(ctx, map[string]any{"cmd": "wifi_status"})
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("wifi_status: unexpected reply %s", strings.TrimSpace(string(raw)))
	}
	return st, nil
}

// podIP is the pod's Ethernet (else Wi-Fi) address from its status, "" when it has none.
func podIP(ctx context.Context, c podCommander) string {
	raw, err := c.Command(ctx, map[string]any{"cmd": "status", "caps": false})
	if err != nil {
		return ""
	}
	var st struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(raw, &st) != nil {
		return ""
	}
	return usableIP(st.IP)
}

func usableIP(ip string) string {
	ip = strings.TrimSpace(ip)
	if p := net.ParseIP(ip); p == nil || p.IsUnspecified() {
		return ""
	}
	return ip
}

// ── set-wifi ─────────────────────────────────────────────────────────────────

func newSetWifiCmd(g *globalFlags) *cobra.Command {
	var ssid, password string
	var passwordStdin, skipBlobs bool
	cmd := &cobra.Command{
		Use:   "set-wifi",
		Short: "Save Wi-Fi credentials and join (over the pod's USB network adapter)",
		Long: "Save Wi-Fi credentials on the pod and wait for it to join. The CLI sends them\n" +
			"over the pod's USB network adapter (wifi_set). Wi-Fi and USB networking are one or\n" +
			"the other on the pod: once the credentials are stored, USB networking turns off\n" +
			"(`benchpod clear-wifi` over the pod's network address, or `benchpod net-reset`\n" +
			"over USB, turns it back on). With a Wi-Fi bridge on the pod's Ethernet port the\n" +
			"bridge keeps them and USB networking stays on.\n\n" +
			"USB networking off: `benchpod usb-network on --connection usb` (the pod's\n" +
			"console `usb-network on`).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(ssid) == "" {
				return usageErrorf("--ssid is required")
			}
			pw, err := resolveWifiPassword(password, passwordStdin)
			if err != nil {
				return fmt.Errorf("set-wifi: %w", err)
			}
			_, err = runSetWifi(g, ssid, pw, skipBlobs)
			return err
		},
	}
	cmd.Flags().StringVar(&ssid, "ssid", "", "Wi-Fi SSID (required)")
	cmd.Flags().StringVar(&password, "password", "", "Wi-Fi password (insecure: visible in shell history); omit to be prompted")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the Wi-Fi password from the first line of stdin")
	cmd.Flags().BoolVar(&skipBlobs, "skip-blobs", false, "do not install a missing ESP32-C3 image first (not needed through a Wi-Fi bridge)")
	return cmd
}

// wifiJoinWait bounds how long set-wifi waits for the pod (or its bridge) to join; wifiJoinPoll is
// how often it asks. Variables for the tests.
var (
	wifiJoinWait = 45 * time.Second
	wifiJoinPoll = 3 * time.Second
)

// runSetWifi saves Wi-Fi credentials over the pod's network API and waits for the join, returning
// the pod's address when the join is confirmed ("" when it is not). Shared by set-wifi and setup.
func runSetWifi(g *globalFlags, ssid, pw string, skipBlobs bool) (string, error) {
	ctx, cancel, t, err := g.usbNetworkClient("set-wifi", 90*time.Second)
	if err != nil {
		return "", err
	}
	defer cancel()

	before, err := readWifi(ctx, t.client)
	if err != nil {
		return "", wifiError("set-wifi", err)
	}
	// Wi-Fi runs on the ESP32-C3, whose image the pod keeps in its W25Q: a pod without it (fresh
	// board, no network) can only get it over USB.
	if !before.viaBridge() && !skipBlobs && t.spec.IsNetwork() {
		if err := ensureEspBlob(ctx, t.spec.Addr); err != nil {
			return "", fmt.Errorf("set-wifi: %w", err)
		}
	}
	raw, err := t.client.Command(ctx, map[string]any{"cmd": "wifi_set", "ssid": ssid, "password": pw})
	if err != nil {
		return "", wifiError("set-wifi", err)
	}
	var rep struct {
		Via string `json:"via"`
	}
	_ = json.Unmarshal(raw, &rep)
	bridge := before.viaBridge() || rep.Via == "wifi-bridge"
	if bridge {
		fmt.Fprintf(os.Stderr, "Saved Wi-Fi credentials for SSID %q on the pod's Wi-Fi bridge via %s; waiting for it to join...\n", ssid, t.where())
	} else {
		fmt.Fprintf(os.Stderr, "Saved Wi-Fi credentials for SSID %q via %s; waiting for the pod to join "+
			"(USB networking turns off now)...\n", ssid, t.where())
	}
	return waitWifiJoin(ctx, g, t, ssid, bridge)
}

// waitWifiJoin follows the join: over the connection the credentials went over while it still
// works (a bridge, the pod's Ethernet, embeddedci.com), else over the address the pod's console
// status reports once it has one.
func waitWifiJoin(ctx context.Context, g *globalFlags, t *podTarget, ssid string, bridge bool) (string, error) {
	deadline := time.Now().Add(wifiJoinWait)
	var st wifiState
	for {
		var c podCommander = t.client
		s, err := readWifi(ctx, c)
		if err != nil && !bridge {
			// The USB link went down with wifi_set: find the pod's new address on its console.
			if ip := consolePodIP(g); ip != "" {
				c = &tcpclient.Client{Addr: net.JoinHostPort(ip, wifiPodPort), DialTimeout: 2 * time.Second}
				s, err = readWifi(ctx, c)
			}
		}
		if err == nil {
			st = s
			if st.joined(ssid) {
				ip := podIP(ctx, c)
				if bridge {
					fmt.Fprintf(os.Stderr, "The Wi-Fi bridge joined %q (rssi %s), pod ip=%s\n", ssid, valueOrDash(st.rssi()), valueOrDash(ip))
				} else {
					fmt.Fprintf(os.Stderr, "Joined %q, ip=%s\n", ssid, valueOrDash(ip))
				}
				return ip, nil
			}
			if why := st.bridgeJoinFailure(); bridge && strings.TrimSpace(st.SSID) == ssid && why != "" {
				return "", fmt.Errorf("set-wifi: %s (the credentials are saved on the bridge; run set-wifi again to change them)", why)
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(wifiJoinPoll):
		}
	}
	fmt.Fprintf(os.Stderr, "Credentials saved; join not confirmed yet (state %s; run `benchpod show-network`).\n", valueOrDash(st.State))
	return "", nil
}

// wifiPodPort is the port set-wifi reaches the pod on at its new address (tests change it).
var wifiPodPort = tcpclient.PlainPort

// consolePodIP is the pod's network address from its USB console's status, "" when it has none
// (or no console answers). A variable so tests can replace it.
var consolePodIP = func(g *globalFlags) string {
	pods, _, err := serialconsole.ProbeSerial(g.serialDevice(), 2*time.Second)
	if err != nil {
		return ""
	}
	name := g.usbPodName()
	for _, p := range pods {
		if name != "" && !strings.Contains(p.Status, name) {
			continue
		}
		if p.Addressed() && !usbLinkNet.Contains(net.ParseIP(p.IP)) {
			return p.IP
		}
	}
	return ""
}

// wifiError words a failed Wi-Fi command; a pod without built-in Wi-Fi or a bridge says so itself.
func wifiError(what string, err error) error {
	if pe, ok := tcpclient.AsPodError(err); ok {
		return refusedError(what, pe)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// ensureEspBlob installs the blobs from the pod's firmware release when its ESP32-C3 slot is
// empty or holds a different image, so set-wifi works on a pod that has never had a network.
func ensureEspBlob(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, blobsInstallTimeout)
	defer cancel()
	lan, err := openLAN(ctx, addr)
	if err != nil {
		return fmt.Errorf("check the pod's ESP32-C3 image: %w (pass --skip-blobs to set Wi-Fi without the check)", err)
	}
	defer lan.s.Close()
	target := lanBlobs{lan: lan, addr: addr}
	slots, err := target.Blobs(ctx)
	missing, err := espBlobMissing(slots, err)
	if err != nil || !missing {
		return err
	}
	fmt.Fprintln(os.Stderr, "set-wifi: the pod has no ESP32-C3 image for Wi-Fi yet; installing it over USB first")
	return installBlobs(ctx, target, releaseBlobSource(target.Release(ctx)), false, nil)
}

// espBlobMissing reads the pod's blob listing for set-wifi: whether the ESP32-C3 image needs
// installing. Firmware without blob slots keeps the image built in, which is fine; any other
// error reading the slots is reported, with --skip-blobs as the way past it.
func espBlobMissing(slots []serialconsole.BlobSlot, err error) (bool, error) {
	if errors.Is(err, serialconsole.ErrNoBlobSlots) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check the pod's ESP32-C3 image: %w (pass --skip-blobs to set Wi-Fi without the check)", err)
	}
	for _, s := range slots {
		if s.Name == "esp" && s.NeedsInstall() {
			return true, nil
		}
	}
	return false, nil
}

// ── show-network ─────────────────────────────────────────────────────────────

// newShowNetworkCmd reports the pod's network state. It is named for what it reports rather than
// for one of the interfaces it can report on: the IP is whichever address the pod is actually
// using, and the pod prefers its wired Ethernet lease, so on a cabled pod this prints an address
// and no SSID at all.
func newShowNetworkCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show-network",
		Short: "Show the pod's network details: IP, plus stored SSID/state/RSSI (over the pod's USB network adapter)",
		Long: "Report the pod's network state (status and wifi_status, over the pod's USB\n" +
			"network adapter, or the --connection given).\n\n" +
			"The IP is whichever address the pod is actually using: it prefers the wired\n" +
			"Ethernet lease over Wi-Fi, so a pod on a cable reports an address here with no\n" +
			"SSID at all. The SSID/state/RSSI fields describe the optional Wi-Fi interface.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel, t, err := g.usbNetworkClient("show-network", 15*time.Second)
			if err != nil {
				return err
			}
			defer cancel()
			st, err := readWifi(ctx, t.client)
			if err != nil {
				return wifiError("show-network", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("show-network: open output: %w", err)
			}
			defer closeOut()
			printNetwork(out, podIP(ctx, t.client), st)
			return nil
		},
	}
}

func printNetwork(out io.Writer, ip string, st wifiState) {
	// IP first: it is the field that matters on any pod, and it is populated on a wired pod that
	// has no SSID at all.
	fmt.Fprintf(out, "IP:    %s\n", valueOrDash(ip))
	fmt.Fprintf(out, "SSID:  %s\n", valueOrDash(st.SSID))
	fmt.Fprintf(out, "State: %s\n", valueOrDash(st.State))
	fmt.Fprintf(out, "RSSI:  %s\n", valueOrDash(st.rssi()))
	if st.Via != "" {
		fmt.Fprintf(out, "Via:   %s\n", st.Via)
	}
	if p := st.problem(); p != "" {
		fmt.Fprintf(out, "Problem: %s\n", p)
	}
}

// ── scan-wifi ────────────────────────────────────────────────────────────────

// wifiScanPoll is how often scan-wifi reads a running scan.
var wifiScanPoll = 500 * time.Millisecond

type wifiNetwork struct {
	SSID    string `json:"ssid"`
	RSSI    int    `json:"rssi"`
	Channel int    `json:"channel"`
	Auth    string `json:"auth"`
}

// newScanWifiCmd lists the networks the pod's Wi-Fi sees, to pick the SSID for set-wifi.
func newScanWifiCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "scan-wifi",
		Short: "List the Wi-Fi networks the pod sees (over the pod's USB network adapter)",
		Long: "List the Wi-Fi networks the pod sees, strongest first, to pick the SSID for set-wifi\n" +
			"(wifi_scan, over the pod's USB network adapter). The pod scans with its built-in\n" +
			"Wi-Fi, or with a Wi-Fi bridge on its Ethernet port when one is plugged in. A scan\n" +
			"takes a few seconds; a joined pod stays joined.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// 45 s: a pod that is joining waits for the join to finish (up to 30 s) before it scans.
			ctx, cancel, t, err := g.usbNetworkClient("scan-wifi", 45*time.Second)
			if err != nil {
				return err
			}
			defer cancel()
			nets, truncated, err := scanWifi(ctx, t.client)
			if err != nil {
				return wifiError("scan-wifi", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("scan-wifi: open output: %w", err)
			}
			defer closeOut()
			fmt.Fprintf(out, "%-6s %-4s %-10s %s\n", "RSSI", "CH", "SECURITY", "SSID")
			for _, n := range nets {
				ssid := n.SSID
				if ssid == "" {
					ssid = "(hidden)"
				}
				fmt.Fprintf(out, "%-6d %-4d %-10s %s\n", n.RSSI, n.Channel, n.Auth, ssid)
			}
			msg := fmt.Sprintf("%d networks", len(nets))
			if truncated {
				msg += " (weaker ones left out)"
			}
			fmt.Fprintln(os.Stderr, msg)
			return nil
		},
	}
}

// scanWifi starts a scan and reads it until it is done.
func scanWifi(ctx context.Context, c podCommander) ([]wifiNetwork, bool, error) {
	var rep struct {
		State     string        `json:"state"`
		ID        int           `json:"id"`
		Networks  []wifiNetwork `json:"networks"`
		Truncated bool          `json:"truncated"`
	}
	req := map[string]any{"cmd": "wifi_scan"}
	for {
		raw, err := c.Command(ctx, req)
		if err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(raw, &rep); err != nil {
			return nil, false, fmt.Errorf("unexpected reply %s", strings.TrimSpace(string(raw)))
		}
		if rep.State == "done" {
			return rep.Networks, rep.Truncated, nil
		}
		req = map[string]any{"cmd": "wifi_scan", "id": rep.ID}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(wifiScanPoll):
		}
	}
}

// ── clear-wifi ───────────────────────────────────────────────────────────────

// clearWifiTimeout covers the reply; the pod erases the ESP32-C3's NVS after it.
const clearWifiTimeout = 60 * time.Second

func newClearWifiCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "clear-wifi",
		Short: "Erase the stored Wi-Fi credentials (Wi-Fi goes down at once)",
		Long: "Erase the pod's stored Wi-Fi credentials (wifi_clear): Wi-Fi goes down and USB\n" +
			"networking comes back on. The CLI sends it over the pod's network: its USB network\n" +
			"adapter, else (USB networking is off while Wi-Fi is set) its network address, or\n" +
			"the --connection given. When no network reaches the pod, `benchpod net-reset`\n" +
			"does it over the USB console.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel, t, err := g.usbNetworkClient("clear-wifi", clearWifiTimeout)
			if err != nil {
				return fmt.Errorf("%w (without a network, `benchpod net-reset` clears the Wi-Fi over the USB console)", err)
			}
			defer cancel()
			if _, err := t.client.Command(ctx, map[string]any{"cmd": "wifi_clear"}); err != nil {
				return wifiError("clear-wifi", err)
			}
			fmt.Fprintln(os.Stderr, "Erased the stored Wi-Fi credentials. Wi-Fi is off; no reboot needed. USB networking is on again.")
			return nil
		},
	}
}

// ── net-reset ────────────────────────────────────────────────────────────────

func newNetResetCmd(g *globalFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "net-reset",
		Short: "Forget the Wi-Fi and reset Ethernet over the USB console, when no network reaches the pod",
		Long: "Run the pod's console `net-reset` (firmware 4.0): the pod forgets its Wi-Fi\n" +
			"credentials (its own flash and the ESP32-C3's, or the Wi-Fi bridge's), which turns\n" +
			"USB networking back on, and puts Ethernet back to autonegotiation with a PHY reset\n" +
			"and a new DHCP lease. The policies, the cloud settings and the device key stay.\n" +
			"It is the way back to a pod that no network reaches; with a network, clear-wifi\n" +
			"does the Wi-Fi part.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !yes && !confirmYes("This forgets the pod's Wi-Fi and resets its Ethernet link. Type 'yes' to continue: ") {
				return errors.New("aborted")
			}
			device, err := g.usbDevice("net-reset")
			if err != nil {
				return err
			}
			console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(clearWifiTimeout))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()
			lines, err := console.NetReset(ctx)
			for _, ln := range lines {
				fmt.Fprintf(os.Stderr, "  %s\n", ln)
			}
			if err != nil {
				return fmt.Errorf("net-reset: %w", err)
			}
			fmt.Fprintln(os.Stderr, "Wi-Fi forgotten and Ethernet reset; USB networking is on again.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}
