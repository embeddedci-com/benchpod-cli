package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/features"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── benchpod ci ─────────────────────────────────────────────────────────────

const ciExitCodesHelp = "Exit codes:\n" +
	"  0  passed\n" +
	"  1  a file, usage or setup error (the YAML, the firmware build, no pod)\n" +
	"  2  a test failed (or errored)\n" +
	"  4  the run is not valid: the pod is in use, require is not met, the pod rebooted\n" +
	"     or was lost, the lease was lost, a capture lost data, or the run was interrupted"

func newCICmd(g *globalFlags) *cobra.Command {
	o := &ciOptions{}
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Run a CI suite from benchpod-ci.yaml: flash, boot checks, power checks, pytest and other test commands",
		Long: "Run the CI suite a benchpod-ci.yaml file defines, on one pod, and record it in a\n" +
			"run folder (run.json, junit.xml, logs, traces), uploaded to embeddedci.com at the end\n" +
			"when signed in (the file's upload:, --no-upload). `benchpod ci` is `benchpod ci run`.\n\n" +
			"The pod: --pod (a name or any --connection value), else the default connection, else\n" +
			"the one pod on the LAN or USB network link (with several, the first one not in use).\n" +
			"The run holds the pod's lease from start to end, so the web app and other sessions\n" +
			"cannot drive it meanwhile; pytest and run steps join the same lease (BENCHPOD_LEASE).\n" +
			"Cleanup (DAC stop, power off, sensors and pulls off) always runs, also after Ctrl-C.\n\n" +
			ciExitCodesHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runCICommand(g, o, cmd) },
	}
	addCIRunFlags(cmd, o)
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the suite (the same as `benchpod ci`)",
		Args:  cobra.NoArgs,
		Long:  cmd.Long,
	}
	ro := &ciOptions{}
	addCIRunFlags(run, ro)
	run.RunE = func(c *cobra.Command, _ []string) error { return runCICommand(g, ro, c) }
	cmd.AddCommand(run, newCIValidateCmd(g), newCIDiffCmd(), newCIUploadCmd(), newCIListCmd(), newCIShowCmd())
	return cmd
}

func addCIRunFlags(cmd *cobra.Command, o *ciOptions) {
	f := cmd.Flags()
	f.StringVarP(&o.file, "file", "f", ci.DefaultFile, "the suite definition")
	f.StringVar(&o.pod, "pod", "", "the pod: a name (benchpod-a1b2c3) or any --connection value (default: the default connection, else the one pod found)")
	f.StringVar(&o.firmware, "firmware", "", "flash this image instead of firmware.file, or build:<id> for a build uploaded to embeddedci.com")
	f.StringArrayVar(&o.steps, "step", nil, "run only this step id (repeatable, or comma-separated)")
	f.StringVar(&o.out, "out", "", "the run folder (default: ./embeddedci-runs/<UTC time>-<suite>)")
	f.BoolVar(&o.noUpload, "no-upload", false, "do not upload the run to embeddedci.com (the file's upload: says when it is; auto uploads when signed in)")
	f.BoolVar(&o.keepGoing, "keep-going", false, "go on after a required step fails")
	f.IntVar(&o.repeat, "repeat", 1, "run the steps N times (the firmware is flashed once); each case records its attempt")
	f.BoolVar(&o.json, "json", false, "print run.json on stdout instead of the summary")
	f.DurationVar(&o.wait, "wait", 0, "wait this long for a pod another session holds, e.g. 10m (default: fail at once with exit 4)")
}

func runCICommand(g *globalFlags, o *ciOptions, cmd *cobra.Command) error {
	if o.repeat < 1 {
		return &ciExit{code: ci.ExitSetup, msg: "--repeat must be 1 or more"}
	}
	cfg, err := ci.Load(o.file, nil)
	if err != nil {
		return &ciExit{code: ci.ExitSetup, msg: ciLoadError(o.file, err)}
	}
	steps, err := cfg.Select(o.steps)
	if err != nil {
		return &ciExit{code: ci.ExitSetup, msg: err.Error()}
	}
	stdout := cmd.OutOrStdout()
	if o.json {
		// Whatever the steps print (OpenOCD, child processes) goes to stderr; stdout is run.json.
		realStdout := os.Stdout
		os.Stdout = os.Stderr
		defer func() { os.Stdout = realStdout }()
	}
	r := &ciRunner{cfg: cfg, opts: *o, steps: steps, stdout: stdout, stderr: cmd.ErrOrStderr()}
	r.connect = func(ctx context.Context) (*ciConnected, error) { return connectCIPod(g, o.pod, cfg.Wiring) }
	r.takeLease = func(ctx context.Context, c *ciConnected, join *tcpclient.LeaseJoin) (ciLease, error) {
		return takeCILease(ctx, c, join, o.wait, r.stderr)
	}
	r.signedIn = ciSignedIn
	r.upload = func(ctx context.Context, dir string) (*ci.Uploaded, error) {
		return uploadRunFolder(ctx, dir, r.stderr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exit := r.execute(ctx)
	if exit == nil || exit.code == 0 {
		return nil
	}
	return exit
}

func ciLoadError(path string, err error) string {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Sprintf("%s: no such file. Write a %s next to the tests (see the README's `benchpod ci` section), or pass -f <file>", path, ci.DefaultFile)
	}
	return err.Error()
}

// connectCIPod picks the pod and connects to it.
func connectCIPod(g *globalFlags, podFlag, wiring string) (*ciConnected, error) {
	t, err := resolveCIPod(g, podFlag)
	if err != nil {
		return nil, err
	}
	gc := *g
	gc.wiringProfile = wiring
	c := &ciConnected{target: t}
	if t.Spec.IsCloud() {
		gc.connection = cloudConnPrefix + t.Spec.Name
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cp, err := resolveCloudPod(ctx, t.Spec.Name)
		if err != nil {
			return nil, err
		}
		c.pod = &cloudPod{g: &gc, client: cp}
		c.conn = cloudConnPrefix + cp.Name
		if c.target.Name == "" {
			c.target.Name = cp.Name
		}
		return c, nil
	}
	gc.connection = t.Spec.Addr
	c.pod = &networkPod{g: &gc, client: &tcpclient.Client{Addr: t.Spec.Addr}, usbNet: isUSBLinkAddr(t.Spec.Addr)}
	c.conn, c.hwAddr = t.Spec.Addr, t.Spec.Addr
	return c, nil
}

// takeCILease takes the pod's lease (LAN, USB network) or the server's (embeddedci.com).
func takeCILease(ctx context.Context, c *ciConnected, join *tcpclient.LeaseJoin, wait time.Duration, warn io.Writer) (ciLease, error) {
	switch p := c.pod.(type) {
	case *cloudPod:
		api := p.client.API
		lease, err := acquireCloudLease(ctx, api, p.client.Credential, p.client.Name, join.ID, wait, ciLeaseInterval, warn)
		if err != nil {
			return nil, err
		}
		if lease.Supported() {
			api.LeaseID = join.ID // every cloud command carries it (X-Benchpod-Lease)
		}
		return lease, nil
	case *networkPod:
		own := &tcpclient.Client{Addr: p.client.Addr, NoLeaseJoin: true}
		st, serr := own.Command(ctx, features.StatusRequest())
		has, form := leaseFeature(st, serr)
		if has == leaseNo {
			fmt.Fprintf(warn, "Warning: this pod's firmware has no lease (its status says %s; the lease needs features_rev %d): "+
				"the run goes on without one, so the web app or another session could use the pod meanwhile. "+
				"Update the firmware with `benchpod flash-self`.\n", form, leaseFeatureRev())
			return &localLease{leaseState: leaseState{id: join.ID}, cmd: own, join: join}, nil
		}
		// leaseYes, or unknown (a status without features_rev): the pod's answer to lease_acquire
		// decides, and old firmware answers unknown command.
		l, err := acquireLocalLease(ctx, own, join, wait, ciLeaseInterval, warn)
		if err == nil && l.Supported() {
			fmt.Fprintf(warn, "[benchpod ci] lease: %s held (status: %s)\n", join.ID, form)
		} else if err == nil {
			fmt.Fprintf(warn, "[benchpod ci] lease: none (status: %s)\n", form)
		}
		return l, err
	}
	return nil, fmt.Errorf("unknown pod type %T", c.pod)
}

// leaseHas is what a pod's status says about the lease feature.
type leaseHas int

const (
	leaseUnknown leaseHas = iota // no features_rev (the caps-list form, or no status)
	leaseNo
	leaseYes
)

// leaseFeature decides the lease feature from a {"cmd":"status","caps":false} reply: with
// features_rev the embedded features table decides; without it (a pod that sent the frozen caps
// list) it is unknown. form says which reply it was, for the log.
func leaseFeature(st json.RawMessage, err error) (leaseHas, string) {
	if err != nil {
		return leaseUnknown, "no status (" + err.Error() + ")"
	}
	var v struct {
		FeaturesRev *int     `json:"features_rev"`
		Caps        []string `json:"caps"`
	}
	if json.Unmarshal(st, &v) != nil {
		return leaseUnknown, "an unreadable status"
	}
	if v.FeaturesRev == nil {
		if v.Caps != nil {
			return leaseUnknown, `the caps list (no features_rev: the pod ignored "caps":false)`
		}
		return leaseUnknown, "no features_rev"
	}
	form := fmt.Sprintf("features_rev %d", *v.FeaturesRev)
	if _, known := features.Lookup("lease"); !known {
		return leaseUnknown, form
	}
	if ok, err := features.Has(st, "lease"); err == nil && ok {
		return leaseYes, form
	}
	return leaseNo, form
}

func leaseFeatureRev() int {
	e, _ := features.Lookup("lease")
	return e.Rev
}

// ── benchpod ci validate ────────────────────────────────────────────────────

func newCIValidateCmd(g *globalFlags) *cobra.Command {
	var file, pod string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check benchpod-ci.yaml (and with --pod, the pod's firmware and wiring profile) without driving any hardware",
		Long: "Parse and check the suite definition: keys, step kinds, durations, regexes, expect\n" +
			"metrics, ${VAR} references, the firmware file, and that `import embeddedci` works for\n" +
			"pytest steps. With --pod it also reads the pod's status and its stored wiring profiles\n" +
			"(read only: nothing is powered, flashed or leased) and checks require.pod_firmware and\n" +
			"the wiring profile.\n\n" + ciExitCodesHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCIValidate(g, file, pod, asJSON, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", ci.DefaultFile, "the suite definition")
	cmd.Flags().StringVar(&pod, "pod", "", "also check this pod (read only): its firmware against require, and the wiring profile")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

type ciValidateResult struct {
	File     string   `json:"file"`
	Suite    string   `json:"suite"`
	DUT      string   `json:"dut"`
	Steps    []string `json:"steps"`
	Problems []string `json:"problems"`
	Notes    []string `json:"notes"`
	Valid    bool     `json:"valid"`
}

func runCIValidate(g *globalFlags, file, podFlag string, asJSON bool, out io.Writer) error {
	res := ciValidateResult{File: file, Problems: []string{}, Notes: []string{}, Steps: []string{}}
	code := 0
	cfg, err := ci.Load(file, nil)
	if err != nil {
		res.Problems = append(res.Problems, strings.Split(ciLoadError(file, err), "\n")...)
		code = ci.ExitSetup
	} else {
		res.Suite, res.DUT = cfg.Suite, cfg.DUT
		for _, s := range cfg.Steps {
			res.Steps = append(res.Steps, s.ID+" ("+strings.ReplaceAll(s.Kind, "_", " ")+")")
		}
		if fw := cfg.Firmware; fw != nil && fw.File != "" {
			if _, err := os.Stat(cfg.Resolve(fw.File)); err != nil {
				if fw.Build != "" {
					res.Notes = append(res.Notes, fmt.Sprintf("firmware.file %s does not exist yet; firmware.build makes it", fw.File))
				} else {
					res.Problems = append(res.Problems, fmt.Sprintf("firmware.file %s: %v", fw.File, err))
					code = ci.ExitSetup
				}
			}
		}
		checkedPy := map[string]bool{}
		for _, s := range cfg.Steps {
			if s.Kind != ci.KindPytest {
				continue
			}
			py := pythonCommand(s.Pytest)
			key := strings.Join(py, " ")
			if checkedPy[key] {
				continue
			}
			checkedPy[key] = true
			if v, err := checkEmbeddedci(context.Background(), py, cfg.Dir, mergeEnv(os.Environ(), cfg.Env)); err != nil {
				res.Problems = append(res.Problems, "step "+s.ID+": "+err.Error())
				code = ci.ExitSetup
			} else {
				res.Notes = append(res.Notes, fmt.Sprintf("%s has embeddedci %s", key, firstNonEmptyStr(v, "(version unknown)")))
			}
		}
		if strings.TrimSpace(podFlag) != "" {
			if c := validatePod(g, cfg, podFlag, &res); c > code {
				code = c
			}
		}
	}
	res.Valid = code == 0
	if asJSON {
		data, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(out, string(data))
	} else {
		if res.Suite != "" {
			fmt.Fprintf(out, "%s: suite %s, dut %s, %d steps: %s\n", file, res.Suite, res.DUT, len(res.Steps), strings.Join(res.Steps, ", "))
		}
		for _, n := range res.Notes {
			fmt.Fprintf(out, "  note: %s\n", n)
		}
		for _, p := range res.Problems {
			fmt.Fprintf(out, "  problem: %s\n", p)
		}
		if res.Valid {
			fmt.Fprintln(out, "ok")
		}
	}
	if code != 0 {
		return &ciExit{code: code}
	}
	return nil
}

// validatePod reads the pod's status and wiring profiles (read only) and checks them.
func validatePod(g *globalFlags, cfg *ci.Config, podFlag string, res *ciValidateResult) int {
	c, err := connectCIPod(g, podFlag, cfg.Wiring)
	if err != nil {
		res.Problems = append(res.Problems, "pod: "+err.Error())
		return ci.ExitSetup
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if np, ok := c.pod.(*networkPod); ok {
		np.client.NoLeaseJoin = true
	}
	st, _, err := readPodStatus(ctx, c.pod)
	if err != nil {
		res.Problems = append(res.Problems, "pod status: "+err.Error())
		return ci.ExitSetup
	}
	code := 0
	res.Notes = append(res.Notes, fmt.Sprintf("pod %s (%s): firmware %s, gateware %d", firstNonEmptyStr(c.target.Name, c.conn), c.pod.Transport(), st.Version, st.Gateware))
	if rng := cfg.Require.Range; len(rng) > 0 && !rng.Allows(st.Version) {
		res.Problems = append(res.Problems, fmt.Sprintf("the pod runs firmware %s, which does not meet require.pod_firmware %q", st.Version, cfg.Require.PodFirmware))
		code = ci.ExitInvalid
	}
	if held, holder := st.held(); held {
		res.Notes = append(res.Notes, "the pod is in use now by "+firstNonEmptyStr(holder, "another session"))
	}
	if cfg.Wiring != "" {
		l, err := wiringStoreList(ctx, c.pod)
		switch {
		case err != nil:
			res.Notes = append(res.Notes, "wiring: "+err.Error())
		default:
			found := false
			for _, p := range l.Profiles {
				found = found || p.Name == cfg.Wiring
			}
			if !found {
				res.Problems = append(res.Problems, fmt.Sprintf("the pod has no wiring profile %q", cfg.Wiring))
				code = max(code, ci.ExitSetup)
			}
		}
	}
	for _, s := range cfg.Steps {
		if (s.Kind == ci.KindUARTExpect || s.Kind == ci.KindMeasurePower || s.Kind == ci.KindPowerCycle && s.PowerCycle.Re != nil) && c.pod.Transport() == "cloud" {
			res.Problems = append(res.Problems, "step "+s.ID+": "+errNeedsDirectLink.Error())
			code = max(code, ci.ExitSetup)
		}
	}
	return code
}

// ── benchpod ci diff ────────────────────────────────────────────────────────

// ciCommandExit maps the error of a `benchpod ci` command to its exit code: the ci codes, with a
// usage error as 1 (file/usage/setup) instead of the CLI's usual 2.
func ciCommandExit(err error) (int, bool) {
	var ce *ciExit
	if errors.As(err, &ce) {
		return ce.code, true
	}
	return 0, false
}

// isCICommand says cmd is `benchpod ci` or one of its subcommands.
func isCICommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "ci" && c.Parent() != nil && !c.Parent().HasParent() {
			return true
		}
	}
	return false
}
