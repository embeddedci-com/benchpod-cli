package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── login (cloud path) ──────────────────────────────────────────────────────

func newLoginCmd(g *globalFlags) *cobra.Command {
	var serverURL, tokenFile string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate with embeddedci-server (device-login flow)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLogin(serverURL, tokenFile, noOpen)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server-url", "https://www.embeddedci.com", "embeddedci-server base URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	cmd.Flags().BoolVar(&noOpen, "no-browser", false, "do not try to open the approval URL in a browser")
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
			tokens, sErr := saveTokensFromResponse(tokenPath, resp, "", "")
			if sErr != nil {
				return fmt.Errorf("save tokens: %w", sErr)
			}
			fmt.Fprintf(os.Stderr, "Logged in as user %s. Tokens saved to %s.\n", tokens.UserID, tokenPath)
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

// ── register (register device by public key + provision direct cloud connection) ──

func newRegisterCmd(g *globalFlags) *cobra.Command {
	var serverURL, tokenFile, deviceName string
	var insecureSkipVerify bool
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register the bench pod and provision it to connect directly to the server",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRegister(g, serverURL, tokenFile, deviceName, insecureSkipVerify)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server-url", "https://www.embeddedci.com", "embeddedci-server base URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "logical device name, URL-safe (defaults to the bench pod host/IP)")
	cmd.Flags().BoolVar(&insecureSkipVerify, "insecure-skip-verify", false,
		"provision the pod to skip TLS certificate verification (bring-up only; default verifies against the ESP x509 cert bundle)")
	return cmd
}

// defaultDeviceNameFromAddr derives a server-valid default device name from a connection address.
// The server requires URL-safe names ([A-Za-z0-9._-], no colons), so we use the host (IP or
// hostname, without the port) and replace any remaining out-of-charset characters with '-'.
func defaultDeviceNameFromAddr(addr string) string {
	host := strings.TrimSpace(addr)
	// SplitHostPort succeeds when a port is present; use the host part (which may be empty for a
	// host-less ":port", caught by the fallback below). A missing port returns an error — keep addr.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = strings.TrimSpace(h)
	}
	host = strings.Trim(host, "[]") // tolerate a bracketed IPv6 literal
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "benchpod"
	}
	return name
}

// runRegister authenticates the CLI as a real user, registers the attached bench pod as a
// benchpod device keyed by the device's own Ed25519 public key, then provisions the pod (via a
// local `cloud_set` command over LAN TCP) to open the control WebSocket to the server itself.
// The CLI is a one-time provisioning step, NOT a runtime bridge: after this the firmware persists
// the cloud config and reconnects on every boot. One logged-in user can register many physical
// devices (one per invocation/address).
func runRegister(g *globalFlags, serverURL, tokenFile, deviceName string, insecureSkipVerify bool) error {
	if strings.TrimSpace(serverURL) == "" {
		return errors.New("--server-url cannot be empty")
	}
	host, port, tls, err := parseServerEndpoint(serverURL)
	if err != nil {
		return fmt.Errorf("parse --server-url: %w", err)
	}
	// Verify the server cert by default when using TLS; --insecure-skip-verify opts out for bring-up.
	verify := tls && !insecureSkipVerify
	spec, err := g.resolveConnection()
	if err != nil {
		return err
	}
	if err := spec.RequireWifi("register"); err != nil {
		return err
	}
	addr := spec.Addr
	tokenPath, err := resolveTokenPath(tokenFile)
	if err != nil {
		return fmt.Errorf("resolve token path: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	api := serverapi.New(serverURL)
	tokens, err := ensureTokens(ctx, api, tokenPath)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	log.Printf("auth: signed in as user %s", tokens.UserID)

	client := &tcpclient.Client{Addr: addr}

	// Fetch the device's public key — this is the device's stable identity.
	idCtx, idCancel := context.WithTimeout(ctx, 30*time.Second)
	pubKey, err := client.IdentityPublic(idCtx)
	idCancel()
	if err != nil {
		return fmt.Errorf("fetch device public key: %w", err)
	}

	name := strings.TrimSpace(deviceName)
	if name == "" {
		// The server requires URL-safe device names ([A-Za-z0-9._-], no colons), so the raw
		// "host:port" address is rejected. Default to the host (IP/hostname without the port);
		// the name is editable in the web UI afterwards.
		name = defaultDeviceNameFromAddr(addr)
	}

	regCtx, regCancel := context.WithTimeout(ctx, 30*time.Second)
	device, err := api.RegisterDevice(regCtx, tokens.AccessToken, name, pubKey, nil)
	regCancel()
	if err != nil {
		return fmt.Errorf("register device: %w", err)
	}
	log.Printf("device: registered name=%s id=%s", device.Name, device.ID)

	// Provision the bench pod to connect to the server directly. After this the firmware owns the
	// cloud websocket and reconnects on every boot — the CLI is no longer a runtime bridge.
	setCtx, setCancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = client.Command(setCtx, map[string]any{
		"cmd":       "cloud_set",
		"host":      host,
		"port":      port,
		"tls":       tls,
		"verify":    verify,
		"device_id": device.ID,
		"enabled":   true,
	})
	setCancel()
	if err != nil {
		return fmt.Errorf("provision bench pod cloud connection: %w", err)
	}
	log.Printf("provisioned: bench pod will connect to %s:%d (tls=%v verify=%v) as device %s", host, port, tls, verify, device.ID)

	// Best-effort: report the cloud connection state the device sees. A failure
	// here doesn't undo the registration/provisioning, so warn (don't fail) — but
	// surface it rather than silently dropping it.
	statCtx, statCancel := context.WithTimeout(ctx, 10*time.Second)
	if raw, err := client.Command(statCtx, map[string]any{"cmd": "cloud_status"}); err != nil {
		log.Printf("warning: could not read bench pod cloud status: %v", err)
	} else {
		log.Printf("bench pod cloud status: %s", strings.TrimSpace(string(raw)))
	}
	statCancel()

	fmt.Fprintf(os.Stderr, "Registered bench pod %q (device %s).\n", device.Name, device.ID)
	return nil
}

// ── deregister (detach the pod from the account, keeping its data) ──────────

func newDeregisterCmd(g *globalFlags) *cobra.Command {
	var serverURL, tokenFile, deviceName, deviceID string
	var keepPodConfig bool
	cmd := &cobra.Command{
		Use:   "deregister",
		Short: "Deregister the bench pod from the server (keeps its data) and stop it connecting",
		Long: "Deregister the bench pod from the logged-in user's account.\n\n" +
			"The server keeps everything recorded for the pod — captures, waveforms, wiring —\n" +
			"and only marks the device disabled, so it disappears from the web UI and its\n" +
			"identity is freed for another account. Registering the SAME pod again for the\n" +
			"same account revives the device with its data; registering it for a different\n" +
			"account gives that account a fresh device and leaves this one's history alone.\n\n" +
			"By default the pod is identified by asking the attached bench pod for its public\n" +
			"key (so --connection must point at it) and is then told to stop connecting to the\n" +
			"cloud. Use --device-name/--device-id to deregister a pod you cannot reach.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDeregister(g, serverURL, tokenFile, deviceName, deviceID, keepPodConfig)
		},
	}
	cmd.Flags().StringVar(&serverURL, "server-url", "https://www.embeddedci.com", "embeddedci-server base URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "deregister the device with this name instead of the attached bench pod")
	cmd.Flags().StringVar(&deviceID, "device-id", "", "deregister the device with this id instead of the attached bench pod")
	cmd.Flags().BoolVar(&keepPodConfig, "keep-pod-config", false,
		"leave the pod's cloud configuration in place (it will keep trying to connect and be refused)")
	return cmd
}

// runDeregister detaches a bench pod from the logged-in user's account. It is the inverse of
// runRegister and undoes both halves of it: the server-side device record (disabled, not deleted —
// the data survives for a later re-register) and the pod-side cloud provisioning written by
// `cloud_set` (so the firmware stops opening the control WebSocket on every boot).
//
// Device selection mirrors register: by default the attached pod is asked for its public key and
// the matching device is looked up, which is the only way to be sure the right physical pod is
// detached. --device-name / --device-id cover the pod that is broken, gone, or already wiped; in
// that case there is nothing to unprovision locally, so the pod-side step is skipped.
func runDeregister(g *globalFlags, serverURL, tokenFile, deviceName, deviceID string, keepPodConfig bool) error {
	if strings.TrimSpace(serverURL) == "" {
		return errors.New("--server-url cannot be empty")
	}
	deviceName = strings.TrimSpace(deviceName)
	deviceID = strings.TrimSpace(deviceID)
	if deviceName != "" && deviceID != "" {
		return errors.New("pass only one of --device-name or --device-id")
	}
	// Only the default (identify-by-key) path talks to the pod; an explicit selector means the
	// pod may be unreachable, which is exactly why the selector exists.
	usePod := deviceName == "" && deviceID == ""

	var client *tcpclient.Client
	if usePod {
		spec, err := g.resolveConnection()
		if err != nil {
			return err
		}
		if err := spec.RequireWifi("deregister"); err != nil {
			return err
		}
		client = &tcpclient.Client{Addr: spec.Addr}
	}

	tokenPath, err := resolveTokenPath(tokenFile)
	if err != nil {
		return fmt.Errorf("resolve token path: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	api := serverapi.New(serverURL)
	tokens, err := ensureTokens(ctx, api, tokenPath)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	log.Printf("auth: signed in as user %s", tokens.UserID)

	target := deviceID
	label := deviceID
	if target == "" {
		var pubKey string
		if usePod {
			idCtx, idCancel := context.WithTimeout(ctx, 30*time.Second)
			pubKey, err = client.IdentityPublic(idCtx)
			idCancel()
			if err != nil {
				return fmt.Errorf("fetch device public key: %w", err)
			}
		}
		listCtx, listCancel := context.WithTimeout(ctx, 30*time.Second)
		devices, lErr := api.ListDevices(listCtx, tokens.AccessToken)
		listCancel()
		if lErr != nil {
			return fmt.Errorf("list devices: %w", lErr)
		}
		dev, fErr := findDeviceToDeregister(devices, pubKey, deviceName)
		if fErr != nil {
			return fErr
		}
		target, label = dev.ID, dev.Name
	}

	degCtx, degCancel := context.WithTimeout(ctx, 30*time.Second)
	device, err := api.DeregisterDevice(degCtx, tokens.AccessToken, target)
	degCancel()
	if err != nil {
		return fmt.Errorf("deregister device: %w", err)
	}
	if strings.TrimSpace(device.Name) != "" {
		label = device.Name
	}
	log.Printf("device: deregistered name=%s id=%s (data retained)", label, device.ID)

	// Wipe the pod-side cloud provisioning so the firmware stops reconnecting. `cloud_clear`
	// (not `cloud_set`) is the right tool: it drops the stored endpoint AND the now-dead
	// device_id, leaving the pod ready for a fresh `benchpod register` — for this or any other
	// account. Best-effort: the server-side deregistration already stands and the server now
	// refuses this device, so a failure here is a warning, not a reason to fail the command.
	if usePod && !keepPodConfig {
		setCtx, setCancel := context.WithTimeout(ctx, 30*time.Second)
		_, err = client.Command(setCtx, map[string]any{"cmd": "cloud_clear"})
		setCancel()
		if err != nil {
			log.Printf("warning: could not clear the bench pod's cloud configuration: %v", err)
			log.Printf("the pod will keep trying to connect and be refused; re-run with the pod reachable, or clear it by hand")
		} else {
			log.Printf("provisioned: bench pod cloud configuration cleared")
		}
	}

	fmt.Fprintf(os.Stderr, "Deregistered bench pod %q (device %s). Its data is kept; register it again to restore it.\n",
		label, device.ID)
	return nil
}

// findDeviceToDeregister picks the device to detach out of the account's device list, by public
// key when the pod could be asked for one (the identity that survives renames and re-addressing)
// and by name otherwise. It refuses to guess: an unmatched selector is an error, so a mistyped
// name can never deregister some other pod.
func findDeviceToDeregister(devices []serverapi.DeviceResponse, publicKey, name string) (serverapi.DeviceResponse, error) {
	if publicKey != "" {
		for _, d := range devices {
			if strings.TrimSpace(d.PublicKey) == publicKey {
				return d, nil
			}
		}
		return serverapi.DeviceResponse{}, errors.New(
			"the attached bench pod is not registered to this account (no device matches its public key)")
	}
	for _, d := range devices {
		if d.Name == name {
			return d, nil
		}
	}
	return serverapi.DeviceResponse{}, fmt.Errorf("no device named %q is registered to this account", name)
}

// parseServerEndpoint derives host, port, and a TLS flag from --server-url so the bench pod can
// open the connection itself. https/wss → TLS (default :443); http/ws → plaintext (default :80),
// which is handy for local bring-up against a non-TLS server.
func parseServerEndpoint(raw string) (host string, port int, tls bool, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, false, err
	}
	host = u.Hostname()
	if host == "" {
		return "", 0, false, fmt.Errorf("no host in %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		tls = true
	case "http", "ws":
		tls = false
	default:
		return "", 0, false, fmt.Errorf("unsupported scheme %q (use http/https/ws/wss)", u.Scheme)
	}
	if p := u.Port(); p != "" {
		port, err = strconv.Atoi(p)
		if err != nil {
			return "", 0, false, fmt.Errorf("invalid port %q: %w", p, err)
		}
	} else if tls {
		port = 443
	} else {
		port = 80
	}
	return host, port, tls, nil
}

// ── set-connection ───────────────────────────────────────────────────────────

func newSetConnectionCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:     "set-connection <addr|device|serial>",
		Aliases: []string{"set-bench-pod"},
		Short:   `Store the default connection: a TCP address, a serial device path, or "serial"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runSetConnection(g, args[0])
		},
	}
}

func runSetConnection(g *globalFlags, arg string) error {
	target := strings.TrimSpace(arg)
	if target == "" {
		return errors.New("connection target cannot be empty")
	}
	// Validate now (and report the inferred transport) rather than failing later.
	spec, err := classifyConnection(target)
	if err != nil {
		return err
	}
	cfgPath, err := resolveConfigPath(g.configFile)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	cfg, err := benchpodconfig.Load(cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg == nil {
		cfg = &benchpodconfig.Config{}
	}
	cfg.Connection = target
	cfg.BenchPodAddr = "" // Connection is the source of truth; drop the legacy field.
	if err := benchpodconfig.Save(cfgPath, cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Default connection set to %s (saved to %s).\n", describeConn(spec), cfgPath)
	return nil
}
