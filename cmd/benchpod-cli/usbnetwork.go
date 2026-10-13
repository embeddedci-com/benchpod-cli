package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// The pod's USB network setting (firmware 3.9.0+): whether its USB port is also a network
// adapter. Only the device changes it: the USB console, or the USB link itself (which can turn it
// off, not back on: the link is gone then). Ethernet, Wi-Fi and the cloud may read it.

const usbNetworkMissing = "this firmware has no USB network setting; update the pod's firmware (3.9.0 or later)"

func newUSBNetworkCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "usb-network [show|on|off]",
		Short: "Show, or on the device turn on or off, the pod's USB network adapter",
		Long: "Show the pod's USB network setting, or turn the adapter on or off (firmware 3.9.0+).\n\n" +
			"Off, the pod re-enumerates as its USB console only: the computer sees no network\n" +
			"adapter. The setting is stored, and is separate from lan-policy (which covers Ethernet\n" +
			"and Wi-Fi). Only the device changes it: `off` over --connection usb (the USB link or the\n" +
			"console), `on` over the USB console. Over Ethernet, Wi-Fi or embeddedci.com it can only\n" +
			"be read.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"show", "on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			set := ""
			if len(args) == 1 {
				switch args[0] {
				case "show":
				case "on", "off":
					set = args[0]
				default:
					return fmt.Errorf("usb-network: use show, on or off (got %q)", args[0])
				}
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			return runUSBNetwork(g, set, out)
		},
	}
}

func runUSBNetwork(g *globalFlags, set string, out io.Writer) error {
	what := "usb-network"
	if set != "" {
		what += " " + set
	}
	spec, err := g.resolveTarget()
	if err != nil {
		return err
	}
	if !spec.IsSerial() {
		if set != "" {
			return fmt.Errorf("%s: only the device changes it: run it over USB (--connection usb); "+
				"Ethernet, Wi-Fi and embeddedci.com may only read the setting", what)
		}
		ctx, cancel, t, err := g.podClient(what, 15*time.Second)
		if err != nil {
			return err
		}
		defer cancel()
		setting, err := usbNetworkJSON(ctx, t.client, "")
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		printUSBNetwork(out, t.label, setting, "")
		return nil
	}
	// Reading and turning off work over the USB link itself when it is up (no console needed);
	// turning on needs the console (the link does not exist while the setting is off).
	if set != "on" && spec.Device == "" {
		if addr := localUSBPod(spec.Pod); addr != "" {
			ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(15*time.Second))
			defer cancel()
			setting, err := usbNetworkJSON(ctx, &tcpclient.Client{Addr: addr}, set)
			if err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			printUSBNetwork(out, addr, setting, set)
			return nil
		}
	}
	console, path, ctx, cancel, err := g.openSerialConsole(spec.Device, g.effectiveTimeout(15*time.Second))
	if err != nil {
		return err
	}
	defer cancel()
	defer console.Close()
	rep, err := console.Policy(ctx, "usb-network", set)
	if err != nil {
		if errors.Is(err, serialconsole.ErrPolicyUnsupported) {
			return errors.New(usbNetworkMissing)
		}
		if pe, ok := tcpclient.AsPodError(err); ok {
			return refusedError(what, pe, false)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	printUSBNetwork(out, path, rep.Policy, set)
	return nil
}

// usbNetworkJSON reads, or with set changes, the setting over the JSON API.
func usbNetworkJSON(ctx context.Context, c podCommander, set string) (string, error) {
	req := map[string]any{"cmd": "usb_network"}
	if set != "" {
		req["set"] = set
	}
	data, err := c.Command(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "unknown cmd") {
			return "", errors.New(usbNetworkMissing)
		}
		return "", err
	}
	var rep struct {
		Setting string `json:"setting"`
	}
	if err := json.Unmarshal(data, &rep); err != nil || rep.Setting == "" {
		return "", fmt.Errorf("unexpected reply %s", strings.TrimSpace(string(data)))
	}
	return rep.Setting, nil
}

func printUSBNetwork(out io.Writer, via, setting, set string) {
	switch {
	case set == "":
		fmt.Fprintf(out, "USB network of the pod at %s: %s\n", via, setting)
	case setting == "off":
		fmt.Fprintf(out, "USB network of the pod at %s is now off: it re-enumerates as its USB console only "+
			"(no network adapter). Turn it back on with `benchpod usb-network on` over the USB console.\n", via)
	default:
		fmt.Fprintf(out, "USB network of the pod at %s is now on: the adapter comes up in a few seconds.\n", via)
	}
}
