package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── the cloud device lease (benchpod ci over embeddedci.com) ────────────────
//
// The lease routes (/api/cloud/devices/lease*) take a short-lived cloud session token, minted from
// the user's credential with POST /api/auth/token; the same routes and body the Python SDK uses.

// CloudSession is a minted cloud session token.
type CloudSession struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	ExpiresIn int    `json:"expires_in"`
}

// MintCloudSession calls POST /api/auth/token with the user's credential (a `benchpod login`
// token or "ApiKey eci_...") and returns a cloud session token valid for ttlSeconds (the server
// may give less).
func (c *Client) MintCloudSession(ctx context.Context, credential string, ttlSeconds int) (*CloudSession, error) {
	var out CloudSession
	body := map[string]any{"ttl_seconds": ttlSeconds}
	if err := c.doAuthedJSON(ctx, http.MethodPost, "/api/auth/token", credential, body, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Token) == "" {
		return nil, errors.New("embeddedci.com: the token exchange returned no token")
	}
	return &out, nil
}

// LeaseBusy is the holder of a lease someone else has.
type LeaseBusy struct {
	Holder    string `json:"holder"`
	ExpiresAt string `json:"expires_at"`
}

// LeaseRequest is the body of the lease routes.
type LeaseRequest struct {
	LeaseID    string `json:"lease_id"`
	RunLabel   string `json:"run_label,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// CloudLease calls POST /api/cloud/devices/lease[/<action>]?device=<name> with the session token:
// action "" acquires (or refreshes the same id), "renew" renews, "release" releases. A lease held
// by someone else comes back as busy (HTTP 409), not an error.
func (c *Client) CloudLease(ctx context.Context, session, action, device string, req LeaseRequest) (*LeaseBusy, error) {
	path := "/api/cloud/devices/lease"
	if action != "" {
		path += "/" + action
	}
	path += "?device=" + url.QueryEscape(device)
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := c.doJSON(tctx, http.MethodPost, path, session, req, nil, false)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
		var busy LeaseBusy
		_ = json.Unmarshal([]byte(apiErr.Body), &busy)
		if busy.Holder == "" {
			busy.Holder = "another session"
		}
		return &busy, nil
	}
	return nil, err
}
