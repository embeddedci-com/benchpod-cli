package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/libp2p/zeroconf/v2"
	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
)

// mdnsService is the DNS-SD service type the firmware advertises (see
// bench-pod-firmware stm32h563/src/net_server.c). Each pod publishes a unique
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
}

func newDiscoverCmd(g *globalFlags) *cobra.Command {
	var (
		wait time.Duration
		save bool
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Find BenchPods on the local network via mDNS (no IP needed)",
		Long: "Browse the LAN for BenchPods advertising over mDNS/DNS-SD and list every\n" +
			"one found, with its address and Ed25519 id. With --save and exactly one\n" +
			"pod present, store it as the default connection (like set-connection).\n\n" +
			"mDNS is link-local only: it works on a flat bench/office subnet but does\n" +
			"not cross routers/VLANs and is often blocked on CI runners.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			pods, err := discoverPods(g.effectiveTimeout(wait))
			if err != nil {
				return err
			}
			if len(pods) == 0 {
				return errors.New("no BenchPod found on the local network via mDNS " +
					"(check it is powered and on this subnet, or pass --connection)")
			}
			printPods(pods)
			if save {
				return saveDiscovered(g, pods)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 3*time.Second, "how long to browse for replies")
	cmd.Flags().BoolVar(&save, "save", false, "save as the default connection when exactly one pod is found")
	return cmd
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
				instance: e.Instance,
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
	if err := zeroconf.Browse(ctx, mdnsService, mdnsDomain, entries); err != nil {
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

func printPods(pods []discoveredPod) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "INSTANCE\tADDRESS\tHOSTNAME\tID")
	for _, p := range pods {
		id := p.id
		if len(id) > 16 {
			id = id[:16] + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.instance, p.addr, p.hostname, id)
	}
	_ = w.Flush()
}

func saveDiscovered(g *globalFlags, pods []discoveredPod) error {
	if len(pods) != 1 {
		return fmt.Errorf("--save needs exactly one pod, found %d; "+
			"set it explicitly with `benchpod set-connection <address>`", len(pods))
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
	cfg.Connection = pods[0].addr
	cfg.BenchPodAddr = "" // Connection is the source of truth.
	if err := benchpodconfig.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Default connection set to %s (saved to %s).\n", pods[0].addr, cfgPath)
	return nil
}
