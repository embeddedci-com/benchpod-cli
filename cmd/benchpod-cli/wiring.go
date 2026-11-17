package main

// benchpod wiring: the wiring profiles stored on the pod (firmware wiring_list / wiring_get /
// wiring_put / wiring_delete / wiring_default), and benchpod explain for the error codes.
//
// A pod with current firmware keeps up to 16 named wiring profiles, one of them its default. The
// same profiles are seen over the LAN, the USB network and embeddedci.com. A profile is schema
// version 2: what it does not set is undefined (that pin is not wired).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

const (
	wiringPiece   = 600 // the firmware's WIRING_PIECE_MAX
	wiringTimeout = 20 * time.Second
)

// referenceBench is what a version 1 profile fills in (the EmbeddedCI reference bench, a
// NUCLEO-F446RE): the defaults profiles had before version 2.
var referenceBench = map[string]any{
	"la_mv": 3300, "efuse": 1, "uart_rx": 5, "uart_tx": 4, "i2c_sda": 1, "i2c_scl": 2,
	"i2c_addr": "0x76", "swd_swclk": 11, "swd_swdio": 12,
}

type wiringListReply struct {
	Default  *string `json:"default"`
	Profiles []struct {
		Name  string `json:"name"`
		Rev   int    `json:"rev"`
		Bytes int    `json:"bytes"`
	} `json:"profiles"`
	MaxProfiles int `json:"max_profiles"`
	MaxBytes    int `json:"max_bytes"`
	FreeBytes   int `json:"free_bytes"`
}

func wiringList(ctx context.Context, c podCommander) (*wiringListReply, error) {
	raw, err := c.Command(ctx, map[string]any{"cmd": "wiring_list"})
	if err != nil {
		return nil, err
	}
	var probe struct {
		Profiles *json.RawMessage `json:"profiles"`
	}
	if json.Unmarshal(raw, &probe) != nil || probe.Profiles == nil {
		return nil, errNoWiringStore // firmware that stores profiles always lists them
	}
	var l wiringListReply
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("wiring_list: %w", err)
	}
	return &l, nil
}

// errNoWiringStore: the pod's firmware stores no wiring profiles.
var errNoWiringStore = errors.New("this pod's firmware does not store wiring profiles; update it with `benchpod flash-self`")

// wiringStoreList is wiringList, with an older firmware's "unknown cmd" as errNoWiringStore.
func wiringStoreList(ctx context.Context, c podCommander) (*wiringListReply, error) {
	l, err := wiringList(ctx, c)
	if pe, ok := tcpclient.AsPodError(err); ok && (pe.Code == "BP0002" || strings.TrimSpace(pe.Reason) == "unknown cmd") {
		return nil, errNoWiringStore
	}
	return l, err
}

// wiringGet reads one profile in pieces; a save meanwhile (the revision moves) starts over.
func wiringGet(ctx context.Context, c podCommander, name string) (json.RawMessage, int, error) {
	for attempt := 0; attempt < 3; attempt++ {
		var buf []byte
		rev := -1
		for {
			raw, err := c.Command(ctx, map[string]any{"cmd": "wiring_get", "name": name, "offset": len(buf)})
			if err != nil {
				return nil, 0, err
			}
			var p struct {
				Rev     int    `json:"rev"`
				Total   int    `json:"total"`
				DataB64 string `json:"data_b64"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, 0, fmt.Errorf("wiring_get: %w", err)
			}
			if rev >= 0 && p.Rev != rev {
				break
			}
			rev = p.Rev
			b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(p.DataB64, "="))
			if err != nil {
				return nil, 0, fmt.Errorf("wiring_get: %w", err)
			}
			buf = append(buf, b...)
			if len(b) == 0 || len(buf) >= p.Total {
				return json.RawMessage(buf), rev, nil
			}
		}
	}
	return nil, 0, fmt.Errorf("wiring profile %s kept changing while it was read; try again", name)
}

// wiringPut stores one profile in pieces; ifRev nil = no revision check. Returns the new revision.
func wiringPut(ctx context.Context, c podCommander, name string, profile []byte, ifRev *int) (int, error) {
	var rev int
	for off := 0; off < len(profile); off += wiringPiece {
		end := min(off+wiringPiece, len(profile))
		req := map[string]any{"cmd": "wiring_put", "name": name, "offset": off, "total": len(profile),
			"data_b64": base64.RawURLEncoding.EncodeToString(profile[off:end])}
		if off == 0 && ifRev != nil {
			req["if_revision"] = *ifRev
		}
		raw, err := c.Command(ctx, req)
		if err != nil {
			return 0, err
		}
		if end == len(profile) {
			var done struct {
				Rev int `json:"rev"`
			}
			if err := json.Unmarshal(raw, &done); err != nil {
				return 0, fmt.Errorf("wiring_put: %w", err)
			}
			rev = done.Rev
		}
	}
	return rev, nil
}

// upgradeProfile turns a version 1 profile (or one without a version) into version 2 by writing
// out the reference bench's values for what it leaves out. changed says whether it was version 1.
func upgradeProfile(p map[string]any) (map[string]any, bool, error) {
	v, has := p["version"]
	n, _ := v.(float64)
	if has && n == 2 {
		return p, false, nil
	}
	if has && n != 1 {
		return nil, false, fmt.Errorf("version must be 1 or 2, got %v", v)
	}
	out := map[string]any{}
	for k, val := range referenceBench {
		out[k] = val
	}
	for k, val := range p {
		out[k] = val
	}
	out["version"] = 2
	return out, true, nil
}

func readProfileFile(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p map[string]any
	if err := json.Unmarshal(b, &p); err != nil || p == nil {
		return nil, fmt.Errorf("%s is not a JSON object (a .json wiring profile): %v", path, err)
	}
	return p, nil
}

func newWiringCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wiring",
		Short: "The wiring profiles stored on the pod: which DUT signal is on which LA channel",
		Long: "The pod keeps up to 16 named wiring profiles, one of them its default; the SDK, the MCP server,\n" +
			"the web app and these commands all see the same ones. Without a subcommand: show the default\n" +
			"profile (or --wiring-profile). A profile leaves undefined what it does not set.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWiringShow(g, g.wiringProfile, cmd.OutOrStdout())
		},
	}
	list := &cobra.Command{
		Use: "list", Short: "List the pod's wiring profiles and its default", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error { return runWiringList(g, cmd.OutOrStdout()) },
	}
	show := &cobra.Command{
		Use: "show [NAME]", Short: "Print a stored profile as JSON (default: the pod's default profile)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := g.wiringProfile
			if len(args) == 1 {
				name = args[0]
			}
			return runWiringShow(g, name, cmd.OutOrStdout())
		},
	}
	export := &cobra.Command{
		Use: "export [NAME]", Short: "Write a stored profile to stdout as a file for a repository", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := g.wiringProfile
			if len(args) == 1 {
				name = args[0]
			}
			return runWiringShow(g, name, cmd.OutOrStdout())
		},
	}
	var saveName string
	var force bool
	save := &cobra.Command{
		Use:   "save FILE",
		Short: "Store a .json profile on the pod (--name, default: the pod's default profile)",
		Long: "Store a wiring profile on the pod. A version 1 file (or one without a version) is upgraded\n" +
			"first: what it leaves out takes the reference bench's values. A new name refuses to replace a\n" +
			"profile that exists unless --force. Saving never changes which profile is the default.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWiringSave(g, args[0], saveName, force, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	save.Flags().StringVar(&saveName, "name", "", "the pod profile to store it as (default: the pod's default profile)")
	save.Flags().BoolVar(&force, "force", false, "replace a profile of that name that already exists")
	def := &cobra.Command{
		Use: "default [NAME]", Short: "Show the pod's default profile, or make NAME the default", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runWiringPodCmd(g, cmd.OutOrStdout(), map[string]any{"cmd": "wiring_default", "name": name}, name == "")
		},
	}
	del := &cobra.Command{
		Use: "delete NAME", Short: "Delete a stored profile (not the default)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWiringPodCmd(g, cmd.OutOrStdout(), map[string]any{"cmd": "wiring_delete", "name": args[0]}, false)
		},
	}
	upgrade := &cobra.Command{
		Use:   "upgrade FILE",
		Short: "Rewrite a version 1 wiring file as version 2 (the reference bench's values written out)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWiringUpgrade(args[0], cmd.OutOrStdout())
		},
	}
	cmd.AddCommand(list, show, export, save, def, del, upgrade)
	return cmd
}

func wiringTarget(g *globalFlags) (context.Context, context.CancelFunc, *podTarget, error) {
	return g.podClient("wiring", wiringTimeout)
}

func runWiringList(g *globalFlags, out io.Writer) error {
	ctx, cancel, t, err := wiringTarget(g)
	if err != nil {
		return err
	}
	defer cancel()
	l, err := wiringStoreList(ctx, t.client)
	if err != nil {
		return err
	}
	def := ""
	if l.Default != nil {
		def = *l.Default
	}
	sort.Slice(l.Profiles, func(i, j int) bool { return l.Profiles[i].Name < l.Profiles[j].Name })
	if len(l.Profiles) == 0 {
		fmt.Fprintln(out, "The pod has no wiring profiles yet: `benchpod wiring save <file> --name <board>` stores one.")
		return nil
	}
	fmt.Fprintf(out, "%-3s%-41s %5s %6s\n", "", "NAME", "REV", "BYTES")
	for _, p := range l.Profiles {
		mark := ""
		if p.Name == def {
			mark = "*"
		}
		fmt.Fprintf(out, "%-3s%-41s %5d %6d\n", mark, p.Name, p.Rev, p.Bytes)
	}
	fmt.Fprintf(out, "\n* default. %d of %d profiles, %d bytes free.\n", len(l.Profiles), l.MaxProfiles, l.FreeBytes)
	return nil
}

func runWiringShow(g *globalFlags, name string, out io.Writer) error {
	ctx, cancel, t, err := wiringTarget(g)
	if err != nil {
		return err
	}
	defer cancel()
	l, err := wiringStoreList(ctx, t.client)
	if err != nil {
		return err
	}
	if name == "" {
		if l.Default == nil {
			return fmt.Errorf("wiring: the pod has no wiring profiles yet: `benchpod wiring save <file> --name <board>` stores one")
		}
		name = *l.Default
	}
	raw, _, err := wiringGet(ctx, t.client, name)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("profile %s on the pod is not valid JSON: %w", name, err)
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(out, string(b))
	return nil
}

func runWiringSave(g *globalFlags, path, name string, force bool, out, warn io.Writer) error {
	p, err := readProfileFile(path)
	if err != nil {
		return usageErrorf("wiring save: %v", err)
	}
	p, upgraded, err := upgradeProfile(p)
	if err != nil {
		return usageErrorf("wiring save: %s: %v", path, err)
	}
	if upgraded {
		fmt.Fprintf(warn, "Note: %s is a version 1 profile; the reference bench's values were written out for what it leaves out (`benchpod wiring upgrade %s` rewrites the file).\n", path, path)
	}
	body, _ := json.Marshal(p)
	ctx, cancel, t, err := wiringTarget(g)
	if err != nil {
		return err
	}
	defer cancel()
	l, err := wiringStoreList(ctx, t.client)
	if err != nil {
		return err
	}
	if name == "" {
		name = "default"
		if l.Default != nil {
			name = *l.Default
		}
	}
	var ifRev *int
	if !force {
		exists := false
		for _, q := range l.Profiles {
			exists = exists || q.Name == name
		}
		if exists && (l.Default == nil || *l.Default != name) {
			return usageErrorf("wiring save: the pod already has a profile %s; pass --force to replace it", name)
		}
		if !exists {
			zero := 0
			ifRev = &zero
		}
	}
	rev, err := wiringPut(ctx, t.client, name, body, ifRev)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Stored %s on the pod as %s (revision %d).\n", path, name, rev)
	return nil
}

func runWiringPodCmd(g *globalFlags, out io.Writer, req map[string]any, read bool) error {
	ctx, cancel, t, err := wiringTarget(g)
	if err != nil {
		return err
	}
	defer cancel()
	if read {
		delete(req, "name")
	}
	if _, err := wiringStoreList(ctx, t.client); err != nil {
		return err
	}
	raw, err := t.client.Command(ctx, req)
	if err != nil {
		return err
	}
	var r map[string]any
	_ = json.Unmarshal(raw, &r)
	switch req["cmd"] {
	case "wiring_default":
		if d, ok := r["default"].(string); ok && d != "" {
			if read {
				fmt.Fprintf(out, "The pod's default wiring profile is %s.\n", d)
			} else {
				fmt.Fprintf(out, "%s is now the pod's default wiring profile.\n", d)
			}
		} else {
			fmt.Fprintln(out, "The pod has no wiring profiles yet.")
		}
	case "wiring_delete":
		fmt.Fprintf(out, "Deleted %v from the pod.\n", req["name"])
	}
	return nil
}

func runWiringUpgrade(path string, out io.Writer) error {
	p, err := readProfileFile(path)
	if err != nil {
		return usageErrorf("wiring upgrade: %v", err)
	}
	p, upgraded, err := upgradeProfile(p)
	if err != nil {
		return usageErrorf("wiring upgrade: %s: %v", path, err)
	}
	if !upgraded {
		fmt.Fprintf(out, "%s is already version 2.\n", path)
		return nil
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(path+".v1.bak", mustRead(path), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Rewrote %s as version 2 (the old file is %s.v1.bak).\n", path, path)
	return nil
}

func mustRead(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func newExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain CODE",
		Short: "What a BenchPod error code (BP0001...) means and how to fix it",
		Long: fmt.Sprintf("Print a BenchPod error code's entry from the catalog built into this CLI (%d codes),\n"+
			"offline. Every code is also on https://embeddedci.com/docs/errors.", errcatalog.Count()),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, ok := errcatalog.Explain(args[0])
			if !ok {
				return usageErrorf("explain: %s is not in this CLI's catalog; see %s (a newer firmware's code needs a newer CLI)",
					strings.ToUpper(args[0]), errcatalog.DocsURL(args[0]))
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			return nil
		},
	}
}

// loadPodWiringFromPod reads the pod's own profile (--wiring-profile, else its default) for the
// flags a command leaves out. ok false: the pod's firmware stores no profiles, so the caller
// falls back to embeddedci.com.
// podStoresWiring says whether the pod behind g's connection keeps its wiring profiles itself
// (firmware 3.10.8+). Then nothing is saved to embeddedci.com on the side. A variable so tests can
// replace it.
var podStoresWiring = func(g *globalFlags) bool {
	spec, err := g.resolveTarget()
	if err != nil {
		return false
	}
	if spec = g.preferUSBNetwork(spec, "wiring"); spec.IsSerial() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), wiringLookupTimeout)
	defer cancel()
	t, err := openPodTarget(ctx, spec)
	if err != nil {
		return false
	}
	_, err = wiringStoreList(ctx, t.client)
	return err == nil
}

func loadPodWiringFromPod(g *globalFlags, warn io.Writer) (*podWiring, bool) {
	spec, err := g.resolveTarget()
	if err != nil {
		return nil, false
	}
	if spec = g.preferUSBNetwork(spec, "wiring"); spec.IsSerial() {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), wiringLookupTimeout)
	defer cancel()
	t, err := openPodTarget(ctx, spec)
	if err != nil {
		return nil, false
	}
	l, err := wiringStoreList(ctx, t.client)
	if err != nil {
		return nil, false
	}
	name := g.wiringProfile
	if name == "" {
		if l.Default == nil {
			return &podWiring{}, true // the pod stores profiles but has none: nothing is wired
		}
		name = *l.Default
	}
	raw, _, err := wiringGet(ctx, t.client, name)
	if err != nil {
		fmt.Fprintf(warn, "Warning: could not read wiring profile %s from the pod (%v); using the flags only\n", name, err)
		return &podWiring{}, true
	}
	var w podWiring
	if err := json.Unmarshal(raw, &w); err != nil {
		fmt.Fprintf(warn, "Warning: wiring profile %s on the pod does not parse (%v); using the flags only\n", name, err)
		return &podWiring{}, true
	}
	return &w, true
}
