// Package benchpodconfig persists the benchpod-cli configuration
// (~/.config/benchpod-cli/config.json) with owner-only permissions.
package benchpodconfig

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPort is the bench pod firmware's TCP port.
const DefaultPort = "8080"

// Config holds CLI-level settings that persist between invocations.
//
// Connection is the default connection target (a TCP address, a serial device
// path, or the keyword "serial"); the transport is inferred from its shape by
// the CLI. BenchPodAddr is the legacy field that only ever held a TCP address;
// it is kept for backward-compatible reads and migrated into Connection by Load.
// LastSerial caches the serial device path most recently auto-detected as a bench
// pod, so the next auto-detect probes it first (it can save several port probes).
type Config struct {
	Connection   string `json:"connection,omitempty"`
	BenchPodAddr string `json:"bench_pod_addr,omitempty"`
	LastSerial   string `json:"last_serial,omitempty"`
}

// EnsurePort returns addr with DefaultPort appended when it carries no port.
// An empty addr is returned unchanged so callers can surface a clearer error.
func EnsurePort(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return addr
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, DefaultPort)
}

// DefaultPath returns the canonical config file path, honouring $XDG_CONFIG_HOME
// first and falling back to ~/.config/benchpod-cli/config.json.
func DefaultPath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "benchpod-cli", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("could not resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "benchpod-cli", "config.json"), nil
}

// Load reads the config from disk. Returns os.ErrNotExist if the file is missing.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Migrate the legacy TCP-only field into the unified Connection target.
	if strings.TrimSpace(c.Connection) == "" && strings.TrimSpace(c.BenchPodAddr) != "" {
		c.Connection = strings.TrimSpace(c.BenchPodAddr)
	}
	return &c, nil
}

// Save writes the config to disk atomically (temp file + rename), creating the
// parent directory with 0700 and the file with 0600.
func Save(path string, c *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "config-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	return nil
}
