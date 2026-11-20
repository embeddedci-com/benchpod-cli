package tcpclient

// This computer's TLS client identity for LAN pairing (firmware 4.0.1+, docs/design/lan-pairing.md
// in benchpod-firmware). The pod asks every client on port 8443 for a certificate and trusts the
// ones whose public key is on its list of paired computers; pairing puts the key there, over the
// pod's USB cable or from embeddedci.com.
//
// One key per user, shared with the embeddedci SDK and the MCP server: client.key and client.crt
// next to known_pods.json ($XDG_CONFIG_HOME/benchpod-cli or ~/.config/benchpod-cli). They are made
// on first use. A CI runner without a home directory to keep them in passes its own:
//
//	BENCHPOD_CLIENT_KEY_PEM   the certificate and key PEM (a CI secret; `benchpod lan-clients add --new-key` prints one)
//	BENCHPOD_CLIENT_CERT, BENCHPOD_CLIENT_KEY   files

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	clientKeyFile  = "client.key"
	clientCertFile = "client.crt"
)

// ClientIdentityDir overrides the directory client.key and client.crt live in (tests).
var ClientIdentityDir string

var (
	clientMu   sync.Mutex
	clientCert *tls.Certificate
)

func clientDir() (string, error) {
	if ClientIdentityDir != "" {
		return ClientIdentityDir, nil
	}
	p, err := pinPath()
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// ClientIdentity is this computer's certificate and key, made on first use (see the file comment).
func ClientIdentity() (*tls.Certificate, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if clientCert != nil {
		return clientCert, nil
	}
	c, err := loadClientIdentity()
	if err != nil {
		return nil, err
	}
	clientCert = c
	return c, nil
}

// ResetClientIdentity forgets the cached identity (tests, and after the files change).
func ResetClientIdentity() {
	clientMu.Lock()
	clientCert = nil
	clientMu.Unlock()
}

func loadClientIdentity() (*tls.Certificate, error) {
	if pemData := strings.TrimSpace(os.Getenv("BENCHPOD_CLIENT_KEY_PEM")); pemData != "" {
		c, err := tls.X509KeyPair([]byte(pemData), []byte(pemData))
		if err != nil {
			return nil, fmt.Errorf("BENCHPOD_CLIENT_KEY_PEM: %w", err)
		}
		return withLeaf(&c)
	}
	certFile, keyFile := os.Getenv("BENCHPOD_CLIENT_CERT"), os.Getenv("BENCHPOD_CLIENT_KEY")
	if certFile != "" || keyFile != "" {
		if certFile == "" || keyFile == "" {
			return nil, errors.New("set both BENCHPOD_CLIENT_CERT and BENCHPOD_CLIENT_KEY")
		}
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		return withLeaf(&c)
	}
	dir, err := clientDir()
	if err != nil {
		return nil, err
	}
	certPath, keyPath := filepath.Join(dir, clientCertFile), filepath.Join(dir, clientKeyFile)
	if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return withLeaf(&c)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("client certificate %s: %w", certPath, err)
	}
	certPEM, keyPEM, err := NewClientIdentityPEM(defaultClientCN())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	// The key first and exclusively: two processes starting at once must not end up with two
	// different keys. The loser reads the winner's files.
	if err := writeExclusive(keyPath, keyPEM, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return waitForIdentity(certPath, keyPath)
		}
		return nil, fmt.Errorf("client key: %w", err)
	}
	if err := writeExclusive(certPath, certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return withLeaf(&c)
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

func waitForIdentity(certPath, keyPath string) (*tls.Certificate, error) {
	var last error
	for i := 0; i < 50; i++ {
		c, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err == nil {
			return withLeaf(&c)
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("client certificate: %w", last)
}

func withLeaf(c *tls.Certificate) (*tls.Certificate, error) {
	if c.Leaf == nil && len(c.Certificate) > 0 {
		leaf, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("client certificate: %w", err)
		}
		c.Leaf = leaf
	}
	return c, nil
}

func defaultClientCN() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "computer"
	}
	return "benchpod client " + h
}

// NewClientIdentityPEM makes a new EC P-256 key and a self-signed client certificate for it:
// the certificate PEM and the key PEM (PKCS#8).
func NewClientIdentityPEM(cn string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(100, 0, 0), // the pod checks the key, not the dates
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), nil
}

// SPKIHash is what the pod lists a paired computer by: the lowercase hex SHA-256 of the
// certificate's DER SubjectPublicKeyInfo.
func SPKIHash(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// ClientKeyHash is SPKIHash of this computer's identity.
func ClientKeyHash() (string, error) {
	c, err := ClientIdentity()
	if err != nil {
		return "", err
	}
	return SPKIHash(c.Leaf), nil
}

// PairingName is the name a computer pairs under: its host name, cut to what the pod takes
// (1-23 printable characters; no spaces, so the USB console takes it too).
func PairingName() string {
	h, _ := os.Hostname()
	return SanitizePairingName(strings.TrimSuffix(h, ".local"))
}

// SanitizePairingName makes s a name the pod takes.
func SanitizePairingName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ' || r == '\t':
			b.WriteByte('-')
		case r < 0x21 || r > 0x7e || r == '"' || r == '\\':
			// dropped
		default:
			b.WriteRune(r)
		}
		if b.Len() >= 23 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 23 {
		out = out[:23]
	}
	if out == "" {
		out = "computer"
	}
	return out
}

// clientCertificate is the tls.Config hook: this computer's identity, or none when it cannot be
// read or made (the pod then treats the connection as unpaired and says so).
func clientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c, err := ClientIdentity()
	if err != nil {
		return &tls.Certificate{}, nil
	}
	return c, nil
}
