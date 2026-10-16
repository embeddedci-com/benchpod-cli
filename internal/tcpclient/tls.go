package tcpclient

// The LAN API over TLS (firmware 3.9.3+, docs/design/lan-tls.md in benchpod-firmware).
//
// The pod serves the same API on TCP 8443 with TLS 1.2 and its own certificate (self-signed on
// first boot, or one the user installed). How a Client connects:
//
//   - port 8443: TLS only.
//   - port 8080 (the default): TLS on 8443 first; when 8443 refuses the connection (firmware
//     without LAN TLS) and the client has no pin for that pod, plaintext 8080 as before. With a
//     pin, a pod that stops answering TLS is an error, never a silent downgrade.
//   - any other port, and the pod's USB network adapter (10.237.0.0/16, a physical link): as
//     given, plaintext.
//
// Trust: the SHA-256 of the pod's certificate (DER) is pinned on first use per address in
// known_pods.json next to the CLI's config (shared with the embeddedci SDK). A different
// certificate later is a PinMismatchError. With TLSCAFile set, the certificate is verified against
// those roots and the host name instead, and nothing is pinned.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TLSMode picks how a Client connects.
type TLSMode int

const (
	TLSDefault TLSMode = iota // DefaultTLSMode
	TLSAuto                   // TLS when the pod has it (see the package comment)
	TLSOn                     // TLS only
	TLSOff                    // plaintext only
)

// ParseTLSMode reads "auto", "on" or "off".
func ParseTLSMode(s string) (TLSMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return TLSAuto, nil
	case "on", "true", "yes":
		return TLSOn, nil
	case "off", "false", "no":
		return TLSOff, nil
	}
	return TLSDefault, fmt.Errorf("tls: %q is not auto, on or off", s)
}

const (
	PlainPort = "8080"
	TLSPort   = "8443"
	// tlsHandshakeTimeout bounds the handshake: the pod does ECDHE + ECDSA in software.
	tlsHandshakeTimeout = 10 * time.Second
)

// Process-wide settings, set once by the CLI from its flags and environment.
var (
	DefaultTLSMode = TLSAuto
	TLSCAFile      string // PEM roots; when set, verify chain and host name instead of pinning
	PinStorePath   string // "" = DefaultPinStorePath()
)

// usbLink: the pod's USB network adapter subnet (firmware usb_dhcpd.c).
var usbLink = &net.IPNet{IP: net.IPv4(10, 237, 0, 0), Mask: net.CIDRMask(16, 32)}

// IsUSBLinkAddr reports whether host is on the pod's USB network adapter.
func IsUSBLinkAddr(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && usbLink.Contains(ip)
}

// TLSAddr is the address a TLS connection to addr's pod goes to: host:8443 for the default
// port (or none), else addr as given. It is also the key of the pod's pin.
func TLSAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return net.JoinHostPort(addr, TLSPort)
	}
	if port == PlainPort {
		return net.JoinHostPort(host, TLSPort)
	}
	return addr
}

// noTLS remembers pods that refused 8443 in this process, so each command does not probe again.
var noTLS sync.Map // addr -> true

// dialPlan is how dial reaches addr.
type dialPlan struct {
	tlsAddr   string // "" = no TLS attempt
	plainAddr string // "" = no plaintext
	fallback  bool   // plaintext only when TLS is refused and nothing is pinned
}

func planDial(addr string, mode TLSMode) dialPlan {
	if mode == TLSDefault {
		mode = DefaultTLSMode
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return dialPlan{plainAddr: addr}
	}
	switch {
	case mode == TLSOff:
		return dialPlan{plainAddr: addr}
	case port == TLSPort:
		return dialPlan{tlsAddr: addr}
	case mode == TLSOn && port == PlainPort:
		return dialPlan{tlsAddr: net.JoinHostPort(host, TLSPort)}
	case mode == TLSOn:
		return dialPlan{tlsAddr: addr}
	case port != PlainPort || IsUSBLinkAddr(host):
		return dialPlan{plainAddr: addr}
	}
	return dialPlan{tlsAddr: net.JoinHostPort(host, TLSPort), plainAddr: addr, fallback: true}
}

// dial connects to c.Addr as the TLS mode says, within budget. The returned connection is a
// *tls.Conn or a *net.TCPConn; c.lastTLS records which.
func (c *Client) dial(ctx context.Context, budget time.Duration) (net.Conn, error) {
	p := planDial(strings.TrimSpace(c.Addr), c.TLS)
	if p.fallback {
		if _, known := noTLS.Load(p.plainAddr); known && !pinned(p.tlsAddr) {
			p.tlsAddr = ""
		}
	}
	if p.tlsAddr == "" {
		c.lastTLS = false
		return dialWithRetry(ctx, "tcp", p.plainAddr, budget, dialAttemptTimeout, dialRetryBackoff)
	}
	if p.fallback && !pinned(p.tlsAddr) {
		// One quick probe: a pod without LAN TLS answers 8443 with a reset.
		raw, err := dialWithRetry(ctx, "tcp", p.tlsAddr, dialAttemptTimeout, dialAttemptTimeout, dialRetryBackoff)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				noTLS.Store(p.plainAddr, true)
			}
			c.lastTLS = false
			return dialWithRetry(ctx, "tcp", p.plainAddr, budget, dialAttemptTimeout, dialRetryBackoff)
		}
		return c.handshake(ctx, raw, p.tlsAddr)
	}
	raw, err := dialWithRetry(ctx, "tcp", p.tlsAddr, budget, dialAttemptTimeout, dialRetryBackoff)
	if err != nil {
		return nil, err
	}
	return c.handshake(ctx, raw, p.tlsAddr)
}

func (c *Client) handshake(ctx context.Context, raw net.Conn, addr string) (net.Conn, error) {
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	cfg, err := tlsConfig(addr, true)
	if err != nil {
		raw.Close()
		return nil, err
	}
	conn := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		raw.Close()
		var pm *PinMismatchError
		if errors.As(err, &pm) {
			return nil, pm
		}
		return nil, &dialError{addr: addr, err: fmt.Errorf("TLS handshake: %w", err)}
	}
	c.lastTLS = true
	return conn, nil
}

// LastDialTLS reports whether the Client's most recent connection used TLS.
func (c *Client) LastDialTLS() bool { return c.lastTLS }

var sessionCache = tls.NewLRUClientSessionCache(32)

// tlsConfig is the client configuration for addr (host:8443). pin: check (and on first use
// record) the pin; false accepts any certificate (PeerCertificate, for showing and trusting).
func tlsConfig(addr string, pin bool) (*tls.Config, error) {
	host, _, _ := net.SplitHostPort(addr)
	// Resume sessions (the pod issues TLS 1.2 tickets): a full handshake costs the pod ~460 ms of
	// ECDHE + ECDSA, a resumed one ~20 ms, and every command opens a connection. A resumed
	// session proves the same server as the full handshake whose certificate was pinned;
	// VerifyConnection still runs on it with that certificate.
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ClientSessionCache: sessionCache}
	if net.ParseIP(host) == nil {
		cfg.ServerName = host
	}
	if TLSCAFile != "" && pin {
		pemData, err := os.ReadFile(TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("TLS CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("TLS CA file %s: no PEM certificates", TLSCAFile)
		}
		cfg.RootCAs = pool
		cfg.ServerName = host // an IP is checked against the certificate's IP SANs
		return cfg, nil
	}
	cfg.InsecureSkipVerify = true // the pin below is the check
	if pin {
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the pod sent no certificate")
			}
			return checkPin(addr, cs.PeerCertificates[0])
		}
	}
	return cfg, nil
}

// Fingerprint is the lowercase hex SHA-256 of a certificate's DER, the value the pod reports
// as lan_tls sha256.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// PeerCertificate connects to addr's TLS port and returns the pod's certificate without
// checking or recording a pin.
func PeerCertificate(ctx context.Context, addr string) (*x509.Certificate, error) {
	taddr := TLSAddr(addr)
	raw, err := dialWithRetry(ctx, "tcp", taddr, DefaultDialTimeout, dialAttemptTimeout, dialRetryBackoff)
	if err != nil {
		return nil, err
	}
	defer raw.Close()
	cfg, _ := tlsConfig(taddr, false)
	conn := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, tlsHandshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		return nil, &dialError{addr: taddr, err: fmt.Errorf("TLS handshake: %w", err)}
	}
	defer conn.Close()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, errors.New("the pod sent no certificate")
	}
	return certs[0], nil
}

// ── pins ────────────────────────────────────────────────────────────────────

// PinMismatchError: the pod presented a certificate other than the pinned one.
type PinMismatchError struct {
	Addr   string // host:8443
	Pinned string
	Got    string
}

func (e *PinMismatchError) Error() string {
	host, _, _ := net.SplitHostPort(e.Addr)
	return fmt.Sprintf("the pod at %s presented TLS certificate sha256 %s, but %s is pinned for it. "+
		"If its certificate was replaced or regenerated, trust the new one with "+
		"`benchpod lan-tls trust --connection %s`; otherwise something may be intercepting the connection",
		e.Addr, short(e.Got), short(e.Pinned), host)
}

func short(fp string) string {
	if len(fp) > 16 {
		return fp[:16] + "..."
	}
	return fp
}

// Pin is one known pod.
type Pin struct {
	SHA256  string `json:"sha256"`
	Subject string `json:"subject,omitempty"`
	Added   string `json:"added,omitempty"`
}

type pinFile struct {
	Version int            `json:"version"`
	Pods    map[string]Pin `json:"pods"`
}

var pinMu sync.Mutex

// DefaultPinStorePath: known_pods.json in the CLI's config directory ($XDG_CONFIG_HOME or
// ~/.config, then benchpod-cli/).
func DefaultPinStorePath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "benchpod-cli", "known_pods.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("could not resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "benchpod-cli", "known_pods.json"), nil
}

func pinPath() (string, error) {
	if PinStorePath != "" {
		return PinStorePath, nil
	}
	return DefaultPinStorePath()
}

func loadPins() (*pinFile, string, error) {
	path, err := pinPath()
	if err != nil {
		return nil, "", err
	}
	f := &pinFile{Version: 1, Pods: map[string]Pin{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	if f.Pods == nil {
		f.Pods = map[string]Pin{}
	}
	return f, path, nil
}

func savePins(f *pinFile, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_pods-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// GetPin returns the pin for addr (any port; the TLS address is the key), if there is one.
func GetPin(addr string) (Pin, bool) {
	pinMu.Lock()
	defer pinMu.Unlock()
	f, _, err := loadPins()
	if err != nil {
		return Pin{}, false
	}
	p, ok := f.Pods[TLSAddr(addr)]
	return p, ok
}

func pinned(tlsAddr string) bool {
	_, ok := GetPin(tlsAddr)
	return ok
}

// SetPin records cert as the trusted certificate for addr's pod.
func SetPin(addr string, cert *x509.Certificate) error {
	return SetPinFingerprint(addr, Fingerprint(cert), cert.Subject.String())
}

// SetPinFingerprint records a fingerprint (lowercase hex) for addr's pod.
func SetPinFingerprint(addr, sha, subject string) error {
	pinMu.Lock()
	defer pinMu.Unlock()
	f, path, err := loadPins()
	if err != nil {
		return err
	}
	f.Pods[TLSAddr(addr)] = Pin{SHA256: strings.ToLower(sha), Subject: subject, Added: time.Now().UTC().Format(time.RFC3339)}
	return savePins(f, path)
}

// DeletePin forgets addr's pod. false when there was no pin.
func DeletePin(addr string) (bool, error) {
	pinMu.Lock()
	defer pinMu.Unlock()
	f, path, err := loadPins()
	if err != nil {
		return false, err
	}
	key := TLSAddr(addr)
	if _, ok := f.Pods[key]; !ok {
		return false, nil
	}
	delete(f.Pods, key)
	return true, savePins(f, path)
}

// checkPin: no pin yet records this certificate (trust on first use); a pin must match.
func checkPin(addr string, cert *x509.Certificate) error {
	got := Fingerprint(cert)
	pinMu.Lock()
	defer pinMu.Unlock()
	f, path, err := loadPins()
	if err != nil {
		return fmt.Errorf("read the TLS pins: %w", err)
	}
	key := TLSAddr(addr)
	if p, ok := f.Pods[key]; ok {
		if !strings.EqualFold(p.SHA256, got) {
			return &PinMismatchError{Addr: key, Pinned: p.SHA256, Got: got}
		}
		return nil
	}
	f.Pods[key] = Pin{SHA256: got, Subject: cert.Subject.String(), Added: time.Now().UTC().Format(time.RFC3339)}
	if err := savePins(f, path); err != nil {
		return fmt.Errorf("save the TLS pin: %w", err)
	}
	return nil
}
