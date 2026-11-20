package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── lan-tls ─────────────────────────────────────────────────────────────────
//
// The pod's LAN API over TLS (benchpod-firmware docs/design/lan-tls.md): the certificate on port
// 8443, the HTTPS-only setting and SCPI. The client side (TLS by default, pinning) is
// internal/tcpclient/tls.go.
//
// The pod takes changes (https-only, scpi, regenerate, set-cert) over its USB network adapter or
// from the cloud only, never over Ethernet or Wi-Fi (BP0042), TLS or not.

const (
	lanTLSMissing   = "this pod's firmware has no LAN TLS; update its firmware with `benchpod flash-self`"
	lanTLSMaxUpload = 7664 // the pod's limit on the certificate + key bundle (LAN_TLS_UPLOAD_MAX)
)

// lanTLSReply is the data of the pod's lan_tls reply.
type lanTLSReply struct {
	Port      int      `json:"port"`
	HTTPSOnly bool     `json:"https_only"`
	SCPI      bool     `json:"scpi"`
	Sessions  int      `json:"sessions"`
	Origin    string   `json:"origin"`
	Key       string   `json:"key"`
	Subject   string   `json:"subject"`
	SAN       []string `json:"san"`
	NotAfter  string   `json:"not_after"`
	SHA256    string   `json:"sha256"`
	Error     string   `json:"error"`
}

func newLanTLSCmd(g *globalFlags) *cobra.Command {
	var pemOut bool
	run := func(fn func(out, warn io.Writer) error) error {
		out, closeOut, err := resolveOutput(g.outputFilename)
		if err != nil {
			return fmt.Errorf("lan-tls: open output: %w", err)
		}
		defer closeOut()
		return fn(out, os.Stderr)
	}
	show := func(_ *cobra.Command, _ []string) error {
		return run(func(out, warn io.Writer) error {
			if pemOut {
				return runLanTLSPEM(g, out)
			}
			return runLanTLS(g, nil, out, warn)
		})
	}
	root := &cobra.Command{
		Use:   "lan-tls [show]",
		Short: "Show or change the pod's LAN TLS certificate, HTTPS-only and SCPI",
		Long: "Firmware 4.0 serves its API on Ethernet and Wi-Fi on port 8443 with TLS only;\n" +
			"plaintext 8080 answers on its USB network adapter. benchpod connects to a LAN pod\n" +
			"with TLS and pins the pod's certificate the first time (trust on first use, in\n" +
			"known_pods.json next to the CLI config): nothing to set up. --tls-ca verifies a\n" +
			"certificate signed by your own CA instead of pinning.\n\n" +
			"  show         the certificate, its SHA-256, the settings and whether it is pinned here\n" +
			"  https-only   on (the default): the LAN takes TLS only; off: plaintext 8080 on the LAN too\n" +
			"  scpi         on: SCPI answers on the pod's TCP ports (off by default)\n" +
			"  regenerate   a new key and self-signed certificate\n" +
			"  set-cert     install your own certificate and key\n" +
			"  trust        pin the certificate the pod presents now\n" +
			"  forget       drop the pin for this pod\n\n" +
			"https-only, scpi, regenerate and set-cert only work over USB (--connection usb,\n" +
			"the pod's USB network adapter) or through embeddedci.com (--connection\n" +
			"embeddedci:<name>): the pod refuses them from Ethernet and Wi-Fi, TLS or not. The\n" +
			"pod's USB console has the same switches (`lan-tls https-only on|off`, `lan-tls scpi on|off`).",
		Args: cobra.NoArgs,
		RunE: show,
	}
	showCmd := &cobra.Command{Use: "show", Short: "Show the pod's LAN TLS certificate and settings", Args: cobra.NoArgs, RunE: show}
	showCmd.Flags().BoolVar(&pemOut, "pem", false, "print the certificate the pod presents, PEM (for socat cafile=, curl --cacert)")
	root.Flags().BoolVar(&pemOut, "pem", false, "print the certificate the pod presents, PEM")

	var certFile, keyFile, expect string
	setCert := &cobra.Command{
		Use:   "set-cert --cert <cert.pem> --key <key.pem>",
		Short: "Install your own certificate and private key for port 8443",
		Long: "Install a certificate (optionally followed by its chain) and its unencrypted\n" +
			"private key (EC P-256/P-384 or RSA 2048-4096). The pod checks both and that the\n" +
			"key matches before it stores them; new TLS connections use it. Over the pod's USB\n" +
			"network adapter only (--connection usb). This computer's pin is updated.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(func(out, warn io.Writer) error { return runLanTLSSetCert(g, certFile, keyFile, out, warn) })
		},
	}
	setCert.Flags().StringVar(&certFile, "cert", "", "PEM certificate file (with its chain, if any)")
	setCert.Flags().StringVar(&keyFile, "key", "", "PEM private key file, not encrypted")
	_ = setCert.MarkFlagRequired("cert")
	_ = setCert.MarkFlagRequired("key")

	trust := &cobra.Command{
		Use:   "trust",
		Short: "Pin the certificate the pod presents now (after a deliberate change)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return run(func(out, _ io.Writer) error { return runLanTLSTrust(g, expect, out) })
		},
	}
	trust.Flags().StringVar(&expect, "sha256", "", "only trust a certificate with this SHA-256 (from the pod's console `lan-tls` or the web app)")

	onOffCmd := func(use, key, short string) *cobra.Command {
		return &cobra.Command{
			Use:       use + " <on|off>",
			Short:     short + " (over USB or embeddedci.com only)",
			Args:      cobra.ExactArgs(1),
			ValidArgs: []string{"on", "off"},
			RunE: func(_ *cobra.Command, args []string) error {
				v := strings.ToLower(strings.TrimSpace(args[0]))
				if v != "on" && v != "off" {
					return usageErrorf("lan-tls %s: %q is not on or off", use, args[0])
				}
				return run(func(out, warn io.Writer) error {
					return runLanTLS(g, map[string]any{key: v == "on"}, out, warn)
				})
			},
		}
	}
	root.AddCommand(showCmd,
		onOffCmd("https-only", "https_only", "Take only TLS on Ethernet and Wi-Fi (on, the default) or plaintext 8080 too (off)"),
		onOffCmd("scpi", "scpi", "Turn SCPI on the pod's TCP ports on or off (off by default)"),
		&cobra.Command{
			Use:   "regenerate",
			Short: "Make a new key and self-signed certificate on the pod",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				return run(func(out, warn io.Writer) error {
					return runLanTLS(g, map[string]any{"regenerate": true}, out, warn)
				})
			},
		}, setCert, trust, &cobra.Command{
			Use:   "forget",
			Short: "Drop this computer's pin for the pod",
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				return run(func(out, _ io.Writer) error { return runLanTLSForget(g, out) })
			},
		})
	return root
}

// lanTLSError turns a refusal into one clear line.
func lanTLSError(what string, err error) error {
	if pe, ok := tcpclient.AsPodError(err); ok {
		if isUnknownCommand(err) {
			return errors.New(lanTLSMissing)
		}
		return refusedError(what, pe)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// runLanTLS sends lan_tls with the given change (nil: show) and prints the result.
func runLanTLS(g *globalFlags, change map[string]any, out, warn io.Writer) error {
	ctx, cancel, t, err := g.podClient("lan-tls", 30*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	req := map[string]any{"cmd": "lan_tls"}
	for k, v := range change {
		req[k] = v
	}
	raw, err := t.client.Command(ctx, req)
	if err != nil {
		return lanTLSError("lan-tls", err)
	}
	var rep lanTLSReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("lan-tls: unexpected reply: %s", strings.TrimSpace(string(raw)))
	}
	addr := ""
	if !t.spec.IsCloud() {
		addr = t.spec.Addr
	}
	// This computer made the change: pin the new certificate for the address it used.
	if change["regenerate"] == true && addr != "" && rep.SHA256 != "" && !tcpclient.IsUSBLinkAddr(hostOf(addr)) {
		if err := tcpclient.SetPinFingerprint(addr, rep.SHA256, rep.Subject); err != nil {
			fmt.Fprintf(warn, "Could not update the pin: %v\n", err)
		}
	}
	printLanTLS(out, t.label, addr, rep, lastTLS(t))
	if hs, ok := change["https_only"].(bool); ok && !hs {
		fmt.Fprintln(warn, "Plaintext port 8080 now answers on Ethernet and Wi-Fi too: anyone on the LAN can read and drive the pod without TLS.")
	}
	if sc, ok := change["scpi"].(bool); ok && sc && rep.HTTPSOnly {
		fmt.Fprintln(warn, "SCPI answers on 8443 (TLS) and on plaintext 8080; on the LAN 8080 takes SCPI only while https-only is on.")
	}
	return nil
}

func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return h
}

func lastTLS(t *podTarget) string {
	if c, ok := t.client.(*tcpclient.Client); ok {
		if c.LastDialTLS() {
			return "TLS"
		}
		return "plaintext"
	}
	return "embeddedci.com"
}

func printLanTLS(out io.Writer, label, addr string, r lanTLSReply, via string) {
	onOff := map[bool]string{true: "on", false: "off"}
	fmt.Fprintf(out, "LAN TLS of %s (connected over %s)\n", label, via)
	fmt.Fprintf(out, "  port        %d\n", r.Port)
	fmt.Fprintf(out, "  https-only  %s\n", onOff[r.HTTPSOnly])
	fmt.Fprintf(out, "  scpi        %s\n", onOff[r.SCPI])
	fmt.Fprintf(out, "  sessions    %d\n", r.Sessions)
	if r.SHA256 == "" {
		fmt.Fprintf(out, "  certificate none (%s)\n", valueOr(r.Error, "unknown"))
		return
	}
	fmt.Fprintf(out, "  certificate %s (%s, %s), valid to %s\n", r.Subject, r.Origin, r.Key, r.NotAfter)
	if len(r.SAN) > 0 {
		fmt.Fprintf(out, "  names       %s\n", strings.Join(r.SAN, ", "))
	}
	fmt.Fprintf(out, "  sha256      %s\n", r.SHA256)
	if addr == "" {
		return
	}
	switch p, ok := tcpclient.GetPin(addr); {
	case tcpclient.IsUSBLinkAddr(hostOf(addr)):
		fmt.Fprintln(out, "  pinned      not for the USB link (plaintext there)")
	case !ok:
		fmt.Fprintln(out, "  pinned      no")
	case strings.EqualFold(p.SHA256, r.SHA256):
		fmt.Fprintf(out, "  pinned      yes, for %s\n", tcpclient.TLSAddr(addr))
	default:
		fmt.Fprintf(out, "  pinned      DIFFERENT: %s (run `benchpod lan-tls trust` if the change was yours)\n", p.SHA256)
	}
}

// lanTLSAddr is the pod's network address for the commands that talk TLS to it directly.
func lanTLSAddr(g *globalFlags, what string) (string, error) {
	spec, err := g.networkTarget("lan-tls " + what)
	if err != nil {
		return "", err
	}
	if spec.IsCloud() {
		return "", fmt.Errorf("lan-tls %s: needs the pod's network address (--connection <address>), not embeddedci.com", what)
	}
	return spec.Addr, nil
}

func runLanTLSPEM(g *globalFlags, out io.Writer) error {
	addr, err := lanTLSAddr(g, "show --pem")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(20*time.Second))
	defer cancel()
	cert, err := tcpclient.PeerCertificate(ctx, addr)
	if err != nil {
		return fmt.Errorf("lan-tls show --pem: %w", err)
	}
	return pem.Encode(out, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func runLanTLSTrust(g *globalFlags, expect string, out io.Writer) error {
	addr, err := lanTLSAddr(g, "trust")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(20*time.Second))
	defer cancel()
	cert, err := tcpclient.PeerCertificate(ctx, addr)
	if err != nil {
		return fmt.Errorf("lan-tls trust: %w", err)
	}
	fp := tcpclient.Fingerprint(cert)
	if e := strings.ToLower(strings.TrimSpace(expect)); e != "" && e != fp {
		return fmt.Errorf("lan-tls trust: the pod presents sha256 %s, not %s; not trusted", fp, e)
	}
	if err := tcpclient.SetPin(addr, cert); err != nil {
		return fmt.Errorf("lan-tls trust: %w", err)
	}
	fmt.Fprintf(out, "Trusted %s for %s\n  sha256 %s\n", cert.Subject.String(), tcpclient.TLSAddr(addr), fp)
	if expect == "" {
		fmt.Fprintln(out, "Compare it with the pod's console (`lan-tls`) or the web app's pod Settings.")
	}
	return nil
}

func runLanTLSForget(g *globalFlags, out io.Writer) error {
	addr, err := lanTLSAddr(g, "forget")
	if err != nil {
		return err
	}
	ok, err := tcpclient.DeletePin(addr)
	if err != nil {
		return fmt.Errorf("lan-tls forget: %w", err)
	}
	if !ok {
		fmt.Fprintf(out, "No pin for %s\n", tcpclient.TLSAddr(addr))
		return nil
	}
	fmt.Fprintf(out, "Forgot the pin for %s; the next TLS connection pins what the pod presents\n", tcpclient.TLSAddr(addr))
	return nil
}

// checkCertKey reads the two files, checks them like the pod will, and returns the bundle the
// pod takes (certificate chain, then the key) and the leaf certificate.
func checkCertKey(certFile, keyFile string) ([]byte, *x509.Certificate, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, nil, err
	}
	if blk, _ := pem.Decode(keyPEM); blk == nil {
		return nil, nil, fmt.Errorf("%s: not a PEM private key", keyFile)
	} else if strings.Contains(blk.Type, "ENCRYPTED") || blk.Headers["Proc-Type"] != "" {
		return nil, nil, fmt.Errorf("%s: the key is encrypted; the pod needs it without a passphrase", keyFile)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("certificate and key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	var bundle []byte
	for _, der := range pair.Certificate {
		bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	bundle = append(bundle, keyPEM...)
	if len(bundle) > lanTLSMaxUpload {
		return nil, nil, fmt.Errorf("certificate chain and key are %d bytes; the pod takes at most %d", len(bundle), lanTLSMaxUpload)
	}
	return bundle, leaf, nil
}

func runLanTLSSetCert(g *globalFlags, certFile, keyFile string, out, warn io.Writer) error {
	bundle, leaf, err := checkCertKey(certFile, keyFile)
	if err != nil {
		return fmt.Errorf("lan-tls set-cert: %w", err)
	}
	addr, err := lanTLSAddr(g, "set-cert")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(60*time.Second))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	lan, err := openLAN(ctx, addr)
	if err != nil {
		return fmt.Errorf("lan-tls set-cert: %w", err)
	}
	defer lan.s.Close()
	if err := lan.cmd(ctx, map[string]any{"cmd": "lan_tls"}, &lanTLSReply{}); err != nil {
		return lanTLSError("lan-tls set-cert", err)
	}
	fmt.Fprintf(warn, "Uploading %d bytes to %s ...\n", len(bundle), addr)
	if err := uploadOTALAN(ctx, lan, bundle, "tls"); err != nil {
		if !lan.s.Broken() {
			_ = lan.cmd(ctx, map[string]any{"cmd": "ota_abort"}, nil)
		}
		return lanTLSError("lan-tls set-cert", err)
	}
	var rep lanTLSReply
	if err := lan.cmd(ctx, map[string]any{"cmd": "lan_tls"}, &rep); err != nil {
		return lanTLSError("lan-tls set-cert", err)
	}
	if want := tcpclient.Fingerprint(leaf); !strings.EqualFold(rep.SHA256, want) {
		return fmt.Errorf("lan-tls set-cert: the pod reports sha256 %s after the install, expected %s", rep.SHA256, want)
	}
	if !tcpclient.IsUSBLinkAddr(hostOf(addr)) {
		if err := tcpclient.SetPin(addr, leaf); err != nil {
			fmt.Fprintf(warn, "Could not update the pin: %v\n", err)
		}
	}
	printLanTLS(out, addr, addr, rep, map[bool]string{true: "TLS", false: "plaintext"}[lan.s.TLS()])
	return nil
}
