package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// ── identity / identity wipe ────────────────────────────────────────────────
//
// The pod's device identity is an Ed25519 key in its own flash sector. The firmware generates it
// once, on a blank sector, and never writes over a record it does not recognize: such a pod stays
// offline and says why. `identity wipe` is the deliberate way back. It runs on the USB console
// only (physical presence); the pod refuses it over the LAN and the cloud.
//
// embeddedci.com knows a pod by its public key, so after a wipe the pod is a new pod there: the old
// device record keeps its data and its login state, and the pod is registered again as a new
// record. Nothing on the server has to be reset for the new key (the host-bound login flag,
// ws_auth.v2, belongs to the old record).

const identityMissing = "this firmware has no identity commands; update the pod's firmware"

// identityConsole is the part of the USB console the identity commands use.
type identityConsole interface {
	Identity(ctx context.Context) (serialconsole.IdentityState, error)
	IdentityWipe(ctx context.Context, token string) (string, error)
	Close() error
}

// openIdentityConsole opens the pod's USB console; tests replace it with a fake.
var openIdentityConsole = func(g *globalFlags, device string, timeout time.Duration) (identityConsole, string, context.Context, context.CancelFunc, error) {
	console, path, ctx, cancel, err := g.openSerialConsole(device, timeout)
	if err != nil {
		return nil, "", nil, cancel, err
	}
	return console, path, ctx, cancel, nil
}

// stdinIsTerminal reports whether a confirmation prompt can be answered; tests override it.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func newIdentityCmd(g *globalFlags) *cobra.Command {
	show := func(_ *cobra.Command, _ []string) error {
		out, closeOut, err := resolveOutput(g.outputFilename)
		if err != nil {
			return fmt.Errorf("identity: open output: %w", err)
		}
		defer closeOut()
		return runIdentityShow(g, out)
	}
	root := &cobra.Command{
		Use:   "identity [show]",
		Short: "Show the pod's device identity, or wipe it (USB console only)",
		Long: "Show the pod's device identity (its Ed25519 key, the source of its name\n" +
			"benchpod-a1b2c3) over the USB console.\n\n" +
			"A pod whose identity record the firmware does not recognize stays offline rather\n" +
			"than replace a key it may be registered with; `benchpod identity show` then says\n" +
			"why. `benchpod identity wipe` erases the key and makes a new one.",
		Args: cobra.NoArgs,
		RunE: show,
	}
	root.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the pod's device identity",
		Args:  cobra.NoArgs,
		RunE:  show,
	})

	var yes bool
	wipe := &cobra.Command{
		Use:   "wipe",
		Short: "Erase the pod's device key and make a new one (USB console only)",
		Long: "Erase the pod's device identity and generate a fresh key, over the pod's USB\n" +
			"console (--connection usb or a serial device path). The pod refuses this over the\n" +
			"LAN and the cloud: it needs someone at the pod.\n\n" +
			"Use it to recover a pod that reports an unknown identity record, or to retire a\n" +
			"key. The pod is a NEW pod to embeddedci.com afterwards: deregister the old record\n" +
			"and register the pod again (the command prints the steps).\n\n" +
			"It shows the current identity and asks for confirmation; --yes skips the question\n" +
			"for scripts.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("identity wipe: open output: %w", err)
			}
			defer closeOut()
			return runIdentityWipe(g, yes, os.Stdin, out)
		},
	}
	wipe.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")
	root.AddCommand(wipe)
	return root
}

// identityDevice resolves --connection to a serial device ("" = auto-detect), refusing anything
// that is not the USB console.
func identityDevice(g *globalFlags, what string) (string, error) {
	raw, err := g.rawConnection()
	if err != nil {
		return "", err
	}
	if raw == "" {
		return "", fmt.Errorf("%s: needs the pod's USB console; pass --connection usb (or a serial device path)", what)
	}
	spec, err := classifyConnection(raw)
	if err != nil {
		return "", err
	}
	if !spec.IsSerial() {
		return "", fmt.Errorf("%s: only over the pod's USB console (physical presence); the pod refuses it over the LAN and the cloud. Pass --connection usb", what)
	}
	return spec.Device, nil
}

// describeIdentity is the one-line state, e.g. "benchpod-a1b2c3 (key ...)" or
// "none: unknown identity record ...".
func describeIdentity(st serialconsole.IdentityState) string {
	if st.Known() {
		return fmt.Sprintf("%s (public key %s)", st.Name(), st.PublicKey)
	}
	return "none: " + st.Problem
}

func identityErr(what string, err error) error {
	if errors.Is(err, serialconsole.ErrIdentityUnsupported) {
		return errors.New(identityMissing)
	}
	if pe, ok := tcpclient.AsPodError(err); ok {
		return refusedError(what, pe, false)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func runIdentityShow(g *globalFlags, out io.Writer) error {
	device, err := identityDevice(g, "identity")
	if err != nil {
		return err
	}
	console, path, ctx, cancel, err := openIdentityConsole(g, device, g.effectiveTimeout(15*time.Second))
	if err != nil {
		return err
	}
	defer cancel()
	defer console.Close()
	st, err := console.Identity(ctx)
	if err != nil {
		return identityErr("identity", err)
	}
	fmt.Fprintf(out, "Identity of the pod on %s: %s\n", path, describeIdentity(st))
	if !st.Known() {
		fmt.Fprintln(out, "The pod stays offline without an identity. Recover it with: benchpod identity wipe --connection usb")
	}
	return nil
}

func runIdentityWipe(g *globalFlags, yes bool, in io.Reader, out io.Writer) error {
	device, err := identityDevice(g, "identity wipe")
	if err != nil {
		return err
	}
	if !yes && !stdinIsTerminal() {
		return errors.New("identity wipe: confirmation needed; run it in a terminal or pass --yes")
	}
	console, path, ctx, cancel, err := openIdentityConsole(g, device, g.effectiveTimeout(30*time.Second))
	if err != nil {
		return err
	}
	defer cancel()
	defer console.Close()

	st, err := console.Identity(ctx)
	if err != nil {
		return identityErr("identity wipe", err)
	}
	fmt.Fprintf(out, "Identity of the pod on %s: %s\n", path, describeIdentity(st))
	if !yes {
		fmt.Fprintln(out, "This erases the pod's device key for good and makes a new one. To embeddedci.com")
		fmt.Fprintln(out, "the pod is then a new pod that has to be registered again.")
		fmt.Fprint(out, "Wipe the identity? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errors.New("identity wipe: cancelled, nothing changed")
		}
	}
	newID, err := console.IdentityWipe(ctx, st.ConfirmToken())
	if err != nil {
		return identityErr("identity wipe", err)
	}
	printIdentityNextSteps(out, st, newID)
	return nil
}

// printIdentityNextSteps explains what the server needs now that the pod has a new key.
func printIdentityNextSteps(out io.Writer, old serialconsole.IdentityState, newID string) {
	newName := "benchpod-" + newID
	fmt.Fprintf(out, "Identity wiped: the pod is now %s.\n\n", newName)
	oldName := old.Name()
	nameArg := "<old name>"
	if oldName != "" {
		nameArg = oldName
	}
	fmt.Fprintln(out, "embeddedci.com knows a pod by its public key, so it does not know this one yet")
	fmt.Fprintln(out, "(the pod's cloud login is refused until it is registered again). Next:")
	fmt.Fprintln(out, "  1. Deregister the old record. It keeps its captures, waveforms and wiring, and")
	fmt.Fprintln(out, "     frees the name. This works while the pod is offline:")
	fmt.Fprintf(out, "       benchpod deregister --device-name %s\n", nameArg)
	if oldName == "" {
		fmt.Fprintln(out, "     (the old name is on the BenchPod page of embeddedci.com; an organization owner")
		fmt.Fprintln(out, "     or admin can also deregister it there)")
	}
	fmt.Fprintln(out, "  2. Power-cycle the pod, so it advertises its new name on the LAN.")
	fmt.Fprintln(out, "  3. Register it again over the LAN (it needs Ethernet or Wi-Fi):")
	fmt.Fprintf(out, "       benchpod register --connection <pod address> --device-name %s\n", nameArg)
	fmt.Fprintln(out, "  4. Set its wiring profile again on the BenchPod page (it stays with the old record).")
	fmt.Fprintln(out, "No server-side login reset is needed: the host-bound login state (ws_auth.v2)")
	fmt.Fprintln(out, "belongs to the old record, and the new record sets it on the first login.")
}
