package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The commands in this file always use the firmware's USB serial console: the
// bootloader cannot be reached over the network. They ignore a network choice
// of --connection and only honor an explicit device path from it
// (g.serialDevice()), falling back to auto-detection otherwise.

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

// confirmYes asks on stderr and reads stdin; only "yes" goes ahead.
func confirmYes(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line) == "yes"
}
