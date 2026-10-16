package tcpclient

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// A ticket saved by one run resumes in the next (a fresh in-memory cache stands for a new
// process), and forgetSessions drops it.
func TestSessionTicketSurvivesTheProcess(t *testing.T) {
	PinStorePath = filepath.Join(t.TempDir(), "known_pods.json")
	defer func() { PinStorePath = "" }()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pod"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	srv := &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MaxVersion: tls.VersionTLS12}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _ = c.(*tls.Conn).Handshake(); c.Read(make([]byte, 1)); c.Close() }(c)
		}
	}()
	dial := func() bool {
		cfg, _ := tlsConfig(ln.Addr().String(), true)
		c, err := tls.Dial("tcp", ln.Addr().String(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte{0}) // the TLS 1.2 ticket arrives with the handshake
		return c.ConnectionState().DidResume
	}
	if dial() {
		t.Fatal("first connection resumed")
	}
	sessionCache.(*fileSessionCache).mem = tls.NewLRUClientSessionCache(32) // a new process
	if !dial() {
		t.Fatal("the saved ticket did not resume")
	}
	forgetSessions()
	if dial() {
		t.Fatal("resumed after forgetSessions")
	}
}
