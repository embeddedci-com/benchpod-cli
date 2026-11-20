package tcpclient

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// ── client API level ────────────────────────────────────────────────────────
//
// The pod announces its client API level ("api") and the lowest it takes ("min_client_api") in
// status (benchpod-firmware docs/API.md, Client compatibility). The level goes up only for a change
// an older client would misread, so this CLI stops when a pod announces a level above ClientAPI,
// and every JSON command says which level it was built for ("client_api") and who sent it
// ("client"). Older firmware ignores both fields and announces no api, which counts as 1.

// ClientAPI is the highest pod API level this CLI handles: the "api" of the features.json it embeds
// (a test checks that).
const ClientAPI = 1

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

// CheckPodAPI checks a status reply's API level: an *UpdateRequiredError when the pod announces
// an "api" (or a "min_client_api") above ClientAPI, nil otherwise. A reply without "api"
// (firmware 3.10.13 and older) is level 1; a reply that does not parse is left to its reader.
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
	if api <= ClientAPI && st.MinClientAPI <= ClientAPI {
		return nil
	}
	return &UpdateRequiredError{Firmware: st.Version, PodAPI: max(api, st.MinClientAPI), MinClientAPI: st.MinClientAPI}
}

// newerPodAllowed turns the status check off for the commands that must work against firmware
// this CLI is too old for: showing the status, updating or rolling back the firmware and blobs,
// DFU, discovery. Set before each command runs (SetAllowNewerPod).
var newerPodAllowed atomic.Bool

// SetAllowNewerPod lets status replies through (true) even when the pod's API level is above
// ClientAPI, or checks them again (false).
func SetAllowNewerPod(allow bool) { newerPodAllowed.Store(allow) }

// NewerPodAllowed says whether SetAllowNewerPod(true) is in effect.
func NewerPodAllowed() bool { return newerPodAllowed.Load() }

// checkReply is CheckPodAPI on a status command's reply, nil for any other command and when
// SetAllowNewerPod(true) is in effect.
func checkReply(req map[string]any, data json.RawMessage) error {
	if !IsStatus(req) || NewerPodAllowed() {
		return nil
	}
	return CheckPodAPI(data)
}
