package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/spf13/cobra"
)

// connKind is the transport implied by the --connection value.
type connKind int

const (
	connNetwork connKind = iota
	connSerial
	connCloud
)

// ConnSpec is a fully resolved connection target. A single --connection value
// (or stored default) carries both where and how to connect; the transport is
// inferred from the value's shape — there is no separate address flag.
//
//	192.168.1.5[:port], host        -> network/TCP (Addr set)
//	/dev/tty..., COM3, \\.\COM3      -> USB console (Device set)
//	usb (legacy: serial)            -> USB console, auto-detect (Device == "")
//	embeddedci:<name>               -> through embeddedci.com (Name set), where a command can work over it
type ConnSpec struct {
	Kind   connKind
	Addr   string // TCP host:port for the network transport
	Device string // serial device path; "" means auto-detect
	Name   string // the pod's name on embeddedci.com, for the cloud form
}

// cloudConnPrefix is the SDK's and MCP's connection form for a pod on embeddedci.com.
const cloudConnPrefix = "embeddedci:"

// comPattern matches a Windows serial device name like COM3 / COM12.
var comPattern = regexp.MustCompile(`^(?i:COM)\d+$`)

// isSerialDevice reports whether raw looks like a serial device path rather than
// a network address: a Unix device path (/dev/...), a Windows COM name, or the
// \\.\COMx form.
func isSerialDevice(raw string) bool {
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, `\\.\`) {
		return true
	}
	return comPattern.MatchString(raw)
}

// classifyConnection turns a raw --connection value (or stored default) into a ConnSpec for a
// command that talks to the pod itself, over the network or USB. It errors on an empty value, on
// a bare transport keyword that carries no address, and on the cloud form, which those commands
// cannot use.
func classifyConnection(raw string) (ConnSpec, error) {
	spec, err := parseTarget(raw)
	if err == nil && spec.IsCloud() {
		// Parsed as host:port it would be dialed (host "embeddedci", port "<name>") and retried
		// until the timeout, so say what it is instead.
		return ConnSpec{}, cloudRefusal("this command", spec, true)
	}
	return spec, err
}

// parseTarget is the one parser of a target (--connection value or stored default),
// inferring the transport from its shape. Unlike classifyConnection it accepts the
// cloud form, embeddedci:<name>; only the commands that can work over embeddedci.com
// use it directly (see globalFlags.resolveTarget).
func parseTarget(raw string) (ConnSpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ConnSpec{}, errors.New("no connection set. Run `benchpod discover --save` to find the pod and save it as the default " +
			"(or `benchpod setup` for the guided first-time setup), or pass --connection <address|device|usb|embeddedci:name>, " +
			"or save one with `benchpod set-connection <address|device|usb|embeddedci:name>`")
	}
	switch strings.ToLower(raw) {
	// "usb" is the name this keyword goes by everywhere the CLI speaks to a
	// user — it names what the person plugged in. "serial" is the older
	// spelling, still accepted so saved configs and BENCHPOD_CONNECTION values
	// written before the rename keep working; it is not advertised anywhere.
	case "usb", "serial":
		return ConnSpec{Kind: connSerial}, nil
	case "wifi", "tcp":
		return ConnSpec{}, fmt.Errorf("--connection %s needs an address, e.g. --connection 192.168.1.5", strings.ToLower(raw))
	}
	// The SDK's and MCP's cloud form.
	if strings.HasPrefix(strings.ToLower(raw), cloudConnPrefix) {
		name := strings.TrimSpace(raw[len(cloudConnPrefix):])
		if name == "" {
			return ConnSpec{}, fmt.Errorf("--connection %s needs the pod's name on embeddedci.com, e.g. %sbenchpod-a1b2c3", raw, cloudConnPrefix)
		}
		return ConnSpec{Kind: connCloud, Name: name}, nil
	}
	if isSerialDevice(raw) {
		return ConnSpec{Kind: connSerial, Device: raw}, nil
	}
	return ConnSpec{Kind: connNetwork, Addr: benchpodconfig.EnsurePort(raw)}, nil
}

func (c ConnSpec) IsNetwork() bool { return c.Kind == connNetwork }
func (c ConnSpec) IsSerial() bool  { return c.Kind == connSerial }
func (c ConnSpec) IsCloud() bool   { return c.Kind == connCloud }

// describeConn is a short human phrase for a resolved connection, used in
// confirmation messages.
func describeConn(c ConnSpec) string {
	if c.IsNetwork() {
		return "network/TCP " + c.Addr
	}
	if c.IsCloud() {
		return c.Name + " on embeddedci.com"
	}
	if c.Device != "" {
		return "usb " + c.Device
	}
	return "usb (auto-detect)"
}

// RequireNetwork guards the commands that need the pod's own network port (TCP/JSON or a raw
// stream on it): it errors unless the network transport is selected.
//
// The message names the way out rather than only the restriction, because a caller hitting this
// over USB is usually part-way through first-time setup and does not yet know the pod's address;
// `discover` is what finds it. Over embeddedci.com it names what does work there.
func (c ConnSpec) RequireNetwork(cmd string) error {
	if c.IsNetwork() {
		return nil
	}
	if c.IsCloud() {
		return cloudRefusal(cmd, c, false)
	}
	return fmt.Errorf("%s needs the pod on the network; it is not available over a USB connection. "+
		"Run `benchpod discover` to find the pod's address, then pass --connection <address>. "+
		"If it has no address yet, plug in Ethernet or run `benchpod set-wifi --ssid <ssid>`", cmd)
}

// cloudRefusal is the one message for a command that cannot go through embeddedci.com: it streams
// data or needs a direct link to the pod, which the CLI cannot get there. usb says whether the
// command also works over the pod's USB console.
func cloudRefusal(cmd string, c ConnSpec, usb bool) error {
	direct := "pass the pod's network address (`benchpod discover` finds it)"
	if usb {
		direct = "pass the pod's network address (`benchpod discover` finds it) or `usb`"
	}
	return fmt.Errorf("%s does not work over embeddedci.com (--connection %s%s): it needs a direct link to the pod. "+
		"Use the web app or the Python SDK / pytest plugin (they take %s%s), or %s. Over embeddedci.com the CLI runs: %s",
		cmd, cloudConnPrefix, c.Name, cloudConnPrefix, c.Name, direct, cloudCommandList)
}

// cloudCommandList names the commands that work over embeddedci.com, for messages and help.
const cloudCommandList = "status, ping, la voltage, la pullup, la status, la step, generate, " +
	"lan-policy, sig-policy, cloud ca show/clear, cloud proxy, deregister"

// ── set-connection ───────────────────────────────────────────────────────────

func newSetConnectionCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "set-connection <address|device|usb|embeddedci:name>",
		Aliases: []string{"set-bench-pod"},
		Short:   `Store the default connection: a network address, a device path, "usb", or embeddedci:<name>`,
		Long: "Store the default connection in the config file, so later commands can leave out\n" +
			"--connection. The value is what --connection takes: the pod's network address\n" +
			"(192.168.1.5[:8080]), a USB device path, `usb` to auto-detect the pod on USB, or\n" +
			"embeddedci:<name> to go through embeddedci.com.\n\n" +
			"The Python SDK and the MCP server read the same saved connection (see the README,\n" +
			"\"Shared config\").",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runSetConnection(g, args[0])
		},
	}
}

func runSetConnection(g *globalFlags, arg string) error {
	target := strings.TrimSpace(arg)
	if target == "" {
		return errors.New("connection target cannot be empty")
	}
	// Validate now (and report the inferred transport) rather than failing later.
	spec, err := parseTarget(target)
	if err != nil {
		return err
	}
	cfgPath, err := resolveConfigPath(g.configFile)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := benchpodconfig.Load(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil {
		cfg = &benchpodconfig.Config{}
	}
	cfg.Connection = target
	cfg.BenchPodAddr = "" // Connection is the source of truth; drop the legacy field.
	if err := benchpodconfig.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Default connection set to %s (saved to %s).\n", describeConn(spec), cfgPath)
	return nil
}
