package main

import (
	"bufio"
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
	var passwordStdin bool
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
			// wifi-set runs the full reconnect ladder on the device: probe the
			// internal modem (with an EN-reset retry), then the external GPIO40/41
			// modem, then AT+CWJAP (up to ~15s) and TCP bring-up. That can take
			// ~40s, so wait generously — the command returns as soon as the prompt
			// reappears, this is only the upper bound. Override with --timeout.
			console, path, ctx, cancel, err := g.openSerialConsole(g.serialDevice(), g.effectiveTimeout(90*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			res, err := console.WifiSet(ctx, ssid, pw)
			// Echo the firmware's WiFi/TCP/ESP32 bring-up lines (incl. the
			// "[wifi] connected  ip=<ip>" result) so the IP is visible — even
			// when the join didn't confirm.
			for _, line := range serialconsole.BringupLines(res.Raw) {
				fmt.Fprintf(os.Stderr, "  %s\n", line)
			}
			if err != nil {
				return fmt.Errorf("set-wifi: %w", err)
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
			if joined {
				fmt.Fprintf(os.Stderr, "Joined %q, ip=%s\n", ssid, valueOrDash(ip))
			} else {
				fmt.Fprintln(os.Stderr, "Credentials saved; join not confirmed (run `benchpod show-network`).")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ssid, "ssid", "", "WiFi SSID (required)")
	cmd.Flags().StringVar(&password, "password", "", "WiFi password (insecure: visible in shell history); omit to be prompted")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the WiFi password from the first line of stdin")
	return cmd
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
			console, _, ctx, cancel, err := g.openSerialConsole(g.serialDevice(), g.effectiveTimeout(15*time.Second))
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
			return nil
		},
	}
}

// ── clear-wifi ───────────────────────────────────────────────────────────────

func newClearWifiCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "clear-wifi",
		Short: "Erase stored WiFi credentials (reboot to fully apply)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			console, _, ctx, cancel, err := g.openSerialConsole(g.serialDevice(), g.effectiveTimeout(10*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			if err := console.WifiClear(ctx); err != nil {
				return fmt.Errorf("clear-wifi: %w", err)
			}
			fmt.Fprintln(os.Stderr, "Erased stored WiFi credentials. Reboot the device to fully apply (`benchpod bootsel` or power-cycle).")
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
				fmt.Fprint(os.Stderr, "This reboots the device into the UF2 bootloader; the USB console will disconnect. Type 'yes' to continue: ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.TrimSpace(line) != "yes" {
					return errors.New("aborted")
				}
			}
			console, _, ctx, cancel, err := g.openSerialConsole(g.serialDevice(), g.effectiveTimeout(10*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			if err := console.Bootsel(ctx); err != nil {
				return fmt.Errorf("bootsel: %w", err)
			}
			fmt.Fprintln(os.Stderr, "Device entering BOOTSEL (UF2 drive). USB console disconnected — this is expected.")
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
			console, _, ctx, cancel, err := g.openSerialConsole(g.serialDevice(), g.effectiveTimeout(10*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()

			if err := console.Dfu(ctx); err != nil {
				return fmt.Errorf("dfu: %w", err)
			}
			fmt.Fprintln(os.Stderr, "Device entering DFU (USB bootloader). USB console disconnected — this is expected.")
			fmt.Fprintln(os.Stderr, "Write firmware with `benchpod flash-self <firmware.bin>` (or dfu-util) to flash.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	return cmd
}
