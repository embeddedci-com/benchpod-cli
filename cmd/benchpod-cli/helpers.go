package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/authstore"
	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"golang.org/x/term"
)

// ── connection resolution ───────────────────────────────────────────────────

// rawConnection returns the connection string to use: the --connection flag when
// set, otherwise the stored default from the config file. It is "" when neither
// is set (a missing config file is not an error here — callers decide whether an
// empty connection is fatal).
func (g *globalFlags) rawConnection() (string, error) {
	if raw := strings.TrimSpace(g.connection); raw != "" {
		return raw, nil
	}
	cfgPath, err := resolveConfigPath(g.configFile)
	if err != nil {
		return "", fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := benchpodconfig.Load(cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("load config: %w", err)
	}
	return strings.TrimSpace(cfg.Connection), nil
}

// resolveConnection classifies the effective connection (flag or stored default)
// into a ConnSpec. Used by the firmware commands and flash; errors when no
// connection is set or the value is unusable.
func (g *globalFlags) resolveConnection() (ConnSpec, error) {
	raw, err := g.rawConnection()
	if err != nil {
		return ConnSpec{}, err
	}
	return classifyConnection(raw)
}

// serialDevice resolves the serial device for the always-serial commands
// (set/show/clear-wifi, bootsel). It honors an explicit device path from the
// connection (flag or stored default) and otherwise auto-detects (""). It never
// errors: provisioning must work before any default exists, and a stored wifi
// address simply falls through to USB auto-detection.
func (g *globalFlags) serialDevice() string {
	raw, err := g.rawConnection()
	if err != nil || raw == "" {
		return ""
	}
	if spec, err := classifyConnection(raw); err == nil && spec.IsSerial() {
		return spec.Device
	}
	return ""
}

// ── shared firmware-command setup (wifi/TCP path) ───────────────────────────

// wifiClient resolves the connection, requires the wifi transport (returning the
// standard "not available over serial" error for the named command otherwise),
// and returns a ready tcpclient plus a deadline context (overridable by
// --timeout) with signal handling. It replaces the old RequireWifi + setupClient
// pair at every TCP command's call site.
func (g *globalFlags) wifiClient(cmdName string, def time.Duration) (context.Context, context.CancelFunc, *tcpclient.Client, error) {
	spec, err := g.resolveConnection()
	if err != nil {
		return nil, func() {}, nil, err
	}
	if err := spec.RequireWifi(cmdName); err != nil {
		return nil, func() {}, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(def))
	stop := installSignalHandler(ctx, cancel)
	return ctx, withSignalCleanup(cancel, stop), &tcpclient.Client{Addr: spec.Addr}, nil
}

// withSignalCleanup wraps a context cancel func so calling it also stops the
// signal handler installed alongside it. Callers already `defer cancel()`, so
// returning this in place of the bare cancel keeps the signal handler scoped to
// the command's lifetime without changing any call site.
func withSignalCleanup(cancel context.CancelFunc, stop func()) context.CancelFunc {
	return func() {
		stop()
		cancel()
	}
}

// openSerialConsole opens the bench-pod serial console (auto-detecting unless an
// explicit device is given) and builds a timeout context with signal handling. It
// is the serial-path analogue of setupClient.
func (g *globalFlags) openSerialConsole(device string, timeout time.Duration) (*serialconsole.Console, string, context.Context, context.CancelFunc, error) {
	console, path, err := g.openBenchpodSerial(device, 2*time.Second)
	if err != nil {
		return nil, "", nil, func() {}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	stop := installSignalHandler(ctx, cancel)
	return console, path, ctx, withSignalCleanup(cancel, stop), nil
}

// openBenchpodSerial is the single serial auto-detect entry point shared by all
// serial commands (and flash). For an explicit device it opens that verbatim;
// otherwise it probes USB serial ports for the bench-pod identifier, trying the
// cached hint (config LastSerial) first, and caches whatever it finds so the next
// auto-detect probes it first.
func (g *globalFlags) openBenchpodSerial(device string, probeTimeout time.Duration) (*serialconsole.Console, string, error) {
	cfgPath, cfgErr := resolveConfigPath(g.configFile)
	hint := ""
	if cfgErr == nil {
		if cfg, err := benchpodconfig.Load(cfgPath); err == nil && cfg != nil {
			hint = strings.TrimSpace(cfg.LastSerial)
		}
	}
	console, path, err := serialconsole.OpenBenchpod(device, hint, probeTimeout)
	if err != nil {
		return nil, "", err
	}
	// Remember the auto-detected device so the next run probes it first.
	if strings.TrimSpace(device) == "" && path != "" && path != hint && cfgErr == nil {
		if err := saveLastSerial(cfgPath, path); err != nil {
			log.Printf("note: could not cache serial device %q: %v", path, err)
		}
	}
	return console, path, nil
}

// saveLastSerial persists the auto-detected serial device path to config.
func saveLastSerial(cfgPath, path string) error {
	cfg, err := benchpodconfig.Load(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cfg == nil {
		cfg = &benchpodconfig.Config{}
	}
	if cfg.LastSerial == path {
		return nil
	}
	cfg.LastSerial = path
	return benchpodconfig.Save(cfgPath, cfg)
}

// ── validation helpers ──────────────────────────────────────────────────────

func validWaveform(w string) bool {
	switch w {
	case "sine", "square", "sawtooth":
		return true
	default:
		return false
	}
}

func validSamples(n int) bool { return n >= 1 && n <= 4096 }

func validOutput(o string) bool { return o == "json" || o == "csv" || o == "ndjson" }

// parseLAPin parses a logic-analyzer pin reference for SWD wiring. These are no
// longer GPIO numbers: they are the pod's LA pins (1-12) that map to the
// ice40/FPGA. Accepts an optional "la" prefix, so "la1", "1", "la12", and "12"
// are all valid.
func parseLAPin(s string) (int, error) {
	orig := strings.TrimSpace(s)
	v := strings.TrimPrefix(strings.ToLower(orig), "la")
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 || n > 12 {
		return 0, fmt.Errorf("invalid LA pin %q (use 1-12, e.g. la1 or 1)", orig)
	}
	return n, nil
}

// ── output formatting ───────────────────────────────────────────────────────

// resolveOutput returns the destination writer for command output. An empty
// filename yields os.Stdout with a no-op closer; otherwise the file is created
// (truncating any existing content) and returned alongside its Close method.
func resolveOutput(filename string) (io.Writer, func() error, error) {
	if strings.TrimSpace(filename) == "" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(filename)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// sampleRecord is one NDJSON line for sample data.
type sampleRecord struct {
	Index int `json:"index"`
	Value int `json:"value"`
}

// writeSamples writes reassembled samples to w in the requested format.
func writeSamples(w io.Writer, samples []int, format string) error {
	switch format {
	case "csv":
		if _, err := fmt.Fprintln(w, "index,value"); err != nil {
			return err
		}
		for i, v := range samples {
			if _, err := fmt.Fprintf(w, "%d,%d\n", i, v); err != nil {
				return err
			}
		}
	case "ndjson":
		enc := json.NewEncoder(w) // Encode appends a newline per record
		for i, v := range samples {
			if err := enc.Encode(sampleRecord{Index: i, Value: v}); err != nil {
				return err
			}
		}
	default: // json
		out, err := json.Marshal(samples)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, string(out)); err != nil {
			return err
		}
	}
	return nil
}

// emitSamples resolves the output destination and writes samples in the chosen
// format, logging a confirmation to stderr when writing to a file.
func emitSamples(cmd, outputFilename string, samples []int, format string) error {
	out, closeOut, err := resolveOutput(outputFilename)
	if err != nil {
		return fmt.Errorf("%s: open output: %w", cmd, err)
	}
	defer closeOut()
	if err := writeSamples(out, samples, format); err != nil {
		return fmt.Errorf("%s: write output: %w", cmd, err)
	}
	if strings.TrimSpace(outputFilename) != "" {
		log.Printf("wrote %d samples to %s", len(samples), outputFilename)
	}
	return nil
}

// printJSON pretty-prints a raw JSON value to w.
func printJSON(w io.Writer, raw json.RawMessage) {
	var buf any
	if err := json.Unmarshal(raw, &buf); err != nil {
		fmt.Fprintln(w, string(raw))
		return
	}
	out, err := json.MarshalIndent(buf, "", "  ")
	if err != nil {
		fmt.Fprintln(w, string(raw))
		return
	}
	fmt.Fprintln(w, string(out))
}

func valueOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// ── auth / config helpers (shared with the cloud login path) ────────────────

// ensureTokens loads the cached real-user tokens, refreshing the access token
// when expired. There is no guest fallback: hardware devices
// require a logged-in user, so a missing/expired session returns an error
// telling the user to run `benchpod login`.
func ensureTokens(ctx context.Context, api *serverapi.Client, tokenPath string) (*authstore.Tokens, error) {
	now := time.Now()
	existing, err := authstore.Load(tokenPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("not logged in; run `benchpod login` first")
		}
		return nil, fmt.Errorf("read %s: %w", tokenPath, err)
	}
	if !existing.AccessExpired(now) {
		return existing, nil
	}
	if existing.RefreshExpired(now) {
		return nil, fmt.Errorf("session expired; run `benchpod login` again")
	}
	resp, err := api.RefreshTokens(ctx, existing.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("refresh token failed (%v); run `benchpod login` again", err)
	}
	saved, err := saveTokensFromResponse(tokenPath, resp, existing.SessionID, existing.UserID)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

func saveTokensFromResponse(path string, resp *serverapi.TokenResponse, fallbackSessionID, fallbackUserID string) (*authstore.Tokens, error) {
	if resp == nil {
		return nil, errors.New("nil response")
	}
	now := time.Now()
	sessionID := strings.TrimSpace(resp.SessionID)
	if sessionID == "" {
		sessionID = fallbackSessionID
	}
	userID := strings.TrimSpace(resp.UserID)
	if userID == "" {
		userID = fallbackUserID
	}
	t := &authstore.Tokens{
		AccessToken:      resp.AccessToken,
		RefreshToken:     resp.RefreshToken,
		AccessExpiresAt:  now.Add(time.Duration(resp.AccessExpiresIn) * time.Second),
		RefreshExpiresAt: now.Add(time.Duration(resp.RefreshExpiresIn) * time.Second),
		SessionID:        sessionID,
		UserID:           userID,
	}
	if err := authstore.Save(path, t); err != nil {
		return nil, fmt.Errorf("save tokens to %s: %w", path, err)
	}
	return t, nil
}

func resolveTokenPath(flagVal string) (string, error) {
	p := strings.TrimSpace(flagVal)
	if p != "" {
		return p, nil
	}
	return authstore.DefaultPath()
}

func resolveConfigPath(flagVal string) (string, error) {
	p := strings.TrimSpace(flagVal)
	if p != "" {
		return p, nil
	}
	return benchpodconfig.DefaultPath()
}

// resolveWifiPassword returns the WiFi password using the precedence: --password
// flag, then --password-stdin (one line from stdin), then an interactive masked
// prompt when stdin is a TTY.
func resolveWifiPassword(flagVal string, stdin bool) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	if stdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		pw := strings.TrimRight(line, "\r\n")
		if pw == "" {
			return "", errors.New("empty password on stdin")
		}
		return pw, nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no password provided; pass --password, --password-stdin, or run in an interactive terminal")
	}
	fmt.Fprint(os.Stderr, "WiFi password: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	pw := string(b)
	if pw == "" {
		return "", errors.New("empty password")
	}
	return pw, nil
}

// installSignalHandler arranges for SIGINT/SIGTERM to cancel ctx and returns a
// cleanup func the caller must invoke (defer) to stop delivering signals and let
// the watcher goroutine exit. Returning a cleanup — rather than leaking the
// goroutine and the signal registration for the process lifetime as the old
// per-call version did — keeps each command's handler scoped to that command and
// avoids piling up duplicate SIGINT registrations.
func installSignalHandler(ctx context.Context, cancel context.CancelFunc) func() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case s := <-sig:
			log.Printf("received %s, shutting down", s)
			cancel()
		case <-ctx.Done():
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sig)
		close(done)
	}
}

func openBrowser(target string) error {
	if strings.TrimSpace(target) == "" {
		return errors.New("empty url")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}
