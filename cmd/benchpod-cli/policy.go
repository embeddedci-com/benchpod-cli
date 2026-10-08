package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── lan-policy / sig-policy ─────────────────────────────────────────────────
//
// Two persisted pod policies (firmware docs/design/policy-commands.md):
//
//   - lan-policy: open | locked | off. Settable from the cloud and the USB console, never from
//     the LAN itself.
//   - sig-policy: audit < permissive < required. The cloud may only make it stricter; the USB
//     console may set anything. The LAN can never change it.
//
// The target follows the CLI's conventions: --device-name / --device-id or --connection
// embeddedci:<name> go through embeddedci.com (like deregister), otherwise --connection picks the
// USB console or, for show only, the LAN JSON API.

// podPolicy describes one of the two policies.
type podPolicy struct {
	cmd     string            // CLI and console command, e.g. "lan-policy"
	jsonCmd string            // the pod's JSON verb, e.g. "lan_policy"
	title   string            // "LAN policy", starts a sentence
	noun    string            // "LAN policy", inside a sentence
	values  []string          // in order of strictness for sig-policy
	meaning map[string]string // one-line explanation of each value
	missing string            // what to say when the pod's firmware lacks the policy
}

var lanPolicy = podPolicy{
	cmd:     "lan-policy",
	jsonCmd: "lan_policy",
	title:   "LAN policy",
	noun:    "LAN policy",
	values:  []string{"open", "locked", "off"},
	meaning: map[string]string{
		"open":   "full access on the LAN",
		"locked": "read and instrument control only on the LAN",
		"off":    "no LAN access; cloud and USB only",
	},
	missing: "this firmware has no LAN policy; update the pod's firmware",
}

var sigPolicy = podPolicy{
	cmd:     "sig-policy",
	jsonCmd: "sig_policy",
	title:   "Signature policy",
	noun:    "signature policy",
	values:  []string{"audit", "permissive", "required"},
	meaning: map[string]string{
		"audit":      "every image is accepted; signatures are only reported",
		"permissive": "unsigned images are accepted, a bad signature is refused",
		"required":   "unsigned images are refused",
	},
	missing: "this firmware has no signature policy; update the pod's firmware",
}

func (p podPolicy) valid(v string) bool {
	for _, x := range p.values {
		if x == v {
			return true
		}
	}
	return false
}

// describe is "<value> (<meaning>)", or the bare value when the pod reports one this CLI does
// not know yet.
func (p podPolicy) describe(v string) string {
	if m, ok := p.meaning[v]; ok {
		return v + " (" + m + ")"
	}
	return v
}

// policyTarget is how the pod is picked: by name or id on embeddedci.com, or (both empty) through
// --connection.
type policyTarget struct {
	serverURL  string
	tokenFile  string
	deviceName string
	deviceID   string
}

func (t policyTarget) cloud() bool { return t.deviceName != "" || t.deviceID != "" }

func newLanPolicyCmd(g *globalFlags) *cobra.Command {
	return newPolicyCmd(g, lanPolicy,
		"Show or set the pod's LAN policy (open, locked, off)",
		"Show or set what the pod allows on its LAN TCP port.\n\n"+
			"  open    full access on the LAN (the default)\n"+
			"  locked  read and instrument control only on the LAN; configuration and\n"+
			"          firmware changes need the cloud or the USB console\n"+
			"  off     no LAN access at all: the pod stops listening on the LAN and stops\n"+
			"          advertising itself; the cloud and USB keep working\n\n"+
			"Over the cloud (--device-name, --device-id or --connection embeddedci:<name>)\n"+
			"the change needs an organization owner or admin. Over USB (--connection usb)\n"+
			"it always works. The LAN itself can only show the policy, never change it.")
}

func newSigPolicyCmd(g *globalFlags) *cobra.Command {
	return newPolicyCmd(g, sigPolicy,
		"Show or set the pod's signature policy (audit, permissive, required)",
		"Show or set how the pod treats firmware and blob signatures.\n\n"+
			"  audit       every image is accepted; signatures are only reported\n"+
			"  permissive  unsigned images are accepted, a bad signature is refused\n"+
			"  required    unsigned images are refused\n\n"+
			"Over the cloud (--device-name, --device-id or --connection embeddedci:<name>)\n"+
			"the policy can only be made stricter, and only by an organization owner or\n"+
			"admin. Only the USB console (--connection usb) can loosen it. The LAN can only\n"+
			"show it.")
}

func newPolicyCmd(g *globalFlags, p podPolicy, short, long string) *cobra.Command {
	t := &policyTarget{}
	run := func(set string) error {
		out, closeOut, err := resolveOutput(g.outputFilename)
		if err != nil {
			return fmt.Errorf("%s: open output: %w", p.cmd, err)
		}
		defer closeOut()
		return runPolicy(g, p, *t, set, out, os.Stderr)
	}
	show := func(_ *cobra.Command, _ []string) error { return run("") }

	root := &cobra.Command{
		Use:   p.cmd + " [show]",
		Short: short,
		Long:  long,
		Args:  cobra.NoArgs,
		RunE:  show,
	}
	root.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the pod's " + p.noun,
		Args:  cobra.NoArgs,
		RunE:  show,
	}, &cobra.Command{
		Use:       "set <" + strings.Join(p.values, "|") + ">",
		Short:     "Set the pod's " + p.noun,
		Args:      cobra.ExactArgs(1),
		ValidArgs: p.values,
		RunE:      func(_ *cobra.Command, args []string) error { return run(args[0]) },
	})
	pf := root.PersistentFlags()
	pf.StringVar(&t.serverURL, "server-url", "https://www.embeddedci.com", "embeddedci-server base URL")
	pf.StringVar(&t.tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	pf.StringVar(&t.deviceName, "device-name", "", "go through embeddedci.com to the registered pod with this name")
	pf.StringVar(&t.deviceID, "device-id", "", "go through embeddedci.com to the registered pod with this id")
	return root
}

// runPolicy shows the policy (set == "") or sets it, writing the one-line result to out and any
// warning to warn.
func runPolicy(g *globalFlags, p podPolicy, t policyTarget, set string, out, warn io.Writer) error {
	t.deviceName = strings.TrimSpace(t.deviceName)
	t.deviceID = strings.TrimSpace(t.deviceID)
	set = strings.ToLower(strings.TrimSpace(set))
	if set != "" && !p.valid(set) {
		return fmt.Errorf("%s set: %q is not a policy; use one of %s", p.cmd, set, strings.Join(p.values, ", "))
	}
	if t.deviceName != "" && t.deviceID != "" {
		return errors.New("pass only one of --device-name or --device-id")
	}
	if t.cloud() {
		return runPolicyCloud(g, p, t, set, out, warn)
	}

	raw, err := g.rawConnection()
	if err != nil {
		return err
	}
	if raw == "" {
		return fmt.Errorf("%s: no pod selected; pass --device-name <name> (over embeddedci.com) or --connection usb", p.cmd)
	}
	spec, err := parseTarget(raw)
	if err != nil {
		return err
	}
	if spec.IsCloud() {
		t.deviceName = spec.Name
		return runPolicyCloud(g, p, t, set, out, warn)
	}
	if spec.IsSerial() {
		return runPolicyUSB(g, p, spec.Device, set, out, warn)
	}
	if set != "" {
		return fmt.Errorf("%s set: the pod refuses policy changes from the LAN; use --device-name <name> (over embeddedci.com) or --connection usb", p.cmd)
	}
	return runPolicyLAN(g, p, spec.Addr, out)
}

// printPolicy writes the result line, e.g.
// "LAN policy of benchpod-baea06: locked (read and instrument control only on the LAN)".
func printPolicy(out io.Writer, p podPolicy, label, policy string, keys int, changed bool) {
	verb := ":"
	if changed {
		verb = " is now"
	}
	line := fmt.Sprintf("%s of %s%s %s", p.title, label, verb, p.describe(policy))
	if keys >= 0 {
		line += ", " + plural(keys, "trusted key", "trusted keys")
	}
	fmt.Fprintln(out, line)
}

// warnLanOff tells the user what stops working when the LAN is switched off.
func warnLanOff(warn io.Writer) {
	fmt.Fprintln(warn, "Note: with the LAN policy off, LAN tools stop working until it is set back:")
	fmt.Fprintln(warn, "SCPI/VISA, hwe2e TestHW and LAN discovery (`benchpod discover`). Set it back")
	fmt.Fprintln(warn, "with `benchpod lan-policy set open` over embeddedci.com or the USB console.")
}

// ── cloud ───────────────────────────────────────────────────────────────────

func runPolicyCloud(g *globalFlags, p podPolicy, t policyTarget, set string, out, warn io.Writer) error {
	if strings.TrimSpace(t.serverURL) == "" {
		return errors.New("--server-url cannot be empty")
	}
	tokenPath, err := resolveTokenPath(t.tokenFile)
	if err != nil {
		return fmt.Errorf("resolve token path: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(60*time.Second))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	api := serverapi.New(t.serverURL)
	tokens, err := ensureTokens(ctx, api, tokenPath)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	id, label := t.deviceID, t.deviceID
	if id == "" {
		devices, err := api.ListDevices(ctx, tokens.AccessToken)
		if err != nil {
			return fmt.Errorf("list devices: %w", err)
		}
		dev, err := findDeviceToDeregister(devices, "", t.deviceName)
		if err != nil {
			return err
		}
		id, label = dev.ID, dev.Name
	}

	cur, err := api.GetPodPolicy(ctx, tokens.AccessToken, id, p.cmd)
	if err != nil {
		return cloudPolicyError(p, "", err)
	}
	if !cur.Supported {
		return errors.New(p.missing)
	}
	if set == "" {
		printPolicy(out, p, label, cur.Policy, -1, false)
		return nil
	}

	res, err := api.SetPodPolicy(ctx, tokens.AccessToken, id, p.cmd, set)
	if err != nil {
		return cloudPolicyError(p, set, err)
	}
	printPolicy(out, p, label, res.Policy, -1, true)
	if p.cmd == lanPolicy.cmd && res.Policy == "off" {
		warnLanOff(warn)
	}
	return nil
}

// adminNeeded is what a 403 from the policy endpoints means.
const adminNeeded = "this needs an organization owner or admin (API keys: the benchpod:admin scope)"

// cloudPolicyError turns a failed policy call into one clear line: the server's or pod's own
// message, plus the USB way out when the cloud refused to loosen the signature policy.
func cloudPolicyError(p podPolicy, set string, err error) error {
	what := p.cmd
	if set != "" {
		what += " set"
	}
	var apiErr *serverapi.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%s: %w", what, err)
	}
	msg := podMessage(p, apiErr.Message)
	if apiErr.Status == http.StatusForbidden {
		if msg == "" {
			msg = adminNeeded
		}
		return relayedRefusal(p, what+": "+msg, msg)
	}
	if msg == "" {
		return fmt.Errorf("%s: %w", what, err)
	}
	if set != "" && p.cmd == sigPolicy.cmd && strings.Contains(strings.ToLower(msg), "loosen") {
		return relayedRefusal(p, fmt.Sprintf("%s: the pod refused: %s. Loosen it over the pod's USB console: benchpod sig-policy set %s --connection usb",
			what, msg, set), msg)
	}
	if set != "" {
		return relayedRefusal(p, what+": the pod refused: "+msg, msg)
	}
	return fmt.Errorf("%s: %s", what, msg)
}

// relayedRefusal is a refusal the server relayed (or its own 403), shown as text; it carries a
// refusal so the exit code says refused. Cmd names the JSON command, so it is not taken for a
// LAN reply.
func relayedRefusal(p podPolicy, text, reason string) error {
	return &shownRefusal{msg: text, pe: &tcpclient.PodError{Cmd: p.jsonCmd, Reason: reason}}
}

// podMessage strips the pod's "sig_policy: " / "lan_policy: " prefix from a relayed message, so
// the line does not name the command twice.
func podMessage(p podPolicy, msg string) string {
	msg = strings.TrimSpace(msg)
	for _, prefix := range []string{p.jsonCmd + ": ", p.cmd + ": "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	return msg
}

// ── USB console ─────────────────────────────────────────────────────────────

// policyConsole is the part of the USB console the policy commands use.
type policyConsole interface {
	Policy(ctx context.Context, cmd, set string) (serialconsole.PolicyReply, error)
	Close() error
}

// openPolicyConsole opens the pod's USB console; tests replace it with a fake.
var openPolicyConsole = func(g *globalFlags, device string, timeout time.Duration) (policyConsole, string, context.Context, context.CancelFunc, error) {
	console, path, ctx, cancel, err := g.openSerialConsole(device, timeout)
	if err != nil {
		return nil, "", nil, cancel, err
	}
	return console, path, ctx, cancel, nil
}

func runPolicyUSB(g *globalFlags, p podPolicy, device, set string, out, warn io.Writer) error {
	console, path, ctx, cancel, err := openPolicyConsole(g, device, g.effectiveTimeout(15*time.Second))
	if err != nil {
		return err
	}
	defer cancel()
	defer console.Close()

	what := p.cmd
	if set != "" {
		what += " set"
	}
	rep, err := console.Policy(ctx, p.cmd, set)
	if err != nil {
		if errors.Is(err, serialconsole.ErrPolicyUnsupported) {
			return errors.New(p.missing)
		}
		if pe, ok := tcpclient.AsPodError(err); ok {
			return refusedError(what, pe, false)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	keys := -1
	if p.cmd == sigPolicy.cmd {
		keys = rep.Keys
	}
	printPolicy(out, p, "the pod on "+path, rep.Policy, keys, set != "")
	if p.cmd == lanPolicy.cmd && set != "" && rep.Policy == "off" {
		warnLanOff(warn)
	}
	return nil
}

// ── LAN (show only) ─────────────────────────────────────────────────────────

func runPolicyLAN(g *globalFlags, p podPolicy, addr string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(15*time.Second))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	client := &tcpclient.Client{Addr: addr}
	data, err := client.Command(ctx, map[string]any{"cmd": p.jsonCmd})
	if err != nil {
		if strings.Contains(err.Error(), "unknown cmd") {
			return errors.New(p.missing)
		}
		if p.cmd == lanPolicy.cmd {
			return fmt.Errorf("%s: %w (a pod with the LAN policy off does not answer on the LAN; ask with --device-name or --connection usb)", p.cmd, err)
		}
		return fmt.Errorf("%s: %w", p.cmd, err)
	}
	var reply struct {
		Policy string `json:"policy"`
		Keys   *int   `json:"keys"`
	}
	if err := json.Unmarshal(data, &reply); err != nil || reply.Policy == "" {
		return fmt.Errorf("%s: unexpected reply %s", p.cmd, strings.TrimSpace(string(data)))
	}
	keys := -1
	if reply.Keys != nil && p.cmd == sigPolicy.cmd {
		keys = *reply.Keys
	}
	printPolicy(out, p, addr, reply.Policy, keys, false)
	return nil
}
