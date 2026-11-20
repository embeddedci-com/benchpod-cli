package tcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// ── client API level ────────────────────────────────────────────────────────
//
// The pod announces its client API level ("api") and the lowest it takes ("min_client_api") in
// status (benchpod-firmware docs/API.md, Client compatibility). The level goes up only for a change
// an older client would misread, so this CLI stops when a pod announces a level above ClientAPI,
// and every JSON command says which level it was built for ("client_api") and who sent it
// ("client"). Firmware up to 3.10.13 announces no api, which counts as 1.
//
// This CLI speaks level 2 (firmware 4.0) only: a pod below MinPodAPI is refused too (it would
// misread the 4.0 names and units), except by the commands that look at or update the firmware
// (SetAllowAnyPodAPI), so such a pod can still be upgraded.

// ClientAPI is the highest pod API level this CLI handles: the "api" of the features.json it embeds
// (a test checks that).
const ClientAPI = 2

// MinPodAPI is the lowest pod API level this CLI drives: firmware 4.0.
const MinPodAPI = 2

// FirmwareUpdatePath is how to bring a pod up to firmware 4.0.
const FirmwareUpdatePath = "this pod needs firmware 4.0: benchpod flash-self"

// CodeClientTooOld is the pod refusing a client below its min_client_api.
const CodeClientTooOld = "BP0030"

// UpdatePath is how to get a newer CLI (README, Installation).
const UpdatePath = "brew upgrade --cask benchpod, or download the latest release from " +
	"https://github.com/embeddedci-com/benchpod-cli/releases/latest"

// maxClientName is the firmware's limit on "client": longer names are left out.
const maxClientName = 32

var clientName atomic.Value // string

// clientFieldsOff: commands go without client_api and client (SetClientFields).
var clientFieldsOff atomic.Bool

// SetClientFields turns the client_api and client fields on (the default) or off. Off is for tests
// that compare whole requests; the pod treats a command without them as client API 0.
func SetClientFields(on bool) { clientFieldsOff.Store(!on) }

func init() { clientName.Store("cli dev") }

// SetClientName sets the "client" every command carries ("cli 0.9.5"). The firmware takes at most
// 32 printable ASCII characters, so a longer name is cut and other characters are dropped.
func SetClientName(name string) {
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r < 0x7f && b.Len() < maxClientName {
			b.WriteRune(r)
		}
	}
	clientName.Store(strings.TrimSpace(b.String()))
}

// ClientName is the "client" every command carries.
func ClientName() string { return clientName.Load().(string) }

// tagRequest is req as it goes on the wire: with "client_api" and "client", and the lease set
// with SetLeaseJoin unless skipLease (a copy; req is not changed). Fields req has already are kept.
func tagRequest(req map[string]any, skipLease bool) map[string]any {
	out := make(map[string]any, len(req)+3)
	for k, v := range req {
		out[k] = v
	}
	if !clientFieldsOff.Load() {
		if _, has := out["client_api"]; !has {
			out["client_api"] = ClientAPI
		}
		if name := ClientName(); name != "" {
			if _, has := out["client"]; !has {
				out["client"] = name
			}
		}
	}
	if j := leaseJoin.Load(); j != nil && !skipLease {
		if _, has := out["lease"]; !has {
			out["lease"] = j.ID
		}
	}
	return out
}

// TagRequest is req with "client_api" and "client" (not the lease: that is this package's), for a
// JSON command that goes to the pod another way: embeddedci.com's command channel.
func TagRequest(req map[string]any) map[string]any { return tagRequest(req, true) }

// UpdateRequiredError is a pod whose firmware needs a newer CLI: it announces an API level above
// ClientAPI, or it refused the command with BP0030. It is not a *PodError: no hint or fallback
// applies, and the CLI exits with its own code.
type UpdateRequiredError struct {
	Firmware     string // the pod's firmware version, "" when not known (BP0030)
	PodAPI       int    // the pod's client API level
	MinClientAPI int    // the lowest level it takes
	// Refusal is the pod's BP0030 refusal, nil when the CLI stopped on the status announcement.
	Refusal *PodError
}

func (e *UpdateRequiredError) Error() string {
	fw := "this pod's firmware"
	if e.Firmware != "" {
		fw += " " + e.Firmware
	}
	return fmt.Sprintf("%s needs a newer benchpod CLI (pod API %d, this CLI handles %d): %s",
		fw, e.PodAPI, ClientAPI, UpdatePath)
}

func updateRequiredFromRefusal(pe *PodError) *UpdateRequiredError {
	e := &UpdateRequiredError{Refusal: pe}
	e.PodAPI, _ = intArg(pe.Args, "api")
	e.MinClientAPI, _ = intArg(pe.Args, "min_client_api")
	if e.PodAPI < e.MinClientAPI {
		e.PodAPI = e.MinClientAPI
	}
	return e
}

func intArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}

// IsStatus says req is the status command, whose reply announces the pod's API level.
func IsStatus(req map[string]any) bool {
	cmd, _ := req["cmd"].(string)
	return cmd == "status"
}

// FirmwareTooOldError is a pod whose firmware is older than this CLI drives: it announces an API
// level below MinPodAPI (firmware 3.x announces none: level 1). Only the firmware commands
// (status, flash-self, install-blobs, dfu, bootsel, discover, identity) still run against it.
type FirmwareTooOldError struct {
	Firmware string // the pod's firmware version, "" when not known
	PodAPI   int    // the pod's client API level
}

func (e *FirmwareTooOldError) Error() string {
	fw := "this pod's firmware"
	if e.Firmware != "" {
		fw += " " + e.Firmware
	}
	return fmt.Sprintf("%s is too old for this benchpod CLI (pod API %d, this CLI needs %d): %s",
		fw, e.PodAPI, MinPodAPI, FirmwareUpdatePath)
}

// CheckPodAPI checks a status reply's API level: an *UpdateRequiredError when the pod announces
// an "api" (or a "min_client_api") above ClientAPI, a *FirmwareTooOldError when it is below
// MinPodAPI, nil otherwise. A reply without "api" (firmware 3.10.13 and older) is level 1; a
// reply that does not parse is left to its reader.
func CheckPodAPI(statusReply json.RawMessage) error {
	var st struct {
		Version      string `json:"version"`
		API          *int   `json:"api"`
		MinClientAPI int    `json:"min_client_api"`
	}
	if json.Unmarshal(statusReply, &st) != nil {
		return nil
	}
	api := 1
	if st.API != nil {
		api = *st.API
	}
	if api < MinPodAPI {
		return &FirmwareTooOldError{Firmware: st.Version, PodAPI: api}
	}
	if api <= ClientAPI && st.MinClientAPI <= ClientAPI {
		return nil
	}
	return &UpdateRequiredError{Firmware: st.Version, PodAPI: max(api, st.MinClientAPI), MinClientAPI: st.MinClientAPI}
}

// anyPodAPIAllowed turns the API check off for the commands that must work against firmware of
// any level: showing the status, updating or rolling back the firmware and blobs, DFU, discovery,
// the identity. Set before each command runs (SetAllowAnyPodAPI).
var anyPodAPIAllowed atomic.Bool

// SetAllowAnyPodAPI lets status replies through (true) whatever the pod's API level, and skips
// the probe before other commands; false checks them again.
func SetAllowAnyPodAPI(allow bool) { anyPodAPIAllowed.Store(allow) }

// AnyPodAPIAllowed says whether SetAllowAnyPodAPI(true) is in effect.
func AnyPodAPIAllowed() bool { return anyPodAPIAllowed.Load() }

// checkReply is CheckPodAPI on a status command's reply, nil for any other command and when
// SetAllowAnyPodAPI(true) is in effect.
func checkReply(req map[string]any, data json.RawMessage) error {
	if !IsStatus(req) || AnyPodAPIAllowed() {
		return nil
	}
	return CheckPodAPI(data)
}

// ── the API probe ───────────────────────────────────────────────────────────
//
// Firmware 3.x ignores client_api, so it would run a 4.0 command with names and units it reads
// differently. Before the first other command to an address, the client reads status once and
// checks the level (cached for the process). The CLI turns the probe on (SetAPIProbe); a status
// command is checked on its own reply.

var apiProbeOn atomic.Bool

// SetAPIProbe turns the status probe before a pod's first command on or off (the default).
func SetAPIProbe(on bool) { apiProbeOn.Store(on) }

// APIProbe says whether the probe is on, and the command at hand takes only a pod of this
// CLI's level (a transport other than this package's, embeddedci.com, probes when it is).
func APIProbe(req map[string]any) bool {
	return apiProbeOn.Load() && !AnyPodAPIAllowed() && !IsStatus(req)
}

var (
	probeMu  sync.Mutex
	probeRes = map[string]error{} // address -> the probe's verdict (nil: the level is fine)
)

// ResetAPIProbe forgets every probe's verdict (tests, and a pod that was updated meanwhile).
func ResetAPIProbe() {
	probeMu.Lock()
	defer probeMu.Unlock()
	probeRes = map[string]error{}
}

// probePodAPI checks the pod's API level before req, once per address: nil when the level is
// fine, the probe is off, req is status itself or the command takes any level.
func (c *Client) probePodAPI(ctx context.Context, req map[string]any) error {
	if !APIProbe(req) {
		return nil
	}
	addr := strings.TrimSpace(c.Addr)
	probeMu.Lock()
	verdict, done := probeRes[addr]
	probeMu.Unlock()
	if done {
		return verdict
	}
	probe := &Client{Addr: c.Addr, DialTimeout: c.DialTimeout, TLS: c.TLS, NoLeaseJoin: true}
	data, err := probe.Command(ctx, map[string]any{"cmd": "status", "caps": false})
	var upd *UpdateRequiredError
	var old *FirmwareTooOldError
	switch {
	case errors.As(err, &upd) || errors.As(err, &old):
		verdict = err
	case err != nil:
		if _, refused := AsPodError(err); refused {
			return nil // status always runs; a refusal here is the command's to report
		}
		return err // the pod is not reachable: the command would fail the same way
	default:
		verdict = CheckPodAPI(data)
	}
	probeMu.Lock()
	probeRes[addr] = verdict
	probeMu.Unlock()
	return verdict
}
