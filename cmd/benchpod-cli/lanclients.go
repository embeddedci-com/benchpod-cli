package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// LAN pairing (firmware 4.0.1+, feature lan_pairing; benchpod-firmware docs/design/lan-pairing.md):
// a computer on Ethernet or Wi-Fi drives the pod only once its TLS client key is on the pod's list.
// `benchpod setup` pairs this computer over the USB cable on its own; this command shows the list,
// pairs another key (a CI runner) and removes one. Changes go over USB (the console, or the USB
// network adapter) or embeddedci.com, never over the LAN.

const lanClientsMissing = "this firmware has no LAN pairing (anyone on the network may use the pod); update the pod's firmware (4.0.1 or newer)"

func newLanClientsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lan-clients",
		Short: "The computers paired with the pod for use over Ethernet and Wi-Fi",
		Long: "Show, pair or remove the computers paired with the pod (firmware 4.0.1+).\n\n" +
			"Over Ethernet and Wi-Fi the pod only answers status to a computer that is not paired.\n" +
			"`benchpod setup` pairs this computer over the pod's USB-C cable; a registered pod can also\n" +
			"be paired through embeddedci.com (--connection embeddedci:<pod>, as an organization admin).\n" +
			"Changes need USB or embeddedci.com: never the network itself.\n\n" +
			"  benchpod lan-clients                        list them\n" +
			"  benchpod lan-clients add                    pair this computer\n" +
			"  benchpod lan-clients add --new-key --name ci-runner-1\n" +
			"                                              pair a new key and print it for a CI secret\n" +
			"                                              (BENCHPOD_CLIENT_KEY_PEM on the runner)\n" +
			"  benchpod lan-clients remove <name|key>      unpair one (8+ hex digits of its key)\n" +
			"  benchpod lan-clients clear                  unpair them all",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLanClients(g, lanClientsOp{}, os.Stdout)
		},
	}
	var name string
	var newKey bool
	add := &cobra.Command{
		Use:   "add",
		Short: "Pair this computer (or, with --new-key, a new key for a CI runner)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLanClients(g, lanClientsOp{op: "add", name: name, newKey: newKey}, os.Stdout)
		},
	}
	add.Flags().StringVar(&name, "name", "", "the name the pod lists it under (default: this computer's host name)")
	add.Flags().BoolVar(&newKey, "new-key", false, "make a new key, pair it, and print its certificate and key PEM")
	remove := &cobra.Command{
		Use:   "remove <name|key>",
		Short: "Unpair the computers with this name, or the one key that starts with these hex digits",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runLanClients(g, lanClientsOp{op: "remove", what: args[0]}, os.Stdout)
		},
	}
	clear := &cobra.Command{
		Use:   "clear",
		Short: "Unpair every computer",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLanClients(g, lanClientsOp{op: "clear"}, os.Stdout)
		},
	}
	cmd.AddCommand(add, remove, clear)
	return cmd
}

type lanClientsOp struct {
	op     string // "", "add", "remove", "clear"
	name   string
	newKey bool
	what   string
}

// lanClientsView is the list as both transports report it.
type lanClientsView struct {
	Pairing bool              `json:"pairing"`
	Count   int               `json:"count"`
	Max     int               `json:"max"`
	Clients []lanClientRecord `json:"clients"`
}

type lanClientRecord struct {
	Name   string `json:"name"`
	SHA256 string `json:"spki_sha256"`
}

func runLanClients(g *globalFlags, o lanClientsOp, out io.Writer) error {
	var key, pemOut string
	if o.op == "add" {
		if o.newKey {
			cn := o.name
			if cn == "" {
				return usageErrorf("lan-clients add --new-key needs --name (what the runner is called)")
			}
			certPEM, keyPEM, err := tcpclient.NewClientIdentityPEM("benchpod client " + cn)
			if err != nil {
				return err
			}
			block, _ := pem.Decode(certPEM)
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return err
			}
			key, pemOut = tcpclient.SPKIHash(leaf), string(certPEM)+string(keyPEM)
		} else {
			h, err := tcpclient.ClientKeyHash()
			if err != nil {
				return fmt.Errorf("lan-clients add: %w", err)
			}
			key = h
		}
		if o.name == "" {
			o.name = tcpclient.PairingName()
		}
		o.name = tcpclient.SanitizePairingName(o.name)
	}

	spec, err := g.resolveTarget()
	if err != nil {
		return err
	}
	var v lanClientsView
	var label string
	if spec.IsSerial() {
		v, label, err = lanClientsConsole(g, spec.Device, o, key)
	} else {
		v, label, err = lanClientsNetwork(g, o, key)
	}
	if errors.Is(err, errAlreadyPaired) {
		fmt.Fprintf(out, "This computer (%s) is already paired with the pod at %s.\n", o.name, label)
		printLanClients(out, label, v, "")
		return nil
	}
	if err != nil {
		return err
	}
	switch o.op {
	case "add":
		fmt.Fprintf(out, "Paired %s with the pod at %s.\n", o.name, label)
	case "remove":
		fmt.Fprintf(out, "Removed %s from the pod at %s.\n", o.what, label)
	case "clear":
		fmt.Fprintf(out, "Removed every paired computer from the pod at %s.\n", label)
	}
	printLanClients(out, label, v, key)
	if pemOut != "" {
		fmt.Fprintln(out, "\nThe new key, for the runner's BENCHPOD_CLIENT_KEY_PEM secret (shown once, not stored here):")
		fmt.Fprint(out, pemOut)
	}
	return nil
}

func printLanClients(out io.Writer, label string, v lanClientsView, highlight string) {
	mine, _ := tcpclient.ClientKeyHash()
	state := "on: computers on Ethernet and Wi-Fi must be paired"
	if !v.Pairing {
		state = "off: every computer on the network may use the pod"
	}
	fmt.Fprintf(out, "Paired computers on %s: %d of %d (pairing %s)\n", label, v.Count, v.Max, state)
	for _, c := range v.Clients {
		note := ""
		switch {
		case mine != "" && strings.EqualFold(c.SHA256, mine):
			note = "  (this computer)"
		case highlight != "" && strings.EqualFold(c.SHA256, highlight):
			note = "  (new)"
		}
		fmt.Fprintf(out, "  %-23s %s%s\n", c.Name, c.SHA256[:16], note)
	}
}

func lanClientsConsole(g *globalFlags, device string, o lanClientsOp, key string) (lanClientsView, string, error) {
	console, path, err := g.openBenchpodSerial(device, 2*time.Second)
	if err != nil {
		return lanClientsView{}, "", err
	}
	defer console.Close()
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(20*time.Second))
	defer cancel()
	var l serialconsole.LanClients
	switch o.op {
	case "add":
		l, err = console.LanClientsAdd(ctx, o.name, key)
	case "remove":
		l, err = console.LanClientsRemove(ctx, o.what)
	case "clear":
		l, err = console.LanClientsClear(ctx)
	default:
		l, err = console.LanClientsList(ctx)
	}
	if errors.Is(err, serialconsole.ErrUnknownCommand) {
		return lanClientsView{}, "", errors.New(lanClientsMissing)
	}
	if err != nil {
		return lanClientsView{}, "", err
	}
	v := lanClientsView{Pairing: l.Pairing, Count: l.Count, Max: l.Max}
	for _, c := range l.Clients {
		v.Clients = append(v.Clients, lanClientRecord{Name: c.Name, SHA256: c.SHA256})
	}
	return v, "USB " + path, nil
}

func lanClientsNetwork(g *globalFlags, o lanClientsOp, key string) (lanClientsView, string, error) {
	ctx, cancel, t, err := g.podClient("lan-clients", 30*time.Second)
	if err != nil {
		return lanClientsView{}, "", err
	}
	defer cancel()
	// A LAN address: changes, and a list this computer may not read, go over the pod's USB link.
	lanAddr := ""
	if t.spec.IsNetwork() && !isUSBLinkAddr(t.spec.Addr) {
		usb, err := lanClientsRoute(ctx, t.spec.Addr, o)
		if errors.Is(err, errAlreadyPaired) {
			v, err := lanClientsAll(ctx, t.client, map[string]any{"cmd": "lan_clients"})
			if err != nil {
				return lanClientsView{}, "", fmt.Errorf("lan-clients: %w", err)
			}
			return v, t.where(), errAlreadyPaired
		}
		if err != nil {
			return lanClientsView{}, "", err
		}
		if usb != "" {
			lanAddr = t.spec.Addr
			t = &podTarget{spec: ConnSpec{Kind: connNetwork, Addr: usb}, client: &tcpclient.Client{Addr: usb},
				label: usbLinkLabel(t.spec.Addr, usb)}
		}
	}
	req := map[string]any{"cmd": "lan_clients"}
	switch o.op {
	case "add":
		req["add"], req["spki_sha256"] = o.name, key
	case "remove":
		req["remove"] = o.what
	case "clear":
		req["clear"] = true
	}
	v, err := lanClientsAll(ctx, t.client, req)
	if err != nil {
		if isUnknownCommand(err) {
			return lanClientsView{}, "", errors.New(lanClientsMissing)
		}
		return lanClientsView{}, "", fmt.Errorf("lan-clients: %w", err)
	}
	if lanAddr != "" && o.op == "add" {
		pinLanCertOverUSBLink(ctx, t.spec.Addr, lanAddr)
	}
	return v, t.where(), nil
}

// lanClientsAll runs req and reads every page of the list (the pod sends 12 a reply).
func lanClientsAll(ctx context.Context, c podCommander, req map[string]any) (lanClientsView, error) {
	var all lanClientsView
	for page := 0; page < 4; page++ {
		data, err := c.Command(ctx, req)
		if err != nil {
			return lanClientsView{}, err
		}
		var v lanClientsView
		if err := json.Unmarshal(data, &v); err != nil {
			return lanClientsView{}, fmt.Errorf("unexpected reply %s", strings.TrimSpace(string(data)))
		}
		all.Pairing, all.Count, all.Max = v.Pairing, v.Count, v.Max
		all.Clients = append(all.Clients, v.Clients...)
		if len(v.Clients) == 0 || len(all.Clients) >= v.Count {
			break
		}
		req = map[string]any{"cmd": "lan_clients", "offset": len(all.Clients)}
	}
	return all, nil
}

// ── setup's pairing ──────────────────────────────────────────────────────────

// errNotPaired is what setup says when it finds a pod only on the network and cannot pair.
var errNotPaired = errors.New("this computer is not paired with the pod, so over the network it may only read the pod's status. " +
	"Plug the pod's USB-C port (USB-C IN, on the back) into this computer once and run `benchpod setup` again: it pairs this computer over the cable. " +
	"A pod registered to your embeddedci.com account is paired from there too: `benchpod login`, then setup again")

// pairOverConsole pairs this computer with the pod on USB device and pins the pod's LAN
// certificate for lanAddr, so later LAN connections check they reach this very pod. "" when the
// firmware has no pairing (nothing to do).
func pairOverConsole(g *globalFlags, device, lanAddr string) (string, error) {
	key, err := tcpclient.ClientKeyHash()
	if err != nil {
		return "", fmt.Errorf("pair: %w", err)
	}
	console, _, err := g.openBenchpodSerial(device, 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("pair: %w", err)
	}
	defer console.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	name := tcpclient.PairingName()
	l, err := console.LanClientsAdd(ctx, name, key)
	if errors.Is(err, serialconsole.ErrUnknownCommand) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("pair this computer: %w", err)
	}
	if lanAddr != "" {
		if fp, err := console.LanTLSFingerprint(ctx); err == nil && fp != "" {
			if err := tcpclient.SetPinFingerprint(tcpclient.TLSAddr(lanAddr), fp, ""); err != nil {
				return "", fmt.Errorf("pin the pod's certificate: %w", err)
			}
		}
	}
	if !l.Pairing {
		return "", nil // pairing is off on this pod: nothing to say
	}
	if l.Outcome == "same" {
		return fmt.Sprintf("This computer is paired with it (%s).", name), nil
	}
	return fmt.Sprintf("Paired this computer (%s) with it over USB, so it can drive the pod over the network too.", name), nil
}

// lanPairing reads the pod's pairing state over the network: known is false on firmware without
// pairing or when status does not answer; paired is meaningless when the connection needs none.
func lanPairing(addr string) (known, required, paired bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	data, err := (&tcpclient.Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "status", "caps": false})
	if err != nil {
		return false, false, false
	}
	var st struct {
		Pairing *bool `json:"lan_pairing"`
		Paired  *bool `json:"lan_paired"`
	}
	if json.Unmarshal(data, &st) != nil || st.Pairing == nil {
		return false, false, false
	}
	if st.Paired == nil { // the USB link, or pairing off: nothing to say about this computer
		return false, false, false
	}
	return true, *st.Pairing, *st.Paired
}

// pairOverCloud pairs this computer through embeddedci.com (the pod's own cloud link). The server
// lets an organization owner or admin do it.
func pairOverCloud(g *globalFlags, podName string) (string, error) {
	key, err := tcpclient.ClientKeyHash()
	if err != nil {
		return "", err
	}
	gc := *g
	gc.connection = cloudConnPrefix + podName
	ctx, cancel, t, err := gc.podClient("pair", 30*time.Second)
	if err != nil {
		return "", err
	}
	defer cancel()
	name := tcpclient.PairingName()
	if _, err := lanClientsAll(ctx, t.client, map[string]any{"cmd": "lan_clients", "add": name, "spki_sha256": key}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Paired this computer (%s) with it through embeddedci.com.", name), nil
}

// ── lan-pairing ──────────────────────────────────────────────────────────────

func newLanPairingCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "lan-pairing [on|off]",
		Short: "Show or set whether computers on Ethernet and Wi-Fi must be paired",
		Long: "Show or set LAN pairing (firmware 4.0.1+). on (the default): a computer on Ethernet or\n" +
			"Wi-Fi drives the pod only once it is paired (`benchpod setup`, `benchpod lan-clients`).\n" +
			"off: every computer on the network may, as before 4.0.1; then keep the pod on a trusted\n" +
			"lab network. Changed over USB (the console or --connection usb) or embeddedci.com; given\n" +
			"the pod's network address, over that pod's own USB link when it is plugged in here.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(_ *cobra.Command, args []string) error {
			set := ""
			if len(args) == 1 {
				set = strings.ToLower(strings.TrimSpace(args[0]))
				if set != "on" && set != "off" {
					return usageErrorf("lan-pairing: use on or off (got %q)", args[0])
				}
			}
			return runLanPairing(g, set, os.Stdout)
		},
	}
}

func runLanPairing(g *globalFlags, set string, out io.Writer) error {
	spec, err := g.resolveTarget()
	if err != nil {
		return err
	}
	var on bool
	var label string
	if spec.IsSerial() {
		console, path, err := g.openBenchpodSerial(spec.Device, 2*time.Second)
		if err != nil {
			return err
		}
		defer console.Close()
		ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(15*time.Second))
		defer cancel()
		rep, err := console.Policy(ctx, "lan-pairing", set)
		if errors.Is(err, serialconsole.ErrPolicyUnsupported) {
			return errors.New(lanClientsMissing)
		}
		if err != nil {
			return err
		}
		on, label = rep.Policy == "on", "USB "+path
	} else {
		ctx, cancel, t, err := g.podClient("lan-pairing", 20*time.Second)
		if err != nil {
			return err
		}
		defer cancel()
		// A LAN address: a change, and a read this computer may not make while it is not paired,
		// go over the pod's USB link when it is plugged into this computer.
		var need func(lanPodState) bool
		if set == "" {
			need = notPairedHere
		}
		if t, _, err = targetOverUSBLink(ctx, t, need); err != nil {
			return fmt.Errorf("lan-pairing: %w", err)
		}
		req := map[string]any{"cmd": "lan_policy"}
		if set != "" {
			req["pairing"] = set == "on"
		}
		data, err := t.client.Command(ctx, req)
		if err != nil {
			return fmt.Errorf("lan-pairing: %w", err)
		}
		var rep struct {
			Pairing *bool `json:"pairing"`
		}
		if json.Unmarshal(data, &rep) != nil || rep.Pairing == nil {
			return errors.New(lanClientsMissing)
		}
		on, label = *rep.Pairing, t.where()
	}
	state := map[bool]string{true: "on: computers on Ethernet and Wi-Fi must be paired", false: "off: every computer on the network may use the pod"}[on]
	if set != "" {
		fmt.Fprintf(out, "LAN pairing on %s is now %s (stored on the pod).\n", label, state)
	} else {
		fmt.Fprintf(out, "LAN pairing on %s: %s.\n", label, state)
	}
	return nil
}
