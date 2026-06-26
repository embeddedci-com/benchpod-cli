// Package serverapi is a thin HTTP client for embeddedci-server's auth and device endpoints.
//
// The CLI logs a real user in with the RFC-8628 device-authorization flow:
// BeginDeviceLogin starts it (the user approves a code in the browser) and
// PollDeviceLogin waits for the tokens. RefreshTokens rotates the access token
// when it has expired but the refresh token is still valid. RegisterDevice
// records an attached bench pod against the logged-in user. All token-returning
// helpers yield a normalised TokenResponse the caller can save with
// authstore.Tokens directly.
package serverapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TokenResponse is the parsed body returned by the token-issuing endpoints
// (POST /api/auth/device/token and /api/auth/guest/refresh).
type TokenResponse struct {
	AccessToken      string `json:"access_token"`
	AccessExpiresIn  int    `json:"access_expires_in"`
	RefreshToken    string `json:"refresh_token"`
	RefreshExpiresIn int    `json:"refresh_expires_in"`
	SessionID        string `json:"session_id"`
	UserID           string `json:"user_id"`
}

// DeviceResponse is the parsed body returned by POST /api/benchpod/devices.
type DeviceResponse struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	OwnerUserID    string            `json:"owner_user_id"`
	OrganizationID string            `json:"organization_id,omitempty"`
	Parameters     map[string]string `json:"parameters"`
}

// DeviceCodeResponse is the parsed body returned by POST /api/auth/device/code.
// The CLI uses VerificationURIComplete to print the approval URL and DeviceCode
// to poll /api/auth/device/token for tokens.
type DeviceCodeResponse struct {
	UserCode                string `json:"user_code"`
	DeviceCode              string `json:"device_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// PollOutcome classifies the result of one /api/auth/device/token poll.
type PollOutcome int

const (
	// PollSuccess means tokens were returned (TokenResponse populated).
	PollSuccess PollOutcome = iota
	// PollPending means the user has not yet approved the code (server returned authorization_pending).
	PollPending
	// PollExpired means the code expired or was already consumed.
	PollExpired
)

// Client wraps an HTTP client and the server base URL (e.g. "https://www.embeddedci.com").
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a Client targeting baseURL with a 30 s default HTTP timeout.
// baseURL may end with or without a trailing slash; trailing slashes are stripped.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// RefreshTokens calls POST /api/auth/guest/refresh to rotate the session's tokens. The server
// rejects access tokens here (it requires TokenType=benchpod_refresh); see authstore for the
// expiry check that should fence off calls with a stale refresh token before they hit the wire.
func (c *Client) RefreshTokens(ctx context.Context, refreshToken string) (*TokenResponse, error) {
	rt := strings.TrimSpace(refreshToken)
	if rt == "" {
		return nil, errors.New("refresh token is empty")
	}
	body := struct {
		RefreshToken string `json:"refresh_token"`
	}{RefreshToken: rt}
	return c.postTokens(ctx, "/api/auth/guest/refresh", body)
}

// postTokens is the shared POST + JSON + error-shaping helper for the token endpoints.
func (c *Client) postTokens(ctx context.Context, path string, body any) (*TokenResponse, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, errors.New("serverapi: BaseURL is empty")
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Try to surface the server's `error` field when present (writeJSONError shape).
		var errBody struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(data, &errBody)
		msg := strings.TrimSpace(errBody.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return nil, fmt.Errorf("%s %s: %d %s", req.Method, path, resp.StatusCode, msg)
	}
	var out TokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" {
		return nil, errors.New("server response missing access_token or refresh_token")
	}
	return &out, nil
}

// RegisterDevice calls POST /api/benchpod/devices with the logged-in user's benchpod JWT.
// The device is scoped to the user's default organization. When publicKey is non-empty it is the
// device's Ed25519 public key (base64url, 43 chars) and becomes the device's stable
// identity (the server dedups on it); pass "" for the keyless registration flow.
// Returns the upserted device record.
func (c *Client) RegisterDevice(ctx context.Context, accessToken, name, publicKey string, parameters map[string]string) (*DeviceResponse, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, errors.New("serverapi: BaseURL is empty")
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("serverapi: access token is empty")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("serverapi: device name is empty")
	}
	body := struct {
		Name       string            `json:"name"`
		PublicKey  string            `json:"public_key,omitempty"`
		Parameters map[string]string `json:"parameters,omitempty"`
	}{Name: strings.TrimSpace(name), PublicKey: strings.TrimSpace(publicKey), Parameters: parameters}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/benchpod/devices", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(data, &errBody)
		msg := strings.TrimSpace(errBody.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return nil, fmt.Errorf("POST /api/benchpod/devices: %d %s", resp.StatusCode, msg)
	}
	var out DeviceResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if strings.TrimSpace(out.ID) == "" {
		return nil, errors.New("server response missing device id")
	}
	return &out, nil
}

// BeginDeviceLogin calls POST /api/auth/device/code to start the CLI login flow.
// The returned UserCode is what the user types/sees in the browser; DeviceCode is the
// opaque secret the CLI uses when polling for tokens.
func (c *Client) BeginDeviceLogin(ctx context.Context) (*DeviceCodeResponse, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, errors.New("serverapi: BaseURL is empty")
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/auth/device/code", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST /api/auth/device/code: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out DeviceCodeResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if strings.TrimSpace(out.UserCode) == "" || strings.TrimSpace(out.DeviceCode) == "" {
		return nil, errors.New("server response missing user_code or device_code")
	}
	return &out, nil
}

// PollDeviceLogin calls POST /api/auth/device/token with the given device code. The
// returned outcome is one of PollSuccess (with non-nil TokenResponse), PollPending
// (still waiting on the user to approve), or PollExpired (deadline reached / already
// consumed). Network and parsing errors are returned as err.
func (c *Client) PollDeviceLogin(ctx context.Context, deviceCode string) (*TokenResponse, PollOutcome, error) {
	if c == nil || strings.TrimSpace(c.BaseURL) == "" {
		return nil, PollExpired, errors.New("serverapi: BaseURL is empty")
	}
	if strings.TrimSpace(deviceCode) == "" {
		return nil, PollExpired, errors.New("serverapi: device_code is empty")
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	body := struct {
		DeviceCode string `json:"device_code"`
	}{DeviceCode: deviceCode}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, PollExpired, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/auth/device/token", bytes.NewReader(buf))
	if err != nil {
		return nil, PollExpired, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, PollExpired, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, PollExpired, fmt.Errorf("read response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var out TokenResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, PollExpired, fmt.Errorf("parse response: %w", err)
		}
		if strings.TrimSpace(out.AccessToken) == "" || strings.TrimSpace(out.RefreshToken) == "" {
			return nil, PollExpired, errors.New("server response missing access_token or refresh_token")
		}
		return &out, PollSuccess, nil
	case http.StatusBadRequest:
		// authorization_pending is the only documented 400 body.
		return nil, PollPending, nil
	case http.StatusGone:
		// expired_token (timed out or already consumed).
		return nil, PollExpired, nil
	default:
		return nil, PollExpired, fmt.Errorf("POST /api/auth/device/token: %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
}
