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
	if len(names) == 0 {
		return nil, fmt.Errorf("no pod named %q on embeddedci.com: this account has no pods", name)
	}
	return nil, fmt.Errorf("no pod named %q on embeddedci.com (this account has: %s)", name, strings.Join(names, ", "))
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
	resp, err := c.API.DeviceCommand(ctx, c.Credential, c.DeviceID, req, timeout)
	if err != nil {
		var apiErr *serverapi.APIError
		if errors.As(err, &apiErr) {
			return nil, &Error{Name: c.Name, Status: apiErr.Status, Code: apiErr.Code, Message: apiErr.Message}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("embeddedci.com: %w", err)
	}
	if strings.TrimSpace(resp.Status) == "error" {
		msg := strings.TrimSpace(resp.Error)
		if msg == "" {
			msg = "pod returned an error"
		}
		return nil, &tcpclient.PodError{Reason: msg}
	}
	return resp.Data, nil
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
		return "this change needs an organization owner or admin on embeddedci.com (API keys: the benchpod:admin scope)"
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
	return err
}

func valueOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
