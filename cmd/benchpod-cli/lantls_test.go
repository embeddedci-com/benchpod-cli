package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCertKey(t *testing.T, dir, name string) (certFile, keyFile string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{name + ".local"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	return
}

func TestCheckCertKey(t *testing.T) {
	dir := t.TempDir()
	c1, k1 := writeCertKey(t, dir, "pod1")
	_, k2 := writeCertKey(t, dir, "pod2")

	bundle, leaf, err := checkCertKey(c1, k1)
	if err != nil {
		t.Fatalf("matching pair refused: %v", err)
	}
	if leaf.Subject.CommonName != "pod1" {
		t.Errorf("leaf = %v", leaf.Subject)
	}
	if !strings.Contains(string(bundle), "BEGIN CERTIFICATE") || !strings.Contains(string(bundle), "BEGIN EC PRIVATE KEY") {
		t.Errorf("bundle lacks the certificate or the key:\n%s", bundle)
	}
	if _, _, err := checkCertKey(c1, k2); err == nil {
		t.Error("a key that does not match the certificate was accepted")
	}
	enc := filepath.Join(dir, "enc.key")
	os.WriteFile(enc, []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----\nAAAA\n-----END ENCRYPTED PRIVATE KEY-----\n"), 0o600)
	if _, _, err := checkCertKey(c1, enc); err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Errorf("encrypted key: err = %v", err)
	}
}
