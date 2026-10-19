package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/spf13/cobra"
)

// The commands in this file always use the firmware's USB CDC-ACM serial
// console — you cannot provision WiFi or trigger the bootloader over the
// network. They therefore ignore the wifi/serial choice of --connection and
// only honor an explicit device path from it (g.serialDevice()), falling
// back to auto-detection (USB VID 2E8A) otherwise.

// ── set-wifi ─────────────────────────────────────────────────────────────────

func newSetWifiCmd(g *globalFlags) *cobra.Command {
	var ssid, password string
	var passwordStdin, skipBlobs bool
	cmd := &cobra.Command{
		Use:   "set-wifi",
		Short: "Save WiFi credentials and join (over the pod's USB console)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(ssid) == "" {
				return errors.New("--ssid is required")
			}
			pw, err := resolveWifiPassword(password, passwordStdin)
			if err != nil {
				return fmt.Errorf("set-wifi: %w", err)
			}
			_, err = runSetWifi(g, ssid, pw, skipBlobs)
			return err
		},
	}
	cmd.Flags().StringVar(&ssid, "ssid", "", "WiFi SSID (required)")
	cmd.Flags().StringVar(&password, "password", "", "WiFi password (insecure: visible in shell history); omit to be prompted")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the WiFi password from the first line of stdin")
	cmd.Flags().BoolVar(&skipBlobs, "skip-blobs", false, "do not install a missing ESP32-C3 image first (not needed through a Wi-Fi bridge)")
	return cmd
}

// runSetWifi saves Wi-Fi credentials over the USB console and joins, returning the pod's
// address when the join is confirmed ("" when it is not). Shared by set-wifi and setup.
func runSetWifi(g *globalFlags, ssid, pw string, skipBlobs bool) (string, error) {
	// wifi-set runs the full reconnect ladder on the device: probe the
	// internal modem (with an EN-reset retry), then the external GPIO40/41
	// modem, then AT+CWJAP (up to ~15s) and TCP bring-up. That can take
	// ~40s, so wait generously — the command returns as soon as the prompt
	// reappears, this is only the upper bound. Override with --timeout.
	device, err := g.usbDevice("set-wifi")
	if err != nil {
		return "", err
	}
	console, path, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(90*time.Second))
	if err != nil {
		return "", err
	}
	defer cancel()
	defer console.Close()

	// A Wi-Fi bridge on the pod's Ethernet port carries Wi-Fi instead of the ESP32-C3: it keeps
	// the credentials itself and joins in the background.
	before, _ := console.WifiShow(ctx)
	if before.ViaBridge() {
		return setWifiThroughBridge(ctx, console, path, ssid, pw)
	}

	// Wi-Fi runs on the ESP32-C3, whose image the pod keeps in its W25Q: a pod without
	// it (fresh board, no network) can only get it over this cable.
	if !skipBlobs {
		if err := ensureEspBlob(console); err != nil {
			return "", fmt.Errorf("set-wifi: %w", err)
		}
	}

	res, err := console.WifiSet(ctx, ssid, pw)
	// Echo the firmware's WiFi/TCP/ESP32 bring-up lines (incl. the
	// "[wifi] connected  ip=<ip>" result) so the IP is visible — even
	// when the join didn't confirm.
	for _, line := range serialconsole.BringupLines(res.Raw) {
		fmt.Fprintf(os.Stderr, "  %s\n", line)
	}
	if err != nil {
		return "", fmt.Errorf("set-wifi: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Wrote WiFi credentials for SSID %q via %s.\n", ssid, path)

	// Read the authoritative state back with wifi-show: the reconnect's
	// "[wifi] connected ip=" line can be lost in the device's non-blocking
	// USB console output, so don't rely on it alone to report the result.
	st, _ := console.WifiShow(ctx)
	ip := st.IP
	if ip == "" {
		ip = res.IP
	}
	state := strings.ToLower(st.State)
	joined := res.Joined || ip != "" && ip != "(none)" ||
		strings.Contains(state, "ready") || strings.Contains(state, "connected")
	if !joined {
		fmt.Fprintln(os.Stderr, "Credentials saved; join not confirmed (run `benchpod show-network`).")
		return "", nil
	}
	fmt.Fprintf(os.Stderr, "Joined %q, ip=%s\n", ssid, valueOrDash(ip))
	if ip == "(none)" || ip == "0.0.0.0" {
		ip = ""
	}
	return ip, nil
}

// bridgeJoinWait bounds how long set-wifi watches a Wi-Fi bridge join: it reports its state
// every few seconds and a WPA2/WPA3 join takes a few seconds more.
var bridgeJoinWait = 45 * time.Second

// setWifiThroughBridge saves the credentials on the pod's Wi-Fi bridge (the pod seals the
// password for it) and waits for the bridge to join or fail. The pod's address does not change:
// the bridge carries its Ethernet traffic, so the IP is the one it already has.
func setWifiThroughBridge(ctx context.Context, console *serialconsole.Console, path, ssid, pw string) (string, error) {
	if _, err := console.WifiSet(ctx, ssid, pw); err != nil {
		return "", fmt.Errorf("set-wifi: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Saved Wi-Fi credentials for SSID %q on the pod's Wi-Fi bridge via %s; waiting for it to join...\n", ssid, path)
	deadline := time.Now().Add(bridgeJoinWait)
	var st serialconsole.WifiStatus
	for {
		st, _ = console.WifiShow(ctx)
		if st.BridgeJoined() {
			ip := st.IP
			if ip == "(none)" || ip == "0.0.0.0" {
				ip = ""
			}
			fmt.Fprintf(os.Stderr, "The Wi-Fi bridge joined %q (rssi %s), pod ip=%s\n", ssid, valueOrDash(st.RSSI), valueOrDash(ip))
			return ip, nil
		}
		if why := st.BridgeJoinFailure(); why != "" {
			return "", fmt.Errorf("set-wifi: %s (the credentials are saved on the bridge; run set-wifi again to change them)", why)
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	fmt.Fprintf(os.Stderr, "Credentials saved; the Wi-Fi bridge has not joined yet (state %s; run `benchpod show-network`).\n", valueOrDash(st.State))
	return "", nil
}

// ensureEspBlob installs the blobs from the pod's firmware release when its ESP32-C3 slot is
// empty or holds a different image, so set-wifi works on a pod that has never had a network.
func ensureEspBlob(console *serialconsole.Console) error {
	ctx, cancel := context.WithTimeout(context.Background(), blobsInstallTimeout)
	defer cancel()
	// Before Wi-Fi is set the pod's USB network is still on (firmware 3.9.0+): use it when up.
	target, closeTarget, err := blobTargetFor(ctx, console, "the USB console")
	if err != nil {
		return err
	}
	defer closeTarget()
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

// newShowNetworkCmd reports the pod's network state. It is named for what it
// reports rather than for one of the interfaces it can report on: the IP is
// whichever address the pod is actually using, and the pod prefers its wired
// Ethernet lease, so on a cabled pod this prints an address and no SSID at all.
// (It was called show-wifi, which described only the minority case.)
func newShowNetworkCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show-network",
		Short: "Show the pod's network details: IP, plus stored SSID/state/RSSI (over the pod's USB console)",
		Long: "Report the pod's network state over the pod's USB console.\n\n" +
			"The IP is whichever address the pod is actually using: it prefers the wired\n" +
			"Ethernet lease over Wi-Fi, so a pod on a cable reports an address here with no\n" +
			"SSID at all. The SSID/state/RSSI fields describe the optional Wi-Fi interface.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			device, err := g.usbDevice("show-network")
			if err != nil {
				return err
			}
			console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(15*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			st, err := console.WifiShow(ctx)
			if err != nil {
				return fmt.Errorf("show-network: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("show-network: open output: %w", err)
			}
			defer closeOut()
			// IP first: it is the field that matters on any pod, and it is
			// populated on a wired pod that has no SSID at all.
			fmt.Fprintf(out, "IP:    %s\n", valueOrDash(st.IP))
			fmt.Fprintf(out, "SSID:  %s\n", valueOrDash(st.SSID))
			fmt.Fprintf(out, "State: %s\n", valueOrDash(st.State))
			fmt.Fprintf(out, "RSSI:  %s\n", valueOrDash(st.RSSI))
			if st.Via != "" {
				fmt.Fprintf(out, "Via:   %s\n", st.Via)
			}
			if st.Problem != "" {
				fmt.Fprintf(out, "Problem: %s\n", st.Problem)
			}
			return nil
		},
	}
}

// ── scan-wifi ────────────────────────────────────────────────────────────────

// newScanWifiCmd lists the networks the pod's Wi-Fi sees, to pick the SSID for set-wifi.
func newScanWifiCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "scan-wifi",
		Short: "List the Wi-Fi networks the pod sees (over the pod's USB console)",
		Long: "List the Wi-Fi networks the pod sees, strongest first, to pick the SSID for set-wifi.\n" +
			"The pod scans with its built-in Wi-Fi (firmware 3.9.4 or later), or with a Wi-Fi bridge on\n" +
			"its Ethernet port when one is plugged in. A scan takes a few seconds; a joined pod stays\n" +
			"joined. On older firmware the command says so and how to join a network anyway (exit 0).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			device, err := g.usbDevice("scan-wifi")
			if err != nil {
				return err
			}
			// 45 s: a pod that is joining waits for the join to finish (up to 30 s) before it scans.
			console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(45*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			nets, err := console.WifiScan(ctx)
			if errors.Is(err, serialconsole.ErrNoWifiScan) {
				return scanWifiUnsupported(ctx, console)
			}
			if err != nil {
				return fmt.Errorf("scan-wifi: %w", err)
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
			fmt.Fprintf(os.Stderr, "%d networks\n", len(nets))
			return nil
		},
	}
}

// scanWifiUnsupported answers scan-wifi on firmware that cannot scan: not an error when the pod
// has built-in Wi-Fi (it can still join a network), an error when it has no Wi-Fi at all.
func scanWifiUnsupported(ctx context.Context, console *serialconsole.Console) error {
	st, err := console.WifiShow(ctx)
	if err != nil || st.State == "" {
		return errors.New("scan-wifi: this pod has no Wi-Fi")
	}
	fmt.Println("This pod has built-in Wi-Fi, but its firmware cannot scan for networks:" +
		" scanning needs firmware 3.9.4 or later (update with `benchpod flash-self`).")
	fmt.Println("To join a network now: benchpod set-wifi --ssid <network name>")
	return nil
}

// ── clear-wifi ───────────────────────────────────────────────────────────────

// clearWifiTimeout covers the C3 NVS erase that wifi-clear runs before it answers. 10 s was too
// short: the CLI gave up on pods that were still erasing (2026-10-15).
const clearWifiTimeout = 60 * time.Second

func newClearWifiCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "clear-wifi",
		Short: "Erase the stored Wi-Fi credentials (Wi-Fi goes down at once)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			device, err := g.usbDevice("clear-wifi")
			if err != nil {
				return err
			}
			// The pod answers once it has also erased the ESP32-C3's NVS over the C3's ROM
			// loader: about 3 s, and up to half a minute when the C3 is slow to answer.
			console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(clearWifiTimeout))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			if err := console.WifiClear(ctx); err != nil {
				return fmt.Errorf("clear-wifi: %w", err)
			}
			fmt.Fprintln(os.Stderr, "Erased the stored Wi-Fi credentials. Wi-Fi is off; no reboot needed. "+
				"USB networking is on again (firmware 3.9.0+).")
			return nil
		},
	}
}

// ── bootsel ──────────────────────────────────────────────────────────────────

func newBootselCmd(g *globalFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "bootsel",
		Short: "Reboot into the UF2 bootloader to flash firmware",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !yes {
				fmt.Fprint(os.Stderr, "This reboots the pod into the UF2 bootloader; the USB console will disconnect. Type 'yes' to continue: ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.TrimSpace(line) != "yes" {
					return errors.New("aborted")
				}
			}
			device, err := g.usbDevice("bootsel")
			if err != nil {
				return err
			}
			console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(10*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			if err := console.Bootsel(ctx); err != nil {
				return fmt.Errorf("bootsel: %w", err)
			}
			fmt.Fprintln(os.Stderr, "Pod entering BOOTSEL (UF2 drive). USB console disconnected; this is expected.")
			fmt.Fprintln(os.Stderr, "Drop firmware.uf2 onto the RP2350 drive (or use picotool) to flash.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}

// ── dfu ──────────────────────────────────────────────────────────────────────

// newDfuCmd reboots an STM32 bench pod into its ROM USB DFU bootloader (the
// STM32 counterpart of `bootsel`). It only triggers the reboot; write the new
// firmware afterwards with `flash-self` (or dfu-util directly). For a blank
// board with no firmware yet, use the hardware BOOT0 pin instead.
func newDfuCmd(g *globalFlags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "dfu",
		Short: "Reboot an STM32 pod into the USB DFU bootloader to flash firmware",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !yes {
				fmt.Fprint(os.Stderr, "This reboots the device into the USB DFU bootloader; the USB console will disconnect. Type 'yes' to continue: ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.TrimSpace(line) != "yes" {
					return errors.New("aborted")
				}
			}
			device, err := g.usbDevice("dfu")
			if err != nil {
				return err
			}
			console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(10*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			if err := console.Dfu(ctx); err != nil {
				return fmt.Errorf("dfu: %w", err)
			}
			fmt.Fprintln(os.Stderr, "Device entering DFU (USB bootloader). USB console disconnected; this is expected.")
			fmt.Fprintln(os.Stderr, "Write firmware with `benchpod flash-self <firmware.bin>` (or dfu-util) to flash.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}
