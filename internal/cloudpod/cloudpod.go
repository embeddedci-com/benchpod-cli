// Package cloudpod sends the pod's single-reply JSON commands through embeddedci.com instead of
// its LAN port: POST /api/benchpod/devices/{id}/command carries the same {"cmd": ...} map the pod
// takes over TCP, and the server relays it over the pod's own cloud link. Client has the same
// Command method as tcpclient.Client, so a command written against that method runs over either.
//
// Only single-reply commands go this way. Chunked replies (capture, stream, measure, test) and
// raw byte streams (SWD, SPI sessions, uploads) do not: the server refuses them on this channel.
package cloudpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// maxTimeout is the server's cap on how long it waits for the pod.
const maxTimeout = 120 * time.Second

// Client is one pod on embeddedci.com.
type Client struct {
	API        *serverapi.Client
	Credential string // a `benchpod login` access token, or "ApiKey eci_..." (see serverapi.Authorization)
	DeviceID   string // the pod's id on embeddedci.com
	Name       string // the pod's name on embeddedci.com, for messages
	// ByName: the credential is a cloud token (GitHub Actions), which reaches the pod by name on
	// the /api/cloud/devices routes instead of by id on the user's device routes.
	ByName bool
}

// Resolve finds the pod named name (or with that id) among the devices the credential can see.
func Resolve(ctx context.Context, api *serverapi.Client, credential, name string) (*Client, error) {
	name = strings.TrimSpace(name)
	devices, err := api.ListDevices(ctx, credential)
	if err != nil {
		return nil, fmt.Errorf("list your pods on embeddedci.com: %w", authHint(err))
	}
	for _, d := range devices {
		if d.Name == name {
			return &Client{API: api, Credential: credential, DeviceID: d.ID, Name: d.Name}, nil
		}
	}
	for _, d := range devices {
		if d.ID == name {
			return &Client{API: api, Credential: credential, DeviceID: d.ID, Name: d.Name}, nil
		}
	}
	names := make([]string, 0, len(devices))
	for _, d := range devices {
		names = append(names, d.Name)
	}
	where, hint := OrgHint(ctx, api, credential, func(d serverapi.DeviceResponse) bool {
		return d.Name == name || d.ID == name
	})
	if len(names) == 0 {
		return nil, fmt.Errorf("no pod named %q on embeddedci.com: %s has no pods%s", name, where, hint)
	}
	return nil, fmt.Errorf("no pod named %q on embeddedci.com (%s has: %s)%s", name, where, strings.Join(names, ", "), hint)
}

// OrgHint says, for a pod that is not among the devices api lists, which organization was
// searched (where: `organization "Acme" (acme)`, or "this account" when the server does not say)
// and, in hint, how to reach the pod: the --org of another of the user's organizations that has
// a device match accepts, else how to pick another organization. It is best effort: a failing
// lookup only makes the message plainer.
func OrgHint(ctx context.Context, api *serverapi.Client, credential string, match func(serverapi.DeviceResponse) bool) (where, hint string) {
	where = "this account"
	orgs, err := api.ListOrganizations(ctx, credential)
	if err != nil || len(orgs.Organizations) == 0 {
		return where, "; for a pod in another organization pass --org <id|slug|name> (or set BENCHPOD_ORG)"
	}
	searched, ok := orgs.Active()
	if sel := strings.TrimSpace(api.Org); sel != "" {
		searched, err = orgs.Find(sel)
		ok = err == nil
	}
	if ok {
		where = "organization " + searched.Label()
	}
	var found, others []string
	unsure := false
	for _, org := range orgs.Organizations {
		if ok && org.ID == searched.ID {
			continue
		}
		others = append(others, orgSelector(org))
		devices, err := api.WithOrg(org.ID).ListDevices(ctx, credential)
		if err != nil {
			unsure = true // an API key, for one, sees its own organization only
			continue
		}
		for _, d := range devices {
			if match(d) {
				found = append(found, orgSelector(org))
				break
			}
		}
	}
	switch {
	case len(found) == 1:
		return where, fmt.Sprintf("; it is in your organization %s: pass --org %s (or set BENCHPOD_ORG=%s)", found[0], found[0], found[0])
	case len(found) > 1:
		return where, fmt.Sprintf("; it is in your organizations %s: pick one with --org (or BENCHPOD_ORG)", strings.Join(found, ", "))
	case len(others) > 0 && unsure:
		return where, fmt.Sprintf("; for a pod in another of your organizations (%s) pass --org (or set BENCHPOD_ORG)", strings.Join(others, ", "))
	case len(others) > 0:
		return where, fmt.Sprintf("; your other organizations (%s) do not have it either", strings.Join(others, ", "))
	}
	return where, ""
}

// orgSelector is the --org value that names org: its slug, else its id.
func orgSelector(org serverapi.Organization) string {
	if org.Slug != "" {
		return org.Slug
	}
	return org.ID
}

// Command sends req to the pod and returns its reply data. A refusal by the pod comes back as a
// *tcpclient.PodError, exactly like over the LAN; a failure on the way (offline, lease, timeout,
// role) as an *Error.
func (c *Client) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	timeout := time.Duration(0)
	if dl, ok := ctx.Deadline(); ok {
		// Leave the HTTP round trip a moment, so the server's answer arrives before ctx expires.
		timeout = time.Until(dl) - time.Second
		if timeout < time.Second {
			timeout = time.Second
		}
		if timeout > maxTimeout {
			timeout = maxTimeout
		}
	}
	var resp *serverapi.CommandResponse
	var err error
	if c.ByName {
		resp, err = c.API.CloudDeviceCommand(ctx, c.Credential, c.Name, req, timeout)
	} else {
		resp, err = c.API.DeviceCommand(ctx, c.Credential, c.DeviceID, req, timeout)
	}
	if err != nil {
		return nil, c.Explain(ctx, err)
	}
	if strings.TrimSpace(resp.Status) == "error" {
		msg := strings.TrimSpace(resp.Error)
		if msg == "" {
			msg = "pod returned an error"
		}
		return nil, &tcpclient.PodError{Reason: msg, Code: resp.Code, Args: resp.Args}
	}
	return resp.Data, nil
}

// Explain turns a failed call about this pod into an error that says what happened: an *Error for
// an answer from embeddedci.com (offline, lease, role, scope), ctx's error when it ended, else
// the transport failure.
func (c *Client) Explain(ctx context.Context, err error) error {
	var apiErr *serverapi.APIError
	if errors.As(err, &apiErr) {
		return &Error{Name: c.Name, Status: apiErr.Status, Code: apiErr.Code, Message: apiErr.Message}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fmt.Errorf("embeddedci.com: %w", err)
}

// Label is how messages name the pod: "benchpod-a1b2c3 on embeddedci.com".
func (c *Client) Label() string { return c.Name + " on embeddedci.com" }

// Error is embeddedci.com failing to get a command to the pod or its reply back. It is not a
// refusal by the pod. errors.Is(err, tcpclient.ErrUnreachable) holds when the pod is offline, and
// a held lease unwraps to a busy *tcpclient.PodError, so exit codes match the LAN's.
type Error struct {
	Name    string // the pod
	Status  int    // HTTP status
	Code    string // server code: lease_held, device_offline, ...
	Message string // server message
}

func (e *Error) Error() string {
	switch {
	case e.Code == "lease_held":
		return fmt.Sprintf("%s is in use: another session (the web app, a CI job or the SDK) holds its lease on embeddedci.com; try again when it ends", e.Name)
	case e.Code == "device_busy":
		return fmt.Sprintf("%s is busy with another operation (a flash, capture, terminal or firmware update); try again when it ends", e.Name)
	case e.Code == "device_offline":
		return fmt.Sprintf("%s is offline: it is not connected to embeddedci.com (check its network, or reach it on the LAN or over USB)", e.Name)
	case e.Code == "device_reconnecting":
		return fmt.Sprintf("%s is reconnecting to embeddedci.com; try again in a few seconds", e.Name)
	case e.Code == "device_timeout" || e.Status == http.StatusGatewayTimeout:
		return fmt.Sprintf("%s did not answer through embeddedci.com in time", e.Name)
	case e.Code == "not_capable":
		return fmt.Sprintf("%s cannot take commands through embeddedci.com: its firmware is too old; update it (web app or `benchpod flash-self`)", e.Name)
	case e.Code == "forbidden_tier":
		return fmt.Sprintf("this change needs an owner or admin of %s's organization on embeddedci.com (API keys: the benchpod:admin scope)", e.Name)
	case e.Status == http.StatusForbidden && strings.Contains(e.Message, "missing the required scope"):
		return fmt.Sprintf("the API key cannot do this: %s (create a key with that scope, or unset BENCHPOD_API_KEY to use your `benchpod login` session)", e.Message)
	case e.Status == http.StatusUnauthorized:
		return "embeddedci.com refused the sign-in; run `benchpod login` again (or check BENCHPOD_API_KEY)"
	case e.Status == http.StatusForbidden || e.Status == http.StatusNotFound:
		return fmt.Sprintf("%s is not on this account on embeddedci.com (%s)", e.Name, valueOr(e.Message, http.StatusText(e.Status)))
	}
	return fmt.Sprintf("embeddedci.com: %d %s", e.Status, valueOr(e.Message, http.StatusText(e.Status)))
}

// Is makes an offline pod match tcpclient.ErrUnreachable.
func (e *Error) Is(target error) bool {
	return target == tcpclient.ErrUnreachable && (e.Code == "device_offline" || e.Code == "device_reconnecting")
}

// Unwrap gives a held lease or a busy pod as a busy refusal and a missing role as a forbidden
// one, so the CLI's exit code says busy (4) or refused (3) as it does on the LAN.
func (e *Error) Unwrap() error {
	switch e.Code {
	case "lease_held", "device_busy":
		return &tcpclient.PodError{Cmd: "embeddedci.com", Reason: "busy"}
	case "forbidden_tier":
		return &tcpclient.PodError{Cmd: "embeddedci.com", Reason: "forbidden: needs an organization owner or admin"}
	}
	return nil
}

// authHint adds what to do to a refused sign-in.
func authHint(err error) error {
	var apiErr *serverapi.APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		return fmt.Errorf("%w (run `benchpod login` again, or check BENCHPOD_API_KEY)", err)
	}
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden && strings.Contains(apiErr.Message, "organization") {
		return fmt.Errorf("%w (check --org or BENCHPOD_ORG: it must be one of your organizations)", err)
	}
	return err
}

func valueOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
