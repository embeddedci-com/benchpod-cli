package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
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
	label    string // how messages name the pod: "192.168.1.5:8080" or "benchpod-a1b2c3 on embeddedci.com"
	deviceID string // the pod's id on embeddedci.com when the connection goes through it, else ""
}

// podClient resolves the connection for a single-reply JSON command: the pod's network address,
// or embeddedci:<name> through embeddedci.com. A USB connection gets the standard "needs the
// network" error. It returns a deadline context (overridable by --timeout) with signal handling.
func (g *globalFlags) podClient(cmdName string, def time.Duration) (context.Context, context.CancelFunc, *podTarget, error) {
	spec, err := g.resolveTarget()
	if err != nil {
		return nil, func() {}, nil, err
	}
	if spec.IsSerial() {
		return nil, func() {}, nil, spec.RequireNetwork(cmdName)
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
	return &podTarget{spec: spec, client: &tcpclient.Client{Addr: spec.Addr}, label: spec.Addr}, nil
}

// cloudServerURL is the embeddedci.com base URL the cloud connection uses: BENCHPOD_API_BASE (the
// same variable the Python SDK reads), else www.embeddedci.com.
func cloudServerURL() string {
	if v := strings.TrimSpace(os.Getenv("BENCHPOD_API_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultServerURL
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
	api := serverapi.New(cloudServerURL())
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		return nil, err
	}
	return cloudpod.Resolve(ctx, api, cred, name)
}

// ── one wiring profile ──────────────────────────────────────────────────────
//
// The pod's wiring profile lives on embeddedci.com (the web app's Wiring tab, the SDK and MCP
// read it). Commands that take pins default the ones left out from it when this machine can read
// it: signed in (or BENCHPOD_API_KEY) and the pod on that account. Flags always win; without a
// profile the command behaves as before (the flags are required).

// podWiring is the part of the wiring profile the CLI defaults flags from. Pins are LA channel
// numbers; nil is "not wired".
type podWiring struct {
	SwdSwclk  *int   `json:"swd_swclk"`
	SwdSwdio  *int   `json:"swd_swdio"`
	SwdNreset *bool  `json:"swd_nreset"`
	SwdTarget string `json:"swd_target"`
	SpiSclk   *int   `json:"spi_sclk"`
	SpiMosi   *int   `json:"spi_mosi"`
	SpiMiso   *int   `json:"spi_miso"`
	SpiCs     *int   `json:"spi_cs"`
}

// wiringSource names where defaulted pins came from, in log lines.
const wiringSource = "the pod's wiring profile on embeddedci.com"

// wiringLookupTimeout bounds the whole profile lookup, so a slow server never holds up a command
// that has its flags anyway.
const wiringLookupTimeout = 6 * time.Second

// loadPodWiring returns the wiring profile of the pod behind g's connection, or nil when there is
// none to use: over USB, without a sign-in, for a pod not on the account, or when the lookup
// fails (then it writes one warning line to warn). A variable so tests can replace it.
var loadPodWiring = func(g *globalFlags, warn io.Writer) *podWiring {
	spec, err := g.resolveTarget()
	if err != nil || spec.IsSerial() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), wiringLookupTimeout)
	defer cancel()
	api := serverapi.New(cloudServerURL())
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		if !errors.Is(err, errNoCloudCredential) {
			fmt.Fprintf(warn, "Warning: could not read the pod's wiring profile from embeddedci.com (%v); using the flags only\n", err)
		}
		return nil
	}
	id, err := cloudDeviceID(ctx, api, cred, spec)
	if err != nil {
		fmt.Fprintf(warn, "Warning: could not read the pod's wiring profile from embeddedci.com (%v); using the flags only\n", err)
		return nil
	}
	if id == "" {
		log.Printf("wiring: the pod is not on the signed-in account; using the flags only")
		return nil
	}
	raw, err := api.GetDeviceWiringProfile(ctx, cred, id)
	var apiErr *serverapi.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		raw, err = api.GetDeviceWiring(ctx, cred, id) // a server without the profile route
	}
	if err != nil {
		fmt.Fprintf(warn, "Warning: could not read the pod's wiring profile from embeddedci.com (%v); using the flags only\n", err)
		return nil
	}
	b, _ := json.Marshal(raw)
	var w podWiring
	if err := json.Unmarshal(b, &w); err != nil {
		fmt.Fprintf(warn, "Warning: the pod's wiring profile on embeddedci.com does not parse (%v); using the flags only\n", err)
		return nil
	}
	return &w
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
