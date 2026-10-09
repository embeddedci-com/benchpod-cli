package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── register (register device by public key + provision direct cloud connection) ──

func newRegisterCmd(g *globalFlags) *cobra.Command {
	var serverURL, tokenFile, deviceName, podHost string
	var insecureSkipVerify bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register the bench pod and provision it to connect directly to the server",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRegister(g, registerOptions{
				serverURL: serverURL, tokenFile: tokenFile, deviceName: deviceName, podHost: podHost,
				insecureSkipVerify: insecureSkipVerify, wait: wait,
			})
		},
	}
	cmd.Flags().StringVar(&serverURL, "server-url", "https://www.embeddedci.com", "embeddedci-server base URL")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "path to token cache (default: ~/.config/benchpod-cli/token.json)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "device name, URL-safe (default: the pod's own name, e.g. benchpod-a1b2c3)")
	cmd.Flags().StringVar(&podHost, "pod-host", "",
		"host the pod connects to (default: api.embeddedci.com for embeddedci.com, else the --server-url host)")
	cmd.Flags().DurationVar(&wait, "wait", 30*time.Second, "how long to wait for the pod to connect to the server (0 = don't wait)")
	cmd.Flags().BoolVar(&insecureSkipVerify, "insecure-skip-verify", false,
		"provision the pod to skip TLS certificate verification (bring-up only; default verifies against the ESP x509 cert bundle)")
	return cmd
}

type registerOptions struct {
	serverURL, tokenFile, deviceName, podHost string
	insecureSkipVerify                        bool
	wait                                      time.Duration
}

// deviceNameFromPublicKey returns the name the pod advertises over mDNS ("benchpod-a1b2c3"): the
// first three bytes of its Ed25519 public key in hex, exactly as the firmware derives it
// (net_server.c mdns_build_identity). Unlike the IP it survives DHCP handing the address to
// another pod. Returns "" when the key does not decode or is all zeros (identity not ready).
func deviceNameFromPublicKey(publicKey string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(publicKey), "="))
	if err != nil || len(raw) < 3 || (raw[0]|raw[1]|raw[2]) == 0 {
		return ""
	}
	return fmt.Sprintf("benchpod-%02x%02x%02x", raw[0], raw[1], raw[2])
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
// devices (one per invocation/address). With opts.wait > 0 it then waits for the pod to report
// the connection up, and fails (the registration stands) with the firmware's reason if it does not.
func runRegister(g *globalFlags, opts registerOptions) error {
	serverURL, tokenFile, deviceName := opts.serverURL, opts.tokenFile, opts.deviceName
	if strings.TrimSpace(serverURL) == "" {
		return errors.New("--server-url cannot be empty")
	}
	host, port, tls, err := parseServerEndpoint(serverURL)
	if err != nil {
		return fmt.Errorf("parse --server-url: %w", err)
	}
	host = podHostFor(host, opts.podHost)
	// Verify the server cert by default when using TLS; --insecure-skip-verify opts out for bring-up.
	verify := tls && !opts.insecureSkipVerify
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
	log.Printf("auth: signed in as %s", tokens.Who())

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
		// Default to the pod's own stable name (as mDNS advertises it). The IP is only a fallback:
		// DHCP can hand it to another pod, which would then collide with this one's name. The
		// server requires URL-safe names ([A-Za-z0-9._-], no colons), hence the host without port.
		if name = deviceNameFromPublicKey(pubKey); name == "" {
			name = defaultDeviceNameFromAddr(addr)
		}
	}

	regCtx, regCancel := context.WithTimeout(ctx, 30*time.Second)
	device, err := api.RegisterDevice(regCtx, tokens.AccessToken, name, pubKey, nil)
	regCancel()
	if err != nil {
		return fmt.Errorf("register device: %w", err)
	}
	log.Printf("device: registered name=%s id=%s", device.Name, device.ID)
	if device.Name != name {
		// A re-register of an already-registered pod keeps its (possibly user-edited) name.
		fmt.Fprintf(os.Stderr, "This pod is already registered as %q. Rename it on the BenchPod page.\n", device.Name)
	}

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

	if opts.wait <= 0 {
		fmt.Fprintf(os.Stderr, "Registered bench pod %q (device %s).\n", device.Name, device.ID)
		return nil
	}

	// The pod connects on its own now; wait until it says so, so "registered" means usable.
	status, pollErr := waitCloudConnected(ctx, func(ctx context.Context) (cloudStatus, error) {
		return readCloudStatus(ctx, client)
	}, opts.wait, time.Second)
	if status.State == "connected" {
		fmt.Fprintf(os.Stderr, "Registered bench pod %q and it is connected.\n", device.Name)
		fmt.Fprintf(os.Stderr, "Run tests against it: pytest --benchpod-connection=embeddedci:%s\n", device.Name)
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Fprint(os.Stderr, describeNotConnected(device.Name, host, opts.wait, status, pollErr))
	return fmt.Errorf("bench pod did not connect within %s", opts.wait)
}

// cloudStatus is the part of the firmware's `cloud_status` reply that register reads.
// LastError is nil on firmware that predates the field.
type cloudStatus struct {
	State     string  `json:"state"`
	LastError *string `json:"last_error"`
}

func readCloudStatus(ctx context.Context, client *tcpclient.Client) (cloudStatus, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := client.Command(cctx, map[string]any{"cmd": "cloud_status"})
	if err != nil {
		return cloudStatus{}, err
	}
	var st cloudStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return cloudStatus{}, fmt.Errorf("parse cloud_status: %w", err)
	}
	return st, nil
}

// waitCloudConnected polls until the pod reports state "connected" or wait elapses. It returns
// the last status read and the last poll error (a transient read failure does not stop it).
func waitCloudConnected(ctx context.Context, poll func(context.Context) (cloudStatus, error), wait, interval time.Duration) (cloudStatus, error) {
	deadline := time.Now().Add(wait)
	var last cloudStatus
	var lastErr error
	for {
		st, err := poll(ctx)
		if err == nil {
			last, lastErr = st, nil
			if st.State == "connected" {
				return last, nil
			}
		} else {
			lastErr = err
		}
		if !time.Now().Add(interval).Before(deadline) {
			return last, lastErr
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// describeNotConnected explains a register whose pod never connected: the registration stands,
// what the pod last reported, and the likely fix.
func describeNotConnected(name, host string, wait time.Duration, st cloudStatus, pollErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Registered bench pod %q, but it did not connect to %s within %s.\n", name, host, wait)
	fmt.Fprintln(&b, "The registration stands; the pod keeps retrying on its own.")
	if st.State != "" {
		fmt.Fprintf(&b, "  state: %s\n", st.State)
	} else if pollErr != nil {
		fmt.Fprintf(&b, "  could not read the pod's status: %v\n", pollErr)
	}
	lastError := ""
	if st.LastError != nil {
		lastError = strings.TrimSpace(*st.LastError)
		if lastError != "" {
			fmt.Fprintf(&b, "  last error: %s\n", lastError)
		}
	}
	if hint := cloudErrorHint(st, lastError); hint != "" {
		fmt.Fprintf(&b, "  fix: %s\n", hint)
	}
	return b.String()
}

// cloudErrorHint maps the firmware's last_error (cloud_client.c cl_set_error) to the likely fix.
func cloudErrorHint(st cloudStatus, lastError string) string {
	switch {
	case st.State == "waiting-wifi":
		return "the pod has no network. Check its Ethernet cable, or join Wi-Fi with `benchpod set-wifi`."
	case st.State == "disabled":
		return "the pod's cloud connection is off. Run `benchpod register` again."
	case st.State != "" && st.LastError == nil:
		return "update the pod firmware (`benchpod flash-self`) to see why."
	case strings.HasPrefix(lastError, "dns"):
		return "the pod cannot resolve the server name. Check the DNS server its network hands out."
	case strings.HasPrefix(lastError, "tcp"):
		return "the pod cannot reach the server. Check that its network allows outbound HTTPS."
	case strings.HasPrefix(lastError, "tls certificate"):
		return "the server's certificate is not trusted. Check --server-url, or update the pod firmware."
	case strings.HasPrefix(lastError, "tls"):
		return "the secure connection failed. Check --server-url and that nothing on the network intercepts HTTPS."
	case strings.Contains(lastError, "refused the device (HTTP 404)"):
		return "the server does not know this device. Check that --server-url is the server you registered with."
	case strings.Contains(lastError, "refused the device (HTTP 403)"):
		return "the device was deregistered. Run `benchpod register` again."
	case strings.HasPrefix(lastError, "server refused"), strings.HasPrefix(lastError, "websocket"):
		return "the server rejected the connection. Run `benchpod register` again, or update the pod firmware."
	case strings.HasPrefix(lastError, "connection lost"), strings.HasPrefix(lastError, "no reply"):
		return "the link to the server is unreliable. Check the pod's network."
	}
	return ""
}

// parseServerEndpoint derives host, port, and a TLS flag from --server-url so the bench pod can
// open the connection itself. https/wss → TLS (default :443); http/ws → plaintext (default :80),
// which is handy for local bring-up against a non-TLS server.
// podHostFor is the host the pod is provisioned to connect to: override when set, otherwise
// api.embeddedci.com for the embeddedci.com service (its own name, so the pods' path can be moved
// on or off Cloudflare by DNS alone, without reprovisioning them), otherwise the server's host.
func podHostFor(serverHost, override string) string {
	if o := strings.TrimSpace(override); o != "" {
		return strings.ToLower(o)
	}
	switch strings.ToLower(serverHost) {
	case "www.embeddedci.com", "embeddedci.com":
		return "api.embeddedci.com"
	}
	return serverHost
}

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
