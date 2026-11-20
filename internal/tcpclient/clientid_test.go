package tcpclient

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func withClientDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := ClientIdentityDir
	ClientIdentityDir = dir
	ResetClientIdentity()
	t.Setenv("BENCHPOD_CLIENT_KEY_PEM", "")
	t.Setenv("BENCHPOD_CLIENT_CERT", "")
	t.Setenv("BENCHPOD_CLIENT_KEY", "")
	t.Cleanup(func() { ClientIdentityDir = old; ResetClientIdentity() })
	return dir
}

func TestClientIdentityMadeOnceAndKept(t *testing.T) {
	dir := withClientDir(t)
	a, err := ClientKeyHash()
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, clientKeyFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", err, st)
	}
	ResetClientIdentity() // a new process reads the same files
	b, err := ClientKeyHash()
	if err != nil || a != b || len(a) != 64 {
		t.Fatalf("hash %q then %q (%v)", a, b, err)
	}
	c, _ := ClientIdentity()
	if c.Leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("usage %v", c.Leaf.ExtKeyUsage)
	}
	sum := sha256.Sum256(c.Leaf.RawSubjectPublicKeyInfo)
	if hex.EncodeToString(sum[:]) != a {
		t.Fatal("ClientKeyHash is not the SPKI hash")
	}
}

func TestClientIdentityFromEnvPEM(t *testing.T) {
	withClientDir(t)
	certPEM, keyPEM, err := NewClientIdentityPEM("ci-runner-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BENCHPOD_CLIENT_KEY_PEM", string(certPEM)+string(keyPEM))
	c, err := ClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pair, _ := tls.X509KeyPair(certPEM, keyPEM)
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	if SPKIHash(c.Leaf) != SPKIHash(leaf) || c.Leaf.Subject.CommonName != "ci-runner-1" {
		t.Fatal("not the environment's identity")
	}
	entries, _ := os.ReadDir(ClientIdentityDir)
	if len(entries) != 0 {
		t.Fatalf("wrote files with an environment identity: %v", entries)
	}
}

func TestClientIdentityHalfEnv(t *testing.T) {
	withClientDir(t)
	t.Setenv("BENCHPOD_CLIENT_CERT", "/nope.crt")
	if _, err := ClientIdentity(); err == nil {
		t.Fatal("one of the two file variables was accepted")
	}
}

func TestSanitizePairingName(t *testing.T) {
	for in, want := range map[string]string{
		"Edward's MacBook Pro":                  "Edward's-MacBook-Pro",
		"a\"b\\c":                               "abc",
		"":                                      "computer",
		"build-runner-with-a-very-long-name-01": "build-runner-with-a-ver",
		"  x  ":                                 "x",
	} {
		if got := SanitizePairingName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
