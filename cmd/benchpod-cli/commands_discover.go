package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/libp2p/zeroconf/v2"
	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// mdnsService is the DNS-SD service type the firmware advertises (see
// benchpod-firmware stm32h563/src/net_server.c). Each pod publishes a unique
// hostname/instance derived from a slice of its Ed25519 public key, with the
// full public key in the TXT "id=" item.
const (
	mdnsService = "_benchpod._tcp"
	mdnsDomain  = "local."
)

// discoveredPod is one pod heard on the LAN, flattened from a zeroconf entry.
type discoveredPod struct {
	instance string // DNS-SD instance, e.g. "BenchPod a1b2c3"
	hostname string // "benchpod-a1b2c3.local."
	addr     string // "host:port", a numeric IPv4 when one was resolved
	id       string // full base64url Ed25519 pubkey (TXT id=)

	// Filled in by the health check.
	reachable bool
	healthErr string // why the API check failed, when it did
	cloud     cloudState
	sameAs    string // serial device of the pod this is (the same physical pod)
}

// cloudState is the pod's view of its registration, from `cloud_status`.
type cloudState struct {
	known      bool
	State      string `json:"state"`
	Configured bool   `json:"configured"`
	DeviceID   string `json:"device_id"`
}

func newDiscoverCmd(g *globalFlags) *cobra.Command {
	var (
		wait         time.Duration
		probeTimeout time.Duration
		save         bool
		noUSB        bool
		noNetwork    bool
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find every BenchPod on this machine and this LAN, and report whether each one works",
		Long: "Answer two questions in one command: can a BenchPod be found, and is it working?\n\n" +
			"discover looks in both places a pod can be:\n" +
			"  • USB   — every USB port is probed for a bench-pod console, which is how\n" +
			"            a pod is found before it has any network at all.\n" +
			"  • LAN   — mDNS/DNS-SD browsing for pods advertising " + mdnsService + ".\n\n" +
			"Each pod found on the network is then checked over its TCP/JSON API, so the\n" +
			"report says whether the pod answers commands and whether it is registered\n" +
			"with the cloud — not merely that something replied to a broadcast. When a pod\n" +
			"is reachable both ways the two entries are matched up as one physical pod.\n\n" +
			"With --save and exactly one pod present, its address is stored as the default\n" +
			"connection (like set-connection), preferring the network address.\n\n" +
			"mDNS is link-local: it works on a flat bench/office subnet but does not cross\n" +
			"routers/VLANs and is often blocked on CI runners. USB probing needs no network\n" +
			"at all, so a pod that is plugged in always shows up even when mDNS is blocked.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDiscover(g, discoverOpts{
				wait:         g.effectiveTimeout(wait),
				probeTimeout: probeTimeout,
				save:         save,
				usb:          !noUSB,
				network:      !noNetwork,
			})
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 3*time.Second, "how long to browse the LAN for mDNS replies")
	cmd.Flags().DurationVar(&probeTimeout, "probe-timeout", 2*time.Second, "how long to wait for each USB port to answer `status`")
	cmd.Flags().BoolVar(&save, "save", false, "save as the default connection when exactly one pod is found")
	cmd.Flags().BoolVar(&noUSB, "no-usb", false, "skip probing USB ports")
	cmd.Flags().BoolVar(&noNetwork, "no-network", false, "skip mDNS browsing of the LAN")
	return cmd
}

type discoverOpts struct {
	wait         time.Duration
	probeTimeout time.Duration
	save         bool
	usb, network bool
}

// runDiscover gathers both halves of the bench picture, prints a report, and
// fails only when nothing was found anywhere — a pod that is present but
// unhealthy is a successful discovery with a bad verdict, which the report
// explains rather than hiding behind an error.
func runDiscover(g *globalFlags, o discoverOpts) error {
	if !o.usb && !o.network {
		return errors.New("--no-usb and --no-network together leave nothing to look for")
	}

	var (
		serialPods []serialconsole.SerialPod
		skipped    []string
		serialErr  error
		netPods    []discoveredPod
		netErr     error
	)

	if o.usb {
		serialPods, skipped, serialErr = serialconsole.ProbeSerial(g.serialDevice(), o.probeTimeout)
	}
	if o.network {
		netPods, netErr = discoverPods(o.wait)
		// Only a pod that answers its API is actually usable, so check each one
		// rather than trusting the advertisement.
		for i := range netPods {
			checkPod(&netPods[i])
		}
		correlate(netPods, serialPods)
	}

	printReport(reportInput{
		opts:       o,
		serialPods: serialPods,
		skipped:    skipped,
		serialErr:  serialErr,
		netPods:    netPods,
		netErr:     netErr,
	})

	if len(serialPods) == 0 && len(netPods) == 0 {
		return errors.New("no BenchPod found (see the suggestions above)")
	}
	if o.save {
		return saveDiscovered(g, netPods, serialPods)
	}
	return nil
}

// checkPod asks the pod itself whether it is working: `ping` proves the TCP/JSON
// API is served (an mDNS record can outlive the firmware that published it), and
// `cloud_status` reports whether it is registered and connected. A cloud_status
// failure is not fatal — an older firmware may not implement it — so only ping
// decides reachability.
func checkPod(p *discoveredPod) {
	client := &tcpclient.Client{Addr: p.addr}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := client.Command(ctx, map[string]any{"cmd": "ping"})
	cancel()
	if err != nil {
		p.healthErr = err.Error()
		return
	}
	p.reachable = true

	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	raw, err := client.Command(ctx, map[string]any{"cmd": "cloud_status"})
	cancel()
	if err == nil {
		if json.Unmarshal(raw, &p.cloud) == nil {
			p.cloud.known = true
		}
	}
}

// correlate marks each network pod that is the same physical device as one of
// the pods found over USB, matched on the address the pod reports for itself.
// Without this a single pod plugged in and on the LAN reads as two.
func correlate(netPods []discoveredPod, serialPods []serialconsole.SerialPod) {
	for i := range netPods {
		host, _, err := net.SplitHostPort(netPods[i].addr)
		if err != nil {
			host = netPods[i].addr
		}
		for _, sp := range serialPods {
			if sp.Addressed() && sp.IP == host {
				netPods[i].sameAs = sp.Device
				break
			}
		}
	}
}

// discoverPods browses the LAN for `wait` and returns the pods found, sorted by
// instance name and de-duplicated across interfaces.
func discoverPods(wait time.Duration) ([]discoveredPod, error) {
	entries := make(chan *zeroconf.ServiceEntry, 16)
	byKey := map[string]discoveredPod{}

	// Drain entries in the background. zeroconf.Browse closes this channel when
	// the context expires, so the loop ends on its own; byKey is read only after
	// the drain has finished (below), so no lock is needed.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for e := range entries {
			p := discoveredPod{
				instance: unescapeDNSSD(e.Instance),
				hostname: strings.TrimSuffix(e.HostName, "."),
				id:       txtValue(e.Text, "id"),
			}
			// Prefer a numeric IPv4 over the .local name so we don't depend on
			// the OS mDNS resolver later.
			host := p.hostname
			if len(e.AddrIPv4) > 0 {
				host = e.AddrIPv4[0].String()
			}
			p.addr = benchpodconfig.EnsurePort(fmt.Sprintf("%s:%d", host, e.Port))
			byKey[e.Instance] = p
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	// Browse blocks until ctx expires (returning nil for a browse) and closes
	// `entries` on the way out. A non-nil error is a setup failure (e.g. no
	// multicast-capable interface) before the channel was handed off.
	//
	// The interfaces are chosen explicitly rather than left to the library's
	// default: on macOS the default selection silently returns nothing at all,
	// while the very same browse restricted to the real IPv4 interfaces finds
	// the pod immediately. IPv4-only for the same reason — the firmware
	// advertises an A record, and asking for both families reintroduces the
	// empty result.
	opts := []zeroconf.ClientOption{zeroconf.SelectIPTraffic(zeroconf.IPv4)}
	if ifaces := multicastIfaces(); len(ifaces) > 0 {
		opts = append(opts, zeroconf.SelectIfaces(ifaces))
	}
	if err := zeroconf.Browse(ctx, mdnsService, mdnsDomain, entries, opts...); err != nil {
		return nil, fmt.Errorf("browse %s: %w", mdnsService, err)
	}
	<-drained

	pods := make([]discoveredPod, 0, len(byKey))
	for _, p := range byKey {
		pods = append(pods, p)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].instance < pods[j].instance })
	return pods, nil
}

// multicastIfaces lists the interfaces worth browsing for mDNS: up, multicast
// capable, not loopback, and carrying an IPv4 address. Loopback is excluded
// because a pod is never on it and including it is one of the ways the default
// selection ends up finding nothing.
func multicastIfaces() []net.Interface {
	all, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 ||
			ifi.Flags&net.FlagMulticast == 0 ||
			ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aErr := ifi.Addrs()
		if aErr != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ifi)
				break
			}
		}
	}
	return out
}

// unescapeDNSSD undoes the escaping DNS-SD applies to an instance label, where
// a space arrives as "\\ " and a literal dot as "\\.". Without this the pod
// reads as "BenchPod\ b83ba1" everywhere it is printed.
func unescapeDNSSD(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// txtValue returns the value of key=... from a DNS-SD TXT record, or "".
func txtValue(txt []string, key string) string {
	prefix := key + "="
	for _, kv := range txt {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

type reportInput struct {
	opts       discoverOpts
	serialPods []serialconsole.SerialPod
	skipped    []string
	serialErr  error
	netPods    []discoveredPod
	netErr     error
}

// printReport writes the whole picture to stdout: what was found over USB, what
// was found on the LAN and whether it answers, then a verdict with the single
// next command worth running. Everything goes to stdout (not the log) because
// this is the command's output, not a diagnostic about it.
func printReport(in reportInput) {
	out := os.Stdout

	if in.opts.usb {
		fmt.Fprintln(out, "USB")
		switch {
		case in.serialErr != nil:
			fmt.Fprintf(out, "  could not enumerate USB ports: %v\n", in.serialErr)
		case len(in.serialPods) == 0:
			fmt.Fprintf(out, "  no bench-pod console found (%s)\n", probedSummary(in.skipped))
		default:
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			for _, p := range in.serialPods {
				fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", p.Device, describeBoard(p), describePodIP(p),
					describeSerialRegistration(p))
			}
			_ = w.Flush()
		}
		fmt.Fprintln(out)
	}

	if in.opts.network {
		fmt.Fprintln(out, "Network (mDNS)")
		switch {
		case in.netErr != nil:
			fmt.Fprintf(out, "  could not browse the LAN: %v\n", in.netErr)
		case len(in.netPods) == 0:
			fmt.Fprintf(out, "  nothing advertising %s on this subnet\n", mdnsService)
		default:
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "  INSTANCE\tADDRESS\tHOSTNAME\tID")
			for _, p := range in.netPods {
				fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", p.instance, p.addr, p.hostname, shortID(p.id))
			}
			_ = w.Flush()
			for _, p := range in.netPods {
				fmt.Fprintf(out, "  %s: %s\n", p.instance, describeHealth(p))
			}
		}
		fmt.Fprintln(out)
	}

	printVerdict(out, in)
}

// printVerdict turns the findings into a plain-language summary plus the one
// command worth running next — the part of the output a first-time user reads.
func printVerdict(out *os.File, in reportInput) {
	total := len(in.serialPods) + countUnmatched(in.netPods)
	if total == 0 {
		fmt.Fprintln(out, "No BenchPod found.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Things to try:")
		if in.opts.usb {
			fmt.Fprintln(out, "  • Check the pod is powered and that the USB cable carries data (many are charge-only).")
			fmt.Fprintln(out, "  • A pod with no firmware never opens a console or advertises itself. Flash the")
			fmt.Fprintln(out, "    latest release over USB DFU with `benchpod flash-self`.")
		}
		if in.opts.network {
			fmt.Fprintln(out, "  • mDNS does not cross routers/VLANs and is usually blocked on CI runners —")
			fmt.Fprintln(out, "    reach a pod you know the address of with `--connection <ip>`.")
		}
		if !in.opts.usb {
			fmt.Fprintln(out, "  • Re-run without --no-usb to look for a pod connected over USB.")
		}
		return
	}

	fmt.Fprintf(out, "%s found.\n", plural(total, "BenchPod", "BenchPods"))

	// A pod on USB with no address cannot be registered or driven from CI, and
	// that is the most common half-finished state, so call it out by name.
	for _, p := range in.serialPods {
		if !p.Addressed() {
			fmt.Fprintf(out, "\n%s has no network address yet. Plug in Ethernet (it takes a DHCP lease on\n"+
				"its own), or join Wi-Fi with `benchpod set-wifi --ssid <ssid>`. Tests and\n"+
				"`benchpod register` both need the pod on the network.\n", p.Device)
		}
	}
	for _, p := range in.netPods {
		if !p.reachable {
			fmt.Fprintf(out, "\n%s advertises itself but does not answer its API on %s.\n"+
				"The record may be stale (a pod that has since rebooted or moved), or a firewall\n"+
				"may be blocking port 8080.\n", p.instance, p.addr)
		}
	}

	printRegistrationVerdict(out, in)

	if target := saveTarget(in.netPods, in.serialPods); target != "" && !in.opts.save {
		fmt.Fprintf(out, "\nNext: `benchpod discover --save` stores %s as the default connection,\n"+
			"so later commands can omit --connection.\n", target)
	}
}

// printRegistrationVerdict answers the question a new user actually has: is this pod already
// claimed into an account, or do I have to claim it?
//
// Both are ordinary states and neither is an error. A pod that was set up for somebody before it
// was shipped is already registered, and telling that person to run `benchpod register` would
// send them to redo work that is done — so the two cases get different, equally definite advice.
// Nothing is printed when no pod could report its registration (older firmware, or a pod that
// answered neither transport), because silence is honest and a guess is not.
func printRegistrationVerdict(out *os.File, in reportInput) {
	registered, unregistered := 0, 0
	for _, p := range in.serialPods {
		// Skip a pod that also appeared on the network: the network entry is counted below and
		// carries the live cloud state, so counting both would double one physical pod.
		if !p.CloudKnown || serialPodAlsoOnNetwork(p, in.netPods) {
			continue
		}
		if p.Registered {
			registered++
		} else {
			unregistered++
		}
	}
	for _, p := range in.netPods {
		if !p.cloud.known {
			continue
		}
		if p.cloud.Configured {
			registered++
		} else {
			unregistered++
		}
	}

	switch {
	case registered > 0 && unregistered == 0:
		fmt.Fprintf(out, "\nAlready registered — nothing to set up on the pod itself.\n"+
			"Sign in at https://www.embeddedci.com and it will be on your BenchPod page.\n"+
			"Tests address it by name: `pytest --benchpod-connection=embeddedci:<name>`.\n")
	case unregistered > 0 && registered == 0:
		fmt.Fprintln(out, "\nNot registered yet. To drive this pod through embeddedci.com (and from CI):")
		fmt.Fprintln(out, "  benchpod login       # authenticate this machine")
		fmt.Fprintln(out, "  benchpod register    # claim the pod into your account")
		fmt.Fprintln(out, "Or skip both and address the pod directly by IP — no account needed.")
	case registered > 0 && unregistered > 0:
		fmt.Fprintf(out, "\n%d registered, %d not. `benchpod register --connection <ip>` claims the\n"+
			"unregistered one; the registered one needs nothing.\n", registered, unregistered)
	}
}

// serialPodAlsoOnNetwork reports whether this USB pod is the same physical pod as one of the
// network entries, which correlate() has already matched up by address.
func serialPodAlsoOnNetwork(p serialconsole.SerialPod, netPods []discoveredPod) bool {
	for _, np := range netPods {
		if np.sameAs == p.Device {
			return true
		}
	}
	return false
}

// countUnmatched counts network pods that were not already counted as a serial
// pod, so one physical pod seen over both transports is one pod.
func countUnmatched(pods []discoveredPod) int {
	n := 0
	for _, p := range pods {
		if p.sameAs == "" {
			n++
		}
	}
	return n
}

// describeSerialRegistration is the USB half of the registration verdict. It matters most for a
// pod somebody was sent already set up: over USB, before the pod has any network at all, this is
// the only thing that can say so. Firmware too old to report it says nothing rather than guessing.
func describeSerialRegistration(p serialconsole.SerialPod) string {
	if !p.CloudKnown {
		return ""
	}
	if !p.Registered {
		return "not registered"
	}
	if state := strings.TrimSpace(p.CloudState); state != "" {
		return "registered, cloud " + strings.ToLower(state)
	}
	return "registered"
}

func describeBoard(p serialconsole.SerialPod) string {
	board := strings.TrimSpace(p.Board)
	if board == "" {
		board = "bench-pod"
	}
	if p.Firmware != "" {
		return board + "  fw " + p.Firmware
	}
	return board
}

func describePodIP(p serialconsole.SerialPod) string {
	if p.Addressed() {
		return "ip " + p.IP
	}
	return "no network address"
}

// describeHealth is the one-line health verdict for a network pod: whether its
// API answers, whether it is registered with the cloud, and whether it is the
// same physical pod as one already listed under USB.
func describeHealth(p discoveredPod) string {
	var parts []string
	if p.reachable {
		parts = append(parts, "API ok")
	} else {
		msg := "API unreachable"
		if p.healthErr != "" {
			msg += " (" + firstLine(p.healthErr) + ")"
		}
		parts = append(parts, msg)
	}
	if p.cloud.known {
		switch {
		case !p.cloud.Configured:
			parts = append(parts, "not registered (`benchpod login` then `benchpod register`)")
		default:
			parts = append(parts, fmt.Sprintf("registered, cloud %s", strings.ToLower(strings.TrimSpace(p.cloud.State))))
		}
	}
	if p.sameAs != "" {
		parts = append(parts, "same pod as "+p.sameAs)
	}
	return strings.Join(parts, ", ")
}

func probedSummary(skipped []string) string {
	if len(skipped) == 0 {
		return "no USB ports to probe"
	}
	return fmt.Sprintf("probed %s: %s", plural(len(skipped), "port", "ports"), strings.Join(skipped, ", "))
}

func shortID(id string) string {
	if len(id) > 16 {
		return id[:16] + "…"
	}
	return id
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// saveTarget is the connection string --save would store: the network address
// of the single pod found (preferred — it works from other machines and from
// CI), else the serial device of the single pod found over USB. It returns ""
// when the choice is ambiguous, so --save never guesses between two pods.
func saveTarget(netPods []discoveredPod, serialPods []serialconsole.SerialPod) string {
	if len(netPods) == 1 {
		return netPods[0].addr
	}
	if len(netPods) == 0 && len(serialPods) == 1 {
		return serialPods[0].Device
	}
	return ""
}

func saveDiscovered(g *globalFlags, netPods []discoveredPod, serialPods []serialconsole.SerialPod) error {
	target := saveTarget(netPods, serialPods)
	if target == "" {
		return fmt.Errorf("--save needs exactly one pod, found %d on the network and %d over USB; "+
			"set it explicitly with `benchpod set-connection <address|device>`", len(netPods), len(serialPods))
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
	cfg.BenchPodAddr = "" // Connection is the source of truth.
	if err := benchpodconfig.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Default connection set to %s (saved to %s).\n", target, cfgPath)
	return nil
}
