package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── deregister (detach the pod from the account, keeping its data) ──────────

func newDeregisterCmd(g *globalFlags) *cobra.Command {
	var serverURL, tokenFile, deviceName, deviceID string
	var keepPodConfig bool
	cmd := &cobra.Command{
		Use:   "deregister",
		Short: "Deregister the bench pod from the server (keeps its data) and stop it connecting",
		Long: "Deregister the bench pod from the logged-in user's account.\n\n" +
			"The server keeps everything recorded for the pod — captures, waveforms, wiring —\n" +
			"and only marks the device disabled, so it disappears from the web UI and its\n" +
			"identity is freed for another account. Registering the SAME pod again for the\n" +
			"same account revives the device with its data; registering it for a different\n" +
			"account gives that account a fresh device and leaves this one's history alone.\n\n" +
			"By default the pod is identified by asking the attached bench pod for its public\n" +
			"key (so --connection must point at it) and is then told to stop connecting to the\n" +
			"cloud. Use --device-name/--device-id (or --connection embeddedci:<name>) to\n" +
			"deregister a pod you cannot reach.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDeregister(g, serverURL, tokenFile, deviceName, deviceID, keepPodConfig)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server-url", "https://www.embeddedci.com", "embeddedci-server base URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "deregister the device with this name instead of the attached bench pod")
	cmd.Flags().StringVar(&deviceID, "device-id", "", "deregister the device with this id instead of the attached bench pod")
	cmd.Flags().BoolVar(&keepPodConfig, "keep-pod-config", false,
		"leave the pod's cloud configuration in place (it will keep trying to connect and be refused)")
	return cmd
}

// runDeregister detaches a bench pod from the logged-in user's account. It is the inverse of
// runRegister and undoes both halves of it: the server-side device record (disabled, not deleted —
// the data survives for a later re-register) and the pod-side cloud provisioning written by
// `cloud_set` (so the firmware stops opening the control WebSocket on every boot).
//
// Device selection mirrors register: by default the attached pod is asked for its public key and
// the matching device is looked up, which is the only way to be sure the right physical pod is
// detached. --device-name / --device-id cover the pod that is broken, gone, or already wiped; in
// that case there is nothing to unprovision locally, so the pod-side step is skipped.
func runDeregister(g *globalFlags, serverURL, tokenFile, deviceName, deviceID string, keepPodConfig bool) error {
	if strings.TrimSpace(serverURL) == "" {
		return errors.New("--server-url cannot be empty")
	}
	deviceName = strings.TrimSpace(deviceName)
	deviceID = strings.TrimSpace(deviceID)
	if deviceName != "" && deviceID != "" {
		return errors.New("pass only one of --device-name or --device-id")
	}
	// Only the default (identify-by-key) path talks to the pod; an explicit selector means the
	// pod may be unreachable, which is exactly why the selector exists.
	usePod := deviceName == "" && deviceID == ""

	var client *tcpclient.Client
	if usePod {
		spec, err := g.resolveTarget()
		if err != nil {
			return err
		}
		if spec.IsCloud() {
			// --connection embeddedci:<name> is --device-name <name>.
			deviceName, usePod = spec.Name, false
		} else {
			if err := spec.RequireWifi("deregister"); err != nil {
				return err
			}
			client = &tcpclient.Client{Addr: spec.Addr}
		}
	}

	tokenPath, err := resolveTokenPath(tokenFile)
	if err != nil {
		return fmt.Errorf("resolve token path: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	api := serverapi.New(serverURL)
	tokens, err := ensureTokens(ctx, api, tokenPath)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	log.Printf("auth: signed in as user %s", tokens.UserID)

	target := deviceID
	label := deviceID
	if target == "" {
		var pubKey string
		if usePod {
			idCtx, idCancel := context.WithTimeout(ctx, 30*time.Second)
			pubKey, err = client.IdentityPublic(idCtx)
			idCancel()
			if err != nil {
				return fmt.Errorf("fetch device public key: %w", err)
			}
		}
		listCtx, listCancel := context.WithTimeout(ctx, 30*time.Second)
		devices, lErr := api.ListDevices(listCtx, tokens.AccessToken)
		listCancel()
		if lErr != nil {
			return fmt.Errorf("list devices: %w", lErr)
		}
		dev, fErr := findDeviceToDeregister(devices, pubKey, deviceName)
		if fErr != nil {
			return fErr
		}
		target, label = dev.ID, dev.Name
	}

	degCtx, degCancel := context.WithTimeout(ctx, 30*time.Second)
	device, err := api.DeregisterDevice(degCtx, tokens.AccessToken, target)
	degCancel()
	if err != nil {
		return fmt.Errorf("deregister device: %w", err)
	}
	if strings.TrimSpace(device.Name) != "" {
		label = device.Name
	}
	log.Printf("device: deregistered name=%s id=%s (data retained)", label, device.ID)

	// Wipe the pod-side cloud provisioning so the firmware stops reconnecting. `cloud_clear`
	// (not `cloud_set`) is the right tool: it drops the stored endpoint AND the now-dead
	// device_id, leaving the pod ready for a fresh `benchpod register` — for this or any other
	// account. Best-effort: the server-side deregistration already stands and the server now
	// refuses this device, so a failure here is a warning, not a reason to fail the command.
	if usePod && !keepPodConfig {
		setCtx, setCancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = client.Command(setCtx, map[string]any{"cmd": "cloud_clear"})
		setCancel()
		if err != nil {
			log.Printf("warning: could not clear the bench pod's cloud configuration: %v", err)
			log.Printf("the pod will keep trying to connect and be refused; re-run with the pod reachable, or clear it by hand")
		} else {
			log.Printf("provisioned: bench pod cloud configuration cleared")
		}
	}

	fmt.Fprintf(os.Stderr, "Deregistered bench pod %q (device %s). Its data is kept; register it again to restore it.\n",
		label, device.ID)
	return nil
}

// findDeviceToDeregister picks the device to detach out of the account's device list, by public
// key when the pod could be asked for one (the identity that survives renames and re-addressing)
// and by name otherwise. It refuses to guess: an unmatched selector is an error, so a mistyped
// name can never deregister some other pod.
func findDeviceToDeregister(devices []serverapi.DeviceResponse, publicKey, name string) (serverapi.DeviceResponse, error) {
	if publicKey != "" {
		for _, d := range devices {
			if strings.TrimSpace(d.PublicKey) == publicKey {
				return d, nil
			}
		}
		return serverapi.DeviceResponse{}, errors.New(
			"the attached bench pod is not registered to this account (no device matches its public key)")
	}
	for _, d := range devices {
		if d.Name == name {
			return d, nil
		}
	}
	return serverapi.DeviceResponse{}, fmt.Errorf("no device named %q is registered to this account", name)
}
