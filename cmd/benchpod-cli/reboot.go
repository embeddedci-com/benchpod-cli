package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// reboot restarts the pod: {"cmd":"reboot","mode":"soft"|"hard"}. The pod answers {"reboot":"soft"}
// and restarts about 400 ms later, so the connection drops right after the command. Either the
// reply or that drop is success. A hard reboot (power-cycle) is refused with BP0093 on a pod that
// cannot do it, which is every pod today; the catalog's fix.cli words the refusal.

// Seams for tests: how long to wait before asking whether the pod is back (the old firmware
// must not answer for the new one), how long it gets to come back, and how often it is asked.
var (
	rebootSettle = 3 * time.Second
	rebootWait   = 90 * time.Second
	rebootRetry  = 2 * time.Second
)

func newRebootCmd(g *globalFlags) *cobra.Command {
	var hard, soft, noWait bool
	cmd := &cobra.Command{
		Use:   "reboot",
		Short: "Restart the pod (soft: the processor; hard: power-cycle)",
		Long: "Restart the pod and, by default, wait until it answers again.\n\n" +
			"A soft reboot (the default) restarts the pod's processor. It stops the DAC and control\n" +
			"loop and switches the DUT supplies off first, and the pod keeps its settings.\n" +
			"A hard reboot (--hard) switches the pod's own power off and on. No pod can do that yet:\n" +
			"the pod refuses with BP0093, and you unplug its power instead.\n\n" +
			"Works over Ethernet, Wi-Fi, --connection usb, usb:<name> and embeddedci:<name>. Waiting\n" +
			"looks the pod up again, so a USB pod is found after its adapter comes back. With\n" +
			"--no-wait the command returns as soon as the pod has taken the reboot.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hard && soft {
				return usageErrorf("reboot: use --soft or --hard, not both")
			}
			mode := "soft"
			if hard {
				mode = "hard"
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			return runReboot(g, mode, !noWait, out)
		},
	}
	cmd.Flags().BoolVar(&soft, "soft", false, "restart the processor (the default)")
	cmd.Flags().BoolVar(&hard, "hard", false, "power-cycle the pod (refused with BP0093 on a pod that cannot)")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for the pod to come back")
	return cmd
}

func runReboot(g *globalFlags, mode string, wait bool, out io.Writer) error {
	ctx, cancel, t, err := g.podClient("reboot", 15*time.Second+rebootSettle+rebootWait)
	if err != nil {
		return err
	}
	defer cancel()
	where := t.where()

	sendCtx, sendCancel := context.WithTimeout(ctx, 15*time.Second)
	_, err = t.client.Command(sendCtx, map[string]any{"cmd": "reboot", "mode": mode})
	sendCancel()
	if err != nil && !rebootSent(err, ctx) {
		if isUnknownCommand(err) {
			return fmt.Errorf("reboot: this pod's firmware has no reboot command; update it (`benchpod flash-self`): %w", err)
		}
		return fmt.Errorf("reboot: %w", err)
	}
	if !wait {
		fmt.Fprintf(out, "Reboot (%s) sent to the pod at %s.\n", mode, where)
		return nil
	}
	version, err := waitForReboot(ctx, g, t)
	if err != nil {
		return fmt.Errorf("reboot: the pod took the reboot but did not answer again within %s (%v). "+
			"Check `benchpod discover`; if it does not show up, unplug its power and plug it back in", rebootWait, err)
	}
	fmt.Fprintf(out, "Pod at %s rebooted (%s), firmware %s.\n", where, mode, valueOr(version, "unknown"))
	return nil
}

// rebootSent says err, from the reboot command, still means the pod took it: the connection
// dropped as the pod restarted (a closed or reset read), or embeddedci.com timed out waiting for
// the reply of a pod that was already gone. A refusal, an unreachable pod and the caller's own
// cancel are not.
func rebootSent(err error, parent context.Context) bool {
	if parent.Err() != nil || errors.Is(err, tcpclient.ErrUnreachable) {
		return false
	}
	if _, ok := tcpclient.AsPodError(err); ok {
		return false
	}
	var ce *cloudpod.Error
	if errors.As(err, &ce) {
		return ce.Code == "device_timeout" || ce.Status == http.StatusGatewayTimeout
	}
	return !errors.Is(err, context.Canceled)
}

// waitForReboot waits for the restarted pod to answer status and returns its firmware version.
// Over USB the pod's address is looked up again each try, as its adapter comes back after the
// restart; over embeddedci.com the same client is asked until the pod is online again.
func waitForReboot(ctx context.Context, g *globalFlags, t *podTarget) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(rebootSettle):
	}
	deadline := time.Now().Add(rebootWait)
	var lastErr error
	for {
		version, err := rebootStatus(ctx, g, t)
		if err == nil {
			return version, nil
		}
		if _, refused := tcpclient.AsPodError(err); refused && !errors.Is(err, tcpclient.ErrUnreachable) {
			// An answer that is a refusal (busy while it boots) means it is up.
			return "", nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", lastErr
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(rebootRetry):
		}
	}
}

func rebootStatus(ctx context.Context, g *globalFlags, t *podTarget) (string, error) {
	client := t.client
	if !t.spec.IsCloud() {
		g.usbAddr, g.usbErr = "", nil
		spec, err := g.networkTarget("reboot")
		if err != nil {
			return "", err
		}
		nt, err := openPodTarget(ctx, spec)
		if err != nil {
			return "", err
		}
		client = nt.client
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := client.Command(qctx, map[string]any{"cmd": "status"})
	if err != nil {
		return "", err
	}
	var st podStatusInfo
	if json.Unmarshal(data, &st) != nil {
		return "", errors.New("unexpected status reply")
	}
	return strings.TrimPrefix(st.Version, "v"), nil
}
