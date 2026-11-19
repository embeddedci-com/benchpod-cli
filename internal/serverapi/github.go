package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── GitHub Actions sign-in (OIDC) ───────────────────────────────────────────
//
// In a GitHub Actions job with `permissions: id-token: write`, the runner hands out OIDC tokens
// (ACTIONS_ID_TOKEN_REQUEST_URL / _TOKEN). POST /api/github/oidc/token exchanges one for a
// github_action token of the organization that trusts the repository; the /api/cloud/* routes
// and the runs API take it.

// DefaultOIDCAudience is the audience embeddedci.com checks (the Python SDK asks for the same).
const DefaultOIDCAudience = "https://embeddedci.com"

// GitHubActionToken is the exchanged token.
type GitHubActionToken struct {
	AccessToken    string   `json:"access_token"`
	ExpiresAt      string   `json:"expires_at"`
	AllowAnyDevice bool     `json:"allow_any_device"`
	Devices        []string `json:"devices"`
	Repository     string   `json:"repository"`
}

// Expiry is when the token ends (zero when the server did not say).
func (t *GitHubActionToken) Expiry() time.Time {
	e, _ := time.Parse(time.RFC3339, t.ExpiresAt)
	return e
}

// MintGitHubOIDC asks the Actions runner for an OIDC token for audience.
func MintGitHubOIDC(ctx context.Context, httpc *http.Client, requestURL, requestToken, audience string) (string, error) {
	if requestURL == "" || requestToken == "" {
		return "", errors.New("the job has no id-token permission: add `permissions: id-token: write` to the workflow or job")
	}
	sep := "?"
	if strings.Contains(requestURL, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL+sep+"audience="+url.QueryEscape(audience), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+requestToken)
	req.Header.Set("Accept", "application/json")
	if httpc == nil {
		httpc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("request a GitHub OIDC token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request a GitHub OIDC token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var v struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(body, &v) != nil || v.Value == "" {
		return "", errors.New("request a GitHub OIDC token: the reply has no value")
	}
	return v.Value, nil
}

// ExchangeGitHubOIDC trades a GitHub OIDC token for a github_action token (POST
// /api/github/oidc/token, no other credential).
func (c *Client) ExchangeGitHubOIDC(ctx context.Context, oidcToken string) (*GitHubActionToken, error) {
	var out GitHubActionToken
	if err := c.doJSONNoAuth(ctx, http.MethodPost, "/api/github/oidc/token", map[string]string{"token": oidcToken}, &out); err != nil {
		return nil, err
	}
	if out.AccessToken == "" {
		return nil, errors.New("embeddedci.com: the GitHub token exchange returned no access_token")
	}
	return &out, nil
}

// doJSONNoAuth is a JSON request without a credential.
func (c *Client) doJSONNoAuth(ctx context.Context, method, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, strings.NewReader(string(buf)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: strings.TrimSpace(e.Error), Code: e.Code, Body: strings.TrimSpace(string(data))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// CloudDeviceCommand sends one command to the pod named name through the cloud route (POST
// /api/cloud/devices/command?device=<name>), which a github_action token may use.
func (c *Client) CloudDeviceCommand(ctx context.Context, token, name string, command map[string]any, timeout time.Duration) (*CommandResponse, error) {
	body := struct {
		Command   map[string]any `json:"command"`
		TimeoutMs int            `json:"timeout_ms,omitempty"`
	}{Command: command, TimeoutMs: int(timeout / time.Millisecond)}
	var out CommandResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/devices/command?device="+url.QueryEscape(name), token, body, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}
