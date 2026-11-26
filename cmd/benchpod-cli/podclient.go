package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── one pod, two ways in ────────────────────────────────────────────────────
//
// A single-reply JSON command ({"cmd":"status"}) reaches the pod either on its LAN port
// (tcpclient.Client) or through embeddedci.com (cloudpod.Client, POST .../command). Both have the
// same Command method, so the commands below are written once against podCommander and the
// connection decides the way in.

// podCommander sends one JSON command to the pod and returns its reply data. A refusal by the pod
// is a *tcpclient.PodError either way.
type podCommander interface {
	Command(ctx context.Context, req map[string]any) (json.RawMessage, error)
}

var (
	_ podCommander = (*tcpclient.Client)(nil)
	_ podCommander = (*cloudpod.Client)(nil)
)

// podTarget is a resolved connection for single-reply commands.
type podTarget struct {
	spec     ConnSpec
	client   podCommander
	label    string // how messages name the pod when set: "benchpod-a1b2c3 on embeddedci.com"; "" for the address (where)
	deviceID string // the pod's id on embeddedci.com when the connection goes through it, else ""
}

// where is how messages name the pod: its label, else the address the connection went to
// ("192.168.1.5:8443" over TLS; the host alone before the first command).
func (t *podTarget) where() string {
	if t.label != "" {
		return t.label
	}
	if c, ok := t.client.(*tcpclient.Client); ok {
		return c.Shown()
	}
	return tcpclient.ShownAddr(t.spec.Addr)
}

// podClient resolves the connection for a single-reply JSON command: the pod's network address,
// or embeddedci:<name> through embeddedci.com. A USB connection gets the standard "needs the
// network" error unless the pod's USB network adapter is up. It returns a deadline context (overridable by --timeout) with signal handling.
func (g *globalFlags) podClient(cmdName string, def time.Duration) (context.Context, context.CancelFunc, *podTarget, error) {
	spec, err := g.networkTarget(cmdName)
	if err != nil {
		return nil, func() {}, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(def))
	cancel = withSignalCleanup(cancel, installSignalHandler(ctx, cancel))
	t, err := openPodTarget(ctx, spec)
	if err != nil {
		cancel()
		return nil, func() {}, nil, fmt.Errorf("%s: %w", cmdName, err)
	}
	return ctx, cancel, t, nil
}

// openPodTarget builds the client for a network or cloud ConnSpec.
func openPodTarget(ctx context.Context, spec ConnSpec) (*podTarget, error) {
	if spec.IsCloud() {
		c, err := resolveCloudPod(ctx, spec.Name)
		if err != nil {
			return nil, err
		}
		return &podTarget{spec: spec, client: c, label: c.Label(), deviceID: c.DeviceID}, nil
	}
	return &podTarget{spec: spec, client: &tcpclient.Client{Addr: spec.Addr}}, nil
}

// cloudServerURL is the embeddedci.com base URL the cloud connection uses: BENCHPOD_API_BASE (the
// same variable the Python SDK reads), else www.embeddedci.com.
func cloudServerURL() string {
	if v := strings.TrimSpace(os.Getenv("BENCHPOD_API_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultServerURL
}

// cloudOrg is --org (or BENCHPOD_ORG): the organization on embeddedci.com that cloud calls act
// in, an id, slug or name. Empty is the account's active organization.
var cloudOrg string

// cloudLeaseID is the embeddedci.com device lease a `benchpod ci` run holds ("" for none): every
// client newServerAPI makes sends it (X-Benchpod-Lease), so a flash or command the run makes
// through embeddedci.com is let through its own lease.
var cloudLeaseID string

// newServerAPI is a client for embeddedci.com at baseURL that acts in cloudOrg.
func newServerAPI(baseURL string) *serverapi.Client {
	api := serverapi.New(baseURL)
	api.Org = cloudOrg
	api.LeaseID = cloudLeaseID
	return api
}

// errNoCloudCredential is a missing sign-in for a cloud call.
var errNoCloudCredential = errors.New("embeddedci.com needs a sign-in: run `benchpod login` (or set BENCHPOD_API_KEY)")

// cloudCredential returns the credential for embeddedci.com: BENCHPOD_API_KEY when set (sent as
// "ApiKey eci_..."), else the `benchpod login` session, refreshed when needed. It never prompts.
func cloudCredential(ctx context.Context, api *serverapi.Client) (string, error) {
	if key := strings.TrimSpace(os.Getenv("BENCHPOD_API_KEY")); key != "" {
		return "ApiKey " + key, nil
	}
	tokenPath, err := resolveTokenPath("")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(tokenPath); err != nil {
		// In GitHub Actions (permissions: id-token: write), the job's OIDC token.
		if inGitHubActionsOIDC() {
			return githubActionCredential(ctx, api)
		}
		return "", errNoCloudCredential
	}
	tokens, err := ensureTokens(ctx, api, tokenPath)
	if err != nil {
		return "", err
	}
	return tokens.AccessToken, nil
}

// credentialFor is the credential for a command with its own --token-file flag: an explicit
// --token-file means that session; otherwise BENCHPOD_API_KEY when set, else the session at
// tokenPath. It logs who is signed in.
func credentialFor(ctx context.Context, api *serverapi.Client, tokenFlag, tokenPath string) (string, error) {
	if key := strings.TrimSpace(os.Getenv("BENCHPOD_API_KEY")); key != "" && strings.TrimSpace(tokenFlag) == "" {
		log.Printf("auth: BENCHPOD_API_KEY")
		return "ApiKey " + key, nil
	}
	tokens, err := ensureTokens(ctx, api, tokenPath)
	if err != nil {
		return "", err
	}
	log.Printf("auth: signed in as %s", tokens.Who())
	return tokens.AccessToken, nil
}

// resolveCloudPod finds the pod named name on the account and returns a client for it.
func resolveCloudPod(ctx context.Context, name string) (*cloudpod.Client, error) {
	api := newServerAPI(cloudServerURL())
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		return nil, err
	}
	if isGitHubActionCredential(cred) {
		// A github_action token lists no devices: the cloud routes take the pod by name, and the
		// server checks it against the repository's allowed pods.
		return &cloudpod.Client{API: api, Credential: cred, Name: strings.TrimSpace(name), ByName: true}, nil
	}
	return cloudpod.Resolve(ctx, api, cred, name)
}

// ── one wiring profile ──────────────────────────────────────────────────────
//
// The pod's wiring profiles live on the pod (current firmware; the web app, the SDK and MCP read the
// same ones): --wiring-profile, else the pod's default. Older firmware keeps one profile on
// embeddedci.com, read when this machine can: signed in (or BENCHPOD_API_KEY) and the pod on that
// account. Commands that take pins default the ones left out from it; flags always win; without a
// profile the flags are required.

// podWiring is the part of the wiring profile the CLI defaults flags from. Pins are LA channel
// numbers; nil is "not wired".
type podWiring struct {
	SwdSwclk  *int   `json:"swd_swclk"`
	SwdSwdio  *int   `json:"swd_swdio"`
	SwdNreset *bool  `json:"swd_nreset"`
	SwdTarget string `json:"swd_target"`
	// Efuse is the target-power rail the profile uses (1 internal 5 V, 2 external); nil = not set.
	Efuse *int `json:"efuse"`
	// DebugPort is "swd" or "jtag" when the profile fixes the debug port; nil = automatic.
	DebugPort *string `json:"debug_port"`
	// JTAG pins (firmware feature jtag); JtagTrst is optional.
	JtagTck  *int `json:"jtag_tck"`
	JtagTms  *int `json:"jtag_tms"`
	JtagTdi  *int `json:"jtag_tdi"`
	JtagTdo  *int `json:"jtag_tdo"`
	JtagTrst *int `json:"jtag_trst"`
	I2cSda   *int `json:"i2c_sda"`
	I2cScl   *int `json:"i2c_scl"`
	SpiSclk  *int `json:"spi_sclk"`
	SpiMosi  *int `json:"spi_mosi"`
	SpiMiso  *int `json:"spi_miso"`
	SpiCs    *int `json:"spi_cs"`
	UartRx   *int `json:"uart_rx"`
	UartTx   *int `json:"uart_tx"`
	// Monitors are the external INA current monitors (see wiring_monitors.go).
	Monitors []profileMonitor `json:"monitors"`
	// Signals are the profile's named DUT signals ("DRDY" on LA7).
	Signals []struct {
		Name string `json:"name"`
		LA   int    `json:"la"`
	} `json:"signals"`
}

// channel is the LA channel a wiring name stands for: a role ("i2c_sda", "spi_cs") or a signal
// name (case does not matter). ok is false when the profile does not have it.
func (w *podWiring) channel(name string) (int, bool) {
	if w == nil {
		return 0, false
	}
	n := strings.ToLower(strings.TrimSpace(name))
	for role, la := range map[string]*int{"swd_swclk": w.SwdSwclk, "swd_swdio": w.SwdSwdio,
		"jtag_tck": w.JtagTck, "jtag_tms": w.JtagTms, "jtag_tdi": w.JtagTdi, "jtag_tdo": w.JtagTdo, "jtag_trst": w.JtagTrst,
		"i2c_sda": w.I2cSda, "i2c_scl": w.I2cScl, "spi_sclk": w.SpiSclk, "spi_mosi": w.SpiMosi,
		"spi_miso": w.SpiMiso, "spi_cs": w.SpiCs, "uart_rx": w.UartRx, "uart_tx": w.UartTx} {
		if role == n && la != nil {
			return *la, true
		}
	}
	for _, s := range w.Signals {
		if strings.ToLower(s.Name) == n && s.LA >= 1 {
			return s.LA, true
		}
	}
	return 0, false
}

// wiringSource names where defaulted pins came from, in log lines.
const wiringSource = "the pod's wiring profile"

// wiringLookupTimeout bounds the whole profile lookup, so a slow server never holds up a command
// that has its flags anyway.
const wiringLookupTimeout = 6 * time.Second

// loadPodWiring returns the wiring profile stored on the pod behind g's connection
// (--wiring-profile, else its default), or nil when there is none to use. A variable so tests can
// replace it. (Firmware before 3.10.8 kept the profile on embeddedci.com; CLI 0.10 reads the pod's.)
var loadPodWiring = func(g *globalFlags, warn io.Writer) *podWiring {
	// Firmware that stores profiles: the pod's own (--wiring-profile, else its default).
	if w, ok := loadPodWiringFromPod(g, warn); ok {
		return w
	}
	if g.wiringProfile != "" {
		fmt.Fprintf(warn, "Warning: --wiring-profile %s: could not read the pod's wiring profiles; using the flags only\n", g.wiringProfile)
	}
	return nil
}

// cloudDeviceID is the pod's id on embeddedci.com when it is on the credential's account, else
// "". A cloud connection names it; a network one asks the pod (cloud_status) and checks the
// account's device list.
func cloudDeviceID(ctx context.Context, api *serverapi.Client, cred string, spec ConnSpec) (string, error) {
	if spec.IsCloud() {
		c, err := cloudpod.Resolve(ctx, api, cred, spec.Name)
		if err != nil {
			return "", err
		}
		return c.DeviceID, nil
	}
	raw, err := (&tcpclient.Client{Addr: spec.Addr, DialTimeout: 2 * time.Second}).Command(ctx, map[string]any{"cmd": "cloud_status"})
	if err != nil {
		return "", nil // the command itself reports an unreachable pod
	}
	var c cloudState
	if json.Unmarshal(raw, &c) != nil || !c.Configured || strings.TrimSpace(c.DeviceID) == "" {
		return "", nil
	}
	devices, err := api.ListDevices(ctx, cred)
	if err != nil {
		return "", err
	}
	for _, d := range devices {
		if d.ID == strings.TrimSpace(c.DeviceID) {
			return d.ID, nil
		}
	}
	return "", nil
}

// pinFromWiring fills *flagVal (an LA pin flag, "" = not given) from the profile pin when the
// flag was left out. It reports whether it did.
func pinFromWiring(flagVal *string, pin *int) bool {
	if strings.TrimSpace(*flagVal) != "" || pin == nil || *pin < 1 || *pin > laPinCount {
		return false
	}
	*flagVal = fmt.Sprint(*pin)
	return true
}
