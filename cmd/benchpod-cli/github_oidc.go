package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// ── signing in from GitHub Actions ──────────────────────────────────────────
//
// Without BENCHPOD_API_KEY or a `benchpod login` session, a GitHub Actions job with
// `permissions: id-token: write` signs in with its OIDC token: embeddedci.com exchanges it for a
// github_action token of the organization that trusts the repository (Settings, GitHub
// repositories). Cloud pods (embeddedci:<name>) and run uploads take that token.

// inGitHubActionsOIDC says the runner offers OIDC tokens.
func inGitHubActionsOIDC() bool {
	return strings.TrimSpace(os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")) != "" &&
		strings.TrimSpace(os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")) != ""
}

// oidcAudience is EMBEDDEDCI_OIDC_AUDIENCE, else the one embeddedci.com checks.
func oidcAudience() string {
	if v := strings.TrimSpace(os.Getenv("EMBEDDEDCI_OIDC_AUDIENCE")); v != "" {
		return v
	}
	return serverapi.DefaultOIDCAudience
}

var ghToken struct {
	sync.Mutex
	base    string
	token   string
	expires time.Time
}

// githubActionCredential mints (once, and again near its expiry) the github_action token.
func githubActionCredential(ctx context.Context, api *serverapi.Client) (string, error) {
	ghToken.Lock()
	defer ghToken.Unlock()
	if ghToken.token != "" && ghToken.base == api.BaseURL && (ghToken.expires.IsZero() || time.Until(ghToken.expires) > 2*time.Minute) {
		return ghToken.token, nil
	}
	oidc, err := serverapi.MintGitHubOIDC(ctx, &http.Client{Timeout: 30 * time.Second},
		os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"), oidcAudience())
	if err != nil {
		return "", err
	}
	tok, err := api.ExchangeGitHubOIDC(ctx, oidc)
	if err != nil {
		return "", fmt.Errorf("sign in to embeddedci.com from GitHub Actions: %w (is this repository trusted by your organization on embeddedci.com?)", err)
	}
	ghToken.base, ghToken.token, ghToken.expires = api.BaseURL, tok.AccessToken, tok.Expiry()
	return tok.AccessToken, nil
}

// isGitHubActionCredential says cred is the github_action token minted here.
func isGitHubActionCredential(cred string) bool {
	ghToken.Lock()
	defer ghToken.Unlock()
	return ghToken.token != "" && cred == ghToken.token
}

// resetGitHubToken forgets the minted token (tests).
func resetGitHubToken() {
	ghToken.Lock()
	defer ghToken.Unlock()
	ghToken.base, ghToken.token, ghToken.expires = "", "", time.Time{}
}

// cloudIsGitHubAction says the credential for embeddedci.com is (or would be) the GitHub Actions
// token: no API key and no `benchpod login` session, in a job with OIDC.
func cloudIsGitHubAction() bool {
	if strings.TrimSpace(os.Getenv("BENCHPOD_API_KEY")) != "" || !inGitHubActionsOIDC() {
		return false
	}
	path, err := resolveTokenPath("")
	if err != nil {
		return true
	}
	_, err = os.Stat(path)
	return err != nil
}
