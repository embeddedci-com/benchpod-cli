// Package authstore persists benchpod's guest tokens (~/.config/benchpod-cli/token.json) with
// owner-only permissions and exposes helpers to query expiry.
//
// The file shape mirrors the JSON response from POST /api/auth/guest plus the absolute
// expiry timestamps the client computes when the response is saved. Storing absolute
// timestamps (rather than only expires_in seconds) lets a later process check expiry
// without re-running the API call.
package authstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Tokens is the on-disk envelope. AccessExpiresAt / RefreshExpiresAt are computed when
// the response is saved (time.Now().Add(expires_in seconds)).
type Tokens struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	SessionID        string    `json:"session_id"`
	UserID           string    `json:"user_id"`
}

// DefaultPath returns the canonical token file path, honouring $XDG_CONFIG_HOME first and
// falling back to $HOME/.config/benchpod-cli/token.json on Unix-like systems. Returns an error
// when neither $XDG_CONFIG_HOME nor $HOME is set (e.g. broken environment in CI).
func DefaultPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "benchpod-cli", "token.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("could not resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "benchpod-cli", "token.json"), nil
}

// Load reads tokens from disk. Returns os.ErrNotExist if the file is missing so callers
// can branch into the create-new-guest path. Other read/parse errors are returned wrapped.
func Load(path string) (*Tokens, error) {
	if path == "" {
		return nil, errors.New("token path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Tokens
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &t, nil
}

// Save writes tokens to disk, creating the parent directory with 0700 and the file with 0600.
// Writes go through a temp file + os.Rename so a crash mid-write cannot corrupt the existing file.
func Save(path string, t *Tokens) error {
	if path == "" {
		return errors.New("token path is empty")
	}
	if t == nil {
		return errors.New("nil tokens")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "token-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp token: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp token: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}

// expirySkew is the safety margin treated as "already expired" so we refresh before the
// server actually rejects the token. 60 s comfortably covers clock drift + roundtrip latency.
const expirySkew = 60 * time.Second

// AccessExpired reports true when the access token is within expirySkew of, or past, its expiry.
func (t *Tokens) AccessExpired(now time.Time) bool {
	if t == nil || t.AccessToken == "" {
		return true
	}
	return !t.AccessExpiresAt.IsZero() && !now.Before(t.AccessExpiresAt.Add(-expirySkew))
}

// RefreshExpired reports true when the refresh token is within expirySkew of, or past, its expiry.
func (t *Tokens) RefreshExpired(now time.Time) bool {
	if t == nil || t.RefreshToken == "" {
		return true
	}
	return !t.RefreshExpiresAt.IsZero() && !now.Before(t.RefreshExpiresAt.Add(-expirySkew))
}
