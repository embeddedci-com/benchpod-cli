package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/authstore"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/spf13/cobra"
)

// ── login (cloud path) ──────────────────────────────────────────────────────

func newLoginCmd(g *globalFlags) *cobra.Command {
	var serverURL, tokenFile string
	var noOpen, force bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with embeddedci-server (device-login flow)",
		Long: "Authenticate the CLI with embeddedci-server.\n\n" +
			"Running this while a usable session already exists is a no-op: it reports who\n" +
			"you are signed in as and exits, rather than sending you through the browser\n" +
			"approval again. Use --force to authenticate anyway (e.g. to switch account),\n" +
			"or `benchpod logout` to drop the session first.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if !force {
				if who, path := currentSession(serverURL, tokenFile); who != "" {
					fmt.Fprintf(os.Stderr, "Already logged in as %s (%s).\n", who, path)
					fmt.Fprintln(os.Stderr, "Run `benchpod login --force` to sign in again, or `benchpod logout` to sign out.")
					return nil
				}
			}
			return runLogin(serverURL, tokenFile, noOpen)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server-url", "https://www.embeddedci.com", "embeddedci-server base URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	cmd.Flags().BoolVar(&noOpen, "no-browser", false, "do not try to open the approval URL in a browser")
	cmd.Flags().BoolVar(&force, "force", false, "authenticate again even when a session already exists")
	return cmd
}

// currentSession reports the signed-in account (email, else user id) and token path when a session is
// usable, or "" when there is none. A session with an expired access token but a
// live refresh token still counts: ensureTokens would renew it silently, so
// sending the user through the browser again would be pointless.
func currentSession(serverURL, tokenFile string) (who, path string) {
	tokenPath, err := resolveTokenPath(tokenFile)
	if err != nil {
		return "", ""
	}
	tokens, err := authstore.Load(tokenPath)
	if err != nil || tokens == nil {
		return "", ""
	}
	now := time.Now()
	if tokens.AccessExpired(now) && tokens.RefreshExpired(now) {
		return "", ""
	}
	who = strings.TrimSpace(tokens.Who())
	if who == "" {
		who = "(unknown)"
	}
	return who, tokenPath
}

// ── logout ──────────────────────────────────────────────────────────────────

func newLogoutCmd(g *globalFlags) *cobra.Command {
	var tokenFile string
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Sign out: delete the cached embeddedci-server tokens",
		Long: "Delete the local token cache, so cloud commands need `benchpod login` again.\n\n" +
			"This is local only: it does not deregister any pod and does not touch the\n" +
			"devices already registered to the account. Signing out and back in leaves\n" +
			"them exactly as they were.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			tokenPath, err := resolveTokenPath(tokenFile)
			if err != nil {
				return fmt.Errorf("resolve token path: %w", err)
			}
			who, _ := currentSession("", tokenFile)
			if err := os.Remove(tokenPath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					fmt.Fprintln(os.Stderr, "Not logged in; nothing to do.")
					return nil
				}
				return fmt.Errorf("remove %s: %w", tokenPath, err)
			}
			if who != "" {
				fmt.Fprintf(os.Stderr, "Logged out %s (removed %s).\n", who, tokenPath)
			} else {
				fmt.Fprintf(os.Stderr, "Logged out (removed %s).\n", tokenPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	return cmd
}

func runLogin(serverURL, tokenFile string, noOpen bool) error {
	if strings.TrimSpace(serverURL) == "" {
		return errors.New("--server-url cannot be empty")
	}
	tokenPath, err := resolveTokenPath(tokenFile)
	if err != nil {
		return fmt.Errorf("resolve token path: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	api := serverapi.New(serverURL)

	codeCtx, codeCancel := context.WithTimeout(ctx, 30*time.Second)
	code, err := api.BeginDeviceLogin(codeCtx)
	codeCancel()
	if err != nil {
		return fmt.Errorf("begin device login: %w", err)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Open this URL to authorize benchpod:")
	fmt.Fprintf(os.Stderr, "  %s\n", code.VerificationURIComplete)
	fmt.Fprintf(os.Stderr, "Code: %s\n", code.UserCode)
	fmt.Fprintf(os.Stderr, "Waiting for approval (up to %ds)...\n\n", code.ExpiresIn)

	if !noOpen {
		if err := openBrowser(code.VerificationURIComplete); err != nil {
			log.Printf("open browser: %v (open the URL manually)", err)
		}
	}

	interval := time.Duration(code.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	expiresIn := time.Duration(code.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 5 * time.Minute
	}
	deadline := time.Now().Add(expiresIn)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		pollCtx, pollCancel := context.WithTimeout(ctx, 15*time.Second)
		resp, outcome, err := api.PollDeviceLogin(pollCtx, code.DeviceCode)
		pollCancel()
		switch {
		case err != nil:
			return fmt.Errorf("poll device login: %w", err)
		case outcome == serverapi.PollSuccess:
			tokens, sErr := saveTokensFromResponse(tokenPath, resp, nil)
			if sErr != nil {
				return fmt.Errorf("save tokens: %w", sErr)
			}
			fmt.Fprintf(os.Stderr, "Logged in as %s. Tokens saved to %s.\n", tokens.Who(), tokenPath)
			return nil
		case outcome == serverapi.PollExpired:
			return errors.New("login timed out or code already used; run `benchpod login` again")
		case outcome == serverapi.PollPending:
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return errors.New("login timed out; run `benchpod login` again")
			}
			log.Printf("auth: still waiting for approval (%s left)", remaining.Truncate(time.Second))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return errors.New("login timed out; run `benchpod login` again")
			}
		}
	}
}
