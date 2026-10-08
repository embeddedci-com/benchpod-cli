package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── cloud ca / cloud proxy ──────────────────────────────────────────────────
//
// The pod's company CA certificate and HTTP proxy for its cloud link (firmware
// docs/design/cloud-hardening.md, section 3). Corporate networks that inspect TLS re-sign
// HTTPS with a company root, and some only reach the internet through a proxy; the pod needs
// both to reach embeddedci.com there.
//
// --connection picks the transport: the USB console (`ca`, `proxy`, ... and the upload path
// with target "ca") or the LAN JSON API (`cloud_ca`, `cloud_proxy`, ota_* with target "ca").
// Both settings are T2 commands, so a pod whose LAN policy is locked refuses changes on the LAN.
// Newer firmware goes further: it refuses every change to them from the LAN, whatever the LAN
// policy ("cloud_ca: change it from the cloud or the USB console"); only reading works there.

const (
	caMaxPEM     = 16 * 1024   // the pod's limit on the stored PEM
	caMaxFile    = 1024 * 1024 // refuse to even read a larger file
	caLANChunk   = 768         // raw bytes per ota_data line (as the hwe2e LAN OTA sends)
	caMissing    = "this firmware has no company CA support; update the pod's firmware"
	proxyMissing = "this firmware has no HTTP proxy support; update the pod's firmware"
	lanGateHint  = "the pod only takes this change over USB or from the cloud, never from the LAN; run it over the pod's USB console with --connection usb"
)

// lanGateRefusal is the tail of the firmware's refusal of a cloud link change from the LAN
// (pod_policy_cloud_link_gate): "cloud_ca: ..." or "cloud_proxy: ...".
const lanGateRefusal = ": change it from the cloud or the USB console"

// isLANGateRefusal reports whether msg is the firmware's refusal to change the company CA or
// the HTTP proxy from the LAN.
func isLANGateRefusal(msg string) bool {
	verb, ok := strings.CutSuffix(strings.TrimSpace(msg), lanGateRefusal)
	return ok && (verb == "cloud_ca" || verb == "cloud_proxy")
}

func newCloudCmd(g *globalFlags) *cobra.Command {
	root := &cobra.Command{
		Use:   "cloud",
		Short: "Configure how the pod reaches embeddedci.com (company CA, HTTP proxy)",
		Long: "Configure the pod's link to embeddedci.com on networks that need it.\n\n" +
			"  ca     a company root certificate, for networks whose proxy inspects TLS\n" +
			"  proxy  an HTTP proxy the pod tunnels its cloud connection through\n\n" +
			"Both work over the pod's USB console (--connection usb). The LAN\n" +
			"(--connection <address>) can show them; current firmware refuses changes from\n" +
			"the LAN, so change them over USB. The pod reconnects to the cloud after a change.",
		Args: cobra.NoArgs,
	}
	root.AddCommand(newCloudCACmd(g), newCloudProxyCmd(g))
	return root
}

func newCloudCACmd(g *globalFlags) *cobra.Command {
	run := func(fn func(out, warn io.Writer) error) error {
		out, closeOut, err := resolveOutput(g.outputFilename)
		if err != nil {
			return fmt.Errorf("cloud ca: open output: %w", err)
		}
		defer closeOut()
		return fn(out, os.Stderr)
	}
	show := func(_ *cobra.Command, _ []string) error {
		return run(func(out, warn io.Writer) error { return runCloudCA(g, "show", "", out, warn) })
	}
	root := &cobra.Command{
		Use:   "ca [show]",
		Short: "Show, set or clear the pod's company CA certificate",
		Long: "Show, set or clear the company CA certificate the pod trusts for its cloud link.\n\n" +
			"Networks with a TLS-inspecting proxy re-sign every HTTPS connection with a\n" +
			"company root. The pod trusts only its built-in roots, so its cloud link fails\n" +
			"there until it also trusts that company root. The certificate is used in\n" +
			"addition to the built-in roots; host name and chain checks stay on.\n\n" +
			"`set` takes a PEM file with one or more CA certificates (at most 16 KB),\n" +
			"checks it, and uploads only its CERTIFICATE blocks.",
		Args: cobra.NoArgs,
		RunE: show,
	}
	root.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the company CA certificates the pod holds",
		Args:  cobra.NoArgs,
		RunE:  show,
	}, &cobra.Command{
		Use:   "set <file.pem>",
		Short: "Check a PEM file and store its CA certificates on the pod",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return run(func(out, warn io.Writer) error { return runCloudCA(g, "set", args[0], out, warn) })
		},
	}, &cobra.Command{
		Use:   "clear",
		Short: "Remove the company CA; the pod trusts only its built-in roots again",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(func(out, warn io.Writer) error { return runCloudCA(g, "clear", "", out, warn) })
		},
	})
	return root
}

type proxyFlags struct {
	user          string
	password      string
	passwordStdin bool
}

func newCloudProxyCmd(g *globalFlags) *cobra.Command {
	run := func(fn func(out, warn io.Writer) error) error {
		out, closeOut, err := resolveOutput(g.outputFilename)
		if err != nil {
			return fmt.Errorf("cloud proxy: open output: %w", err)
		}
		defer closeOut()
		return fn(out, os.Stderr)
	}
	show := func(_ *cobra.Command, _ []string) error {
		return run(func(out, warn io.Writer) error { return runCloudProxy(g, "show", "", "", "", out, warn) })
	}
	root := &cobra.Command{
		Use:   "proxy [show]",
		Short: "Show, set or clear the HTTP proxy the pod uses to reach the cloud",
		Long: "Show, set or clear the HTTP proxy the pod reaches embeddedci.com through.\n\n" +
			"With a proxy set, the pod opens its cloud connection with an HTTP CONNECT to the\n" +
			"proxy (with Basic authentication when a user is set) and runs TLS to\n" +
			"embeddedci.com inside it. The pod never reports the password back.",
		Args: cobra.NoArgs,
		RunE: show,
	}
	pf := &proxyFlags{}
	set := &cobra.Command{
		Use:   "set <host:port>",
		Short: "Set the pod's HTTP proxy",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			password := ""
			if pf.user != "" {
				// Before the prompt, so nobody types the password without knowing.
				warnPlainLANPassword(g, os.Stderr)
				pw, err := resolvePassword(pf.password, pf.passwordStdin, "Proxy password")
				if err != nil {
					return fmt.Errorf("cloud proxy set: %w", err)
				}
				password = pw
			} else if pf.password != "" || pf.passwordStdin {
				return errors.New("cloud proxy set: --password needs --user")
			}
			return run(func(out, warn io.Writer) error {
				return runCloudProxy(g, "set", args[0], pf.user, password, out, warn)
			})
		},
	}
	set.Flags().StringVar(&pf.user, "user", "", "proxy user name (Basic authentication)")
	set.Flags().StringVar(&pf.password, "password", "", "proxy password (insecure: visible in shell history); omit to be prompted")
	set.Flags().BoolVar(&pf.passwordStdin, "password-stdin", false, "read the proxy password from the first line of stdin")
	root.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the pod's HTTP proxy",
		Args:  cobra.NoArgs,
		RunE:  show,
	}, set, &cobra.Command{
		Use:   "clear",
		Short: "Remove the HTTP proxy; the pod connects to the cloud directly",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(func(out, warn io.Writer) error { return runCloudProxy(g, "clear", "", "", "", out, warn) })
		},
	})
	return root
}

// plainLANPasswordWarning is printed before a proxy password goes to the pod over the LAN.
const plainLANPasswordWarning = "Warning: the pod's LAN API is plain TCP, so the proxy password crosses the network unencrypted.\n" +
	"Set it over the pod's USB console (--connection usb) instead; current firmware refuses this\n" +
	"change from the LAN anyway."

// warnPlainLANPassword warns when the proxy password would go to the pod over the LAN. It stays
// quiet when the target is USB or cannot be resolved (the command then fails with the reason).
func warnPlainLANPassword(g *globalFlags, warn io.Writer) {
	if spec, err := cloudCfgTarget(g, "cloud proxy set"); err == nil && spec.IsWifi() {
		fmt.Fprintln(warn, plainLANPasswordWarning)
	}
}

// cloudCfgTarget resolves --connection for the cloud ca/proxy commands: a serial device ("" =
// auto-detect) or a LAN address.
func cloudCfgTarget(g *globalFlags, what string) (spec ConnSpec, err error) {
	raw, err := g.rawConnection()
	if err != nil {
		return ConnSpec{}, err
	}
	if raw == "" {
		return ConnSpec{}, fmt.Errorf("%s: no pod selected; pass --connection usb or --connection <address>", what)
	}
	return classifyConnection(raw)
}

// ── CA certificate checks ───────────────────────────────────────────────────

// caCert is one certificate of a PEM file, as checked locally.
type caCert struct {
	Subject string
	SHA256  string // hex SHA-256 of the DER certificate, as the pod reports it
	IsCA    bool   // basic constraints CA, or key usage cert sign
}

// checkCAPEM checks a PEM file for the pod: one or more CERTIFICATE blocks that parse as X.509,
// nothing else, at most caMaxPEM bytes once re-encoded. It returns the certificates and the PEM
// to upload (the CERTIFICATE blocks only, without the text around them).
func checkCAPEM(data []byte) ([]caCert, []byte, error) {
	var certs []caCert
	var upload bytes.Buffer
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, nil, fmt.Errorf("the file holds a %q block; only CERTIFICATE blocks belong in a CA file", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("certificate %d does not parse: %v", len(certs)+1, err)
		}
		sum := sha256.Sum256(c.Raw)
		certs = append(certs, caCert{
			Subject: c.Subject.String(),
			SHA256:  hex.EncodeToString(sum[:]),
			IsCA:    c.IsCA || c.KeyUsage&x509.KeyUsageCertSign != 0,
		})
		_ = pem.Encode(&upload, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	if len(certs) == 0 {
		return nil, nil, errors.New("no CERTIFICATE block found; pass a PEM file (-----BEGIN CERTIFICATE-----)")
	}
	if upload.Len() > caMaxPEM {
		return nil, nil, fmt.Errorf("the certificates take %d bytes as PEM; the pod holds at most %d", upload.Len(), caMaxPEM)
	}
	return certs, upload.Bytes(), nil
}

func readCAFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, caMaxFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > caMaxFile {
		return nil, fmt.Errorf("%s is larger than 1 MB; that is not a CA certificate file", path)
	}
	return data, nil
}

// printCACerts writes the certificate list the pod reports.
func printCACerts(out io.Writer, label string, certs []serialconsole.CACert, changed bool) {
	verb := ":"
	if changed {
		verb = " is now"
	}
	if len(certs) == 0 {
		fmt.Fprintf(out, "Company CA of %s%s none (the pod trusts only its built-in roots)\n", label, verb)
		return
	}
	fmt.Fprintf(out, "Company CA of %s%s %s\n", label, verb, plural(len(certs), "certificate", "certificates"))
	for _, c := range certs {
		fmt.Fprintf(out, "  %s\n    SHA-256 %s\n", c.Subject, c.SHA256)
	}
}

func runCloudCA(g *globalFlags, action, file string, out, warn io.Writer) error {
	what := "cloud ca " + action
	var local []caCert
	var upload []byte
	if action == "set" {
		data, err := readCAFile(file)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		local, upload, err = checkCAPEM(data)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", what, file, err)
		}
		fmt.Fprintf(warn, "%s: %s\n", file, plural(len(local), "certificate", "certificates"))
		for _, c := range local {
			fmt.Fprintf(warn, "  %s\n    SHA-256 %s\n", c.Subject, c.SHA256)
			if !c.IsCA {
				fmt.Fprintf(warn, "  Warning: %s is not marked as a CA (no CA basic constraint or cert-sign key usage); the pod may refuse it\n", c.Subject)
			}
		}
	}

	spec, err := cloudCfgTarget(g, what)
	if err != nil {
		return err
	}
	var label string
	var certs []serialconsole.CACert
	if spec.IsSerial() {
		label, certs, err = cloudCAUSB(g, spec.Device, action, upload, warn)
	} else {
		label = spec.Addr
		certs, err = cloudCALAN(g, spec.Addr, action, upload, warn)
	}
	if err != nil {
		return cloudCfgError(what, caMissing, !spec.IsSerial(), err)
	}

	printCACerts(out, label, certs, action != "show")
	switch action {
	case "set":
		if !sameCerts(local, certs) {
			fmt.Fprintln(warn, "Warning: the pod reports other certificates than the file holds")
		}
		fmt.Fprintln(warn, "The pod reconnects to the cloud, trusting this CA in addition to its built-in roots.")
	case "clear":
		fmt.Fprintln(warn, "The pod reconnects to the cloud, trusting only its built-in roots.")
	}
	return nil
}

func sameCerts(local []caCert, pod []serialconsole.CACert) bool {
	if len(local) != len(pod) {
		return false
	}
	for i := range local {
		if !strings.EqualFold(local[i].SHA256, pod[i].SHA256) {
			return false
		}
	}
	return true
}

// cloudCfgError turns a failed command into one clear line: old firmware, a locked LAN, a change
// the pod refuses from the LAN, or the pod's own reason.
func cloudCfgError(what, missing string, lan bool, err error) error {
	if errors.Is(err, serialconsole.ErrUnknownCommand) {
		return errors.New(missing)
	}
	msg := err.Error()
	if strings.Contains(msg, "unknown cmd") || strings.Contains(msg, "unknown target") {
		return errors.New(missing)
	}
	if pe, ok := tcpclient.AsPodError(err); ok {
		// A console refusal (Cmd set) came over USB, a JSON reply over the LAN.
		lan = lan && pe.Cmd == ""
		if lan && isLANGateRefusal(pe.Reason) {
			return refusedWithHint(what, pe, lanGateHint)
		}
		return refusedError(what, pe, lan)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// ── USB console ─────────────────────────────────────────────────────────────

// cloudConsole is the part of the USB console the cloud ca/proxy commands use.
type cloudConsole interface {
	CloudCA(ctx context.Context) ([]serialconsole.CACert, error)
	CloudCAClear(ctx context.Context) error
	CloudProxy(ctx context.Context) (serialconsole.ProxyConfig, error)
	CloudProxySet(ctx context.Context, addr, user, password string) (serialconsole.ProxyConfig, error)
	CloudProxyClear(ctx context.Context) (serialconsole.ProxyConfig, error)
	Upload(ctx context.Context, target string, data []byte, version int, progress func(done, total int)) error
	Close() error
}

// openCloudConsole opens the pod's USB console; tests replace it with a fake.
var openCloudConsole = func(g *globalFlags, device string, timeout time.Duration) (cloudConsole, string, context.Context, context.CancelFunc, error) {
	console, path, ctx, cancel, err := g.openSerialConsole(device, timeout)
	if err != nil {
		return nil, "", nil, cancel, err
	}
	return console, path, ctx, cancel, nil
}

func cloudCAUSB(g *globalFlags, device, action string, upload []byte, warn io.Writer) (string, []serialconsole.CACert, error) {
	console, path, ctx, cancel, err := openCloudConsole(g, device, g.effectiveTimeout(60*time.Second))
	if err != nil {
		return "", nil, err
	}
	defer cancel()
	defer console.Close()
	label := "the pod on " + path

	switch action {
	case "set":
		// Ask first, so old firmware gets the clear message rather than a refused upload.
		if _, err := console.CloudCA(ctx); err != nil {
			return label, nil, err
		}
		fmt.Fprintf(warn, "Uploading %d bytes to %s ...\n", len(upload), label)
		if err := console.Upload(ctx, "ca", upload, 0, nil); err != nil {
			return label, nil, err
		}
	case "clear":
		if err := console.CloudCAClear(ctx); err != nil {
			return label, nil, err
		}
	}
	certs, err := console.CloudCA(ctx)
	return label, certs, err
}

// ── LAN ─────────────────────────────────────────────────────────────────────

// lanSession runs JSON commands on one connection; error replies come back as *tcpclient.PodError.
type lanSession struct{ s *tcpclient.Session }

func (l lanSession) cmd(ctx context.Context, req map[string]any, out any) error {
	data, err := l.s.Command(ctx, req)
	if err != nil {
		if _, ok := tcpclient.AsPodError(err); ok || l.s.Broken() {
			return err
		}
		return &tcpclient.PodError{Reason: err.Error()}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected reply to %v: %s", req["cmd"], strings.TrimSpace(string(data)))
	}
	return nil
}

func openLAN(ctx context.Context, addr string) (lanSession, error) {
	s, err := (&tcpclient.Client{Addr: addr}).Open(ctx)
	if err != nil {
		return lanSession{}, err
	}
	return lanSession{s: s}, nil
}

type caReply struct {
	Present bool                   `json:"present"`
	Certs   []serialconsole.CACert `json:"certs"`
}

// otaReply is the pod's ota_* status.
type otaReply struct {
	State string `json:"state"`
	Error string `json:"error"`
}

func cloudCALAN(g *globalFlags, addr, action string, upload []byte, warn io.Writer) ([]serialconsole.CACert, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(60*time.Second))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	lan, err := openLAN(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer lan.s.Close()

	switch action {
	case "set":
		// Ask first, so old firmware gets the clear message rather than a refused upload.
		if err := lan.cmd(ctx, map[string]any{"cmd": "cloud_ca"}, &caReply{}); err != nil {
			return nil, err
		}
		fmt.Fprintf(warn, "Uploading %d bytes to %s ...\n", len(upload), addr)
		if err := uploadCALAN(ctx, lan, upload); err != nil {
			if !lan.s.Broken() {
				_ = lan.cmd(ctx, map[string]any{"cmd": "ota_abort"}, nil)
			}
			return nil, err
		}
	case "clear":
		if err := lan.cmd(ctx, map[string]any{"cmd": "cloud_ca", "clear": true}, nil); err != nil {
			return nil, err
		}
	}
	var rep caReply
	if err := lan.cmd(ctx, map[string]any{"cmd": "cloud_ca"}, &rep); err != nil {
		return nil, err
	}
	return rep.Certs, nil
}

// uploadCALAN stages the PEM as OTA target "ca", has the pod verify it, and commits it.
func uploadCALAN(ctx context.Context, lan lanSession, data []byte) error {
	sum := sha256.Sum256(data)
	if err := lan.cmd(ctx, map[string]any{"cmd": "ota_begin", "size": len(data),
		"sha256": hex.EncodeToString(sum[:]), "target": "ca"}, nil); err != nil {
		return err
	}
	for off := 0; off < len(data); off += caLANChunk {
		end := min(off+caLANChunk, len(data))
		if err := lan.cmd(ctx, map[string]any{"cmd": "ota_data", "offset": off,
			"data": base64.RawURLEncoding.EncodeToString(data[off:end])}, nil); err != nil {
			return err
		}
	}
	var st otaReply
	if err := lan.cmd(ctx, map[string]any{"cmd": "ota_end"}, &st); err != nil {
		return err
	}
	if st.State != "verified" {
		return &tcpclient.PodError{Reason: "verify: " + valueOr(st.Error, st.State)}
	}
	if err := lan.cmd(ctx, map[string]any{"cmd": "ota_commit"}, &st); err != nil {
		return err
	}
	if st.State == "error" {
		return &tcpclient.PodError{Reason: "commit: " + valueOr(st.Error, "failed")}
	}
	return nil
}

func valueOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ── proxy ───────────────────────────────────────────────────────────────────

// checkProxyAddr checks a host:port locally and returns it normalized.
func checkProxyAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if strings.Contains(addr, "://") {
		return "", fmt.Errorf("give the proxy as host:port, without a scheme (got %q)", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%q is not host:port (%v)", addr, err)
	}
	if host == "" || strings.ContainsAny(host, " \t/@?#") {
		return "", fmt.Errorf("%q has no usable host name", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q: the port must be a number from 1 to 65535", addr)
	}
	return net.JoinHostPort(strings.ToLower(host), port), nil
}

func describeProxy(p serialconsole.ProxyConfig) string {
	switch {
	case p.Addr == "":
		return "none (the pod connects to the cloud directly)"
	case p.Auth:
		return p.Addr + " (with a user and password)"
	default:
		return p.Addr + " (no authentication)"
	}
}

func runCloudProxy(g *globalFlags, action, addr, user, password string, out, warn io.Writer) error {
	what := "cloud proxy " + action
	if action == "set" {
		var err error
		if addr, err = checkProxyAddr(addr); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if (user == "") != (password == "") {
			return fmt.Errorf("%s: pass both --user and a password, or neither", what)
		}
	}
	spec, err := cloudCfgTarget(g, what)
	if err != nil {
		return err
	}
	var label string
	var p serialconsole.ProxyConfig
	if spec.IsSerial() {
		label, p, err = cloudProxyUSB(g, spec.Device, action, addr, user, password)
	} else {
		label = spec.Addr
		p, err = cloudProxyLAN(g, spec.Addr, action, addr, user, password)
	}
	if err != nil {
		err = cloudCfgError(what, proxyMissing, !spec.IsSerial(), err)
		if password != "" && strings.Contains(err.Error(), password) {
			err = errors.New(strings.ReplaceAll(err.Error(), password, "***"))
		}
		return err
	}
	verb := ":"
	if action != "show" {
		verb = " is now"
	}
	fmt.Fprintf(out, "HTTP proxy of %s%s %s\n", label, verb, describeProxy(p))
	if action != "show" {
		if p.Addr == "" {
			fmt.Fprintln(warn, "The pod reconnects to the cloud directly, without a proxy.")
		} else {
			fmt.Fprintf(warn, "The pod reconnects to the cloud through %s.\n", p.Addr)
		}
	}
	return nil
}

func cloudProxyUSB(g *globalFlags, device, action, addr, user, password string) (string, serialconsole.ProxyConfig, error) {
	console, path, ctx, cancel, err := openCloudConsole(g, device, g.effectiveTimeout(15*time.Second))
	if err != nil {
		return "", serialconsole.ProxyConfig{}, err
	}
	defer cancel()
	defer console.Close()
	label := "the pod on " + path
	var p serialconsole.ProxyConfig
	switch action {
	case "set":
		p, err = console.CloudProxySet(ctx, addr, user, password)
	case "clear":
		p, err = console.CloudProxyClear(ctx)
	default:
		p, err = console.CloudProxy(ctx)
	}
	return label, p, err
}

// proxyReply is the pod's JSON proxy report: {"host":"...","port":3128,"auth":true}, with no
// host when none is set.
type proxyReply struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	Auth bool   `json:"auth"`
}

func (r proxyReply) config() serialconsole.ProxyConfig {
	if r.Host == "" {
		return serialconsole.ProxyConfig{}
	}
	return serialconsole.ProxyConfig{Addr: net.JoinHostPort(r.Host, strconv.Itoa(r.Port)), Auth: r.Auth}
}

func cloudProxyLAN(g *globalFlags, addr, action, set, user, password string) (serialconsole.ProxyConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(15*time.Second))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	lan, err := openLAN(ctx, addr)
	if err != nil {
		return serialconsole.ProxyConfig{}, err
	}
	defer lan.s.Close()

	req := map[string]any{"cmd": "cloud_proxy"}
	switch action {
	case "set":
		req["set"] = set
		if user != "" {
			req["user"], req["password"] = user, password
		}
	case "clear":
		req["clear"] = true
	}
	var rep proxyReply
	if err := lan.cmd(ctx, req, &rep); err != nil {
		return serialconsole.ProxyConfig{}, err
	}
	if action != "show" {
		// Report what the pod holds now, not what the change's reply happened to carry.
		rep = proxyReply{}
		if err := lan.cmd(ctx, map[string]any{"cmd": "cloud_proxy"}, &rep); err != nil {
			return serialconsole.ProxyConfig{}, err
		}
	}
	return rep.config(), nil
}
