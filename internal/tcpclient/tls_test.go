package tcpclient

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanDial(t *testing.T) {
	cases := []struct {
		addr  string
		mode  TLSMode
		want  dialPlan
		label string
	}{
		{"192.168.1.5:8080", TLSAuto, dialPlan{tlsAddr: "192.168.1.5:8443", plainAddr: "192.168.1.5:8080", fallback: true}, "default port: TLS first"},
		{"192.168.1.5:8443", TLSAuto, dialPlan{tlsAddr: "192.168.1.5:8443"}, "8443: TLS only"},
		{"192.168.1.5:9000", TLSAuto, dialPlan{plainAddr: "192.168.1.5:9000"}, "other port: plaintext"},
		{"10.237.8.85:8080", TLSAuto, dialPlan{plainAddr: "10.237.8.85:8080"}, "USB link: plaintext"},
		{"192.168.1.5:8080", TLSOff, dialPlan{plainAddr: "192.168.1.5:8080"}, "off"},
		{"192.168.1.5:8443", TLSOff, dialPlan{plainAddr: "192.168.1.5:8443"}, "off wins over the port"},
		{"192.168.1.5:8080", TLSOn, dialPlan{tlsAddr: "192.168.1.5:8443"}, "on: 8080 means the pod"},
		{"10.237.8.85:8080", TLSOn, dialPlan{tlsAddr: "10.237.8.85:8443"}, "on over USB"},
		{"pod.local:9443", TLSOn, dialPlan{tlsAddr: "pod.local:9443"}, "on, other port as given"},
	}
	for _, c := range cases {
		if got := planDial(c.addr, c.mode); got != c.want {
			t.Errorf("%s: planDial(%q) = %+v, want %+v", c.label, c.addr, got, c.want)
		}
	}
}

// tlsPod is a TLS JSON server that answers every line with {"status":"ok","data":"pong"}.
func tlsPod(t *testing.T) (addr string, cert *x509.Certificate, setCert func()) {
	t.Helper()
	mk := func() tls.Certificate {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "benchpod-test"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"benchpod-test.local"}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}
	cur := mk()
	cfg := &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cur, nil }}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					if _, err := r.ReadString('\n'); err != nil {
						return
					}
					c.Write([]byte(`{"status":"ok","data":"pong"}` + "\n"))
				}
			}(c)
		}
	}()
	leaf, _ := x509.ParseCertificate(cur.Certificate[0])
	return ln.Addr().String(), leaf, func() { cur = mk() }
}

func TestTLSPinOnFirstUseThenMismatch(t *testing.T) {
	PinStorePath = filepath.Join(t.TempDir(), "known_pods.json")
	defer func() { PinStorePath = "" }()
	addr, cert, rotate := tlsPod(t)
	ctx := context.Background()
	c := &Client{Addr: addr, TLS: TLSOn, DialTimeout: 2 * time.Second}

	if _, err := c.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
		t.Fatalf("first TLS command: %v", err)
	}
	if !c.LastDialTLS() {
		t.Fatal("LastDialTLS false after a TLS command")
	}
	p, ok := GetPin(addr)
	if !ok || p.SHA256 != Fingerprint(cert) {
		t.Fatalf("pin after first use = %+v, %v; want %s", p, ok, Fingerprint(cert))
	}
	// Same certificate: fine. The key is the TLS address, so the 8080 form finds it too.
	if _, err := c.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
		t.Fatalf("second TLS command: %v", err)
	}

	rotate()
	_, err := c.Command(ctx, map[string]any{"cmd": "ping"})
	var pm *PinMismatchError
	if !errors.As(err, &pm) {
		t.Fatalf("new certificate: err = %v, want a PinMismatchError", err)
	}
	if pm.Pinned != Fingerprint(cert) {
		t.Errorf("mismatch names pinned %s, want %s", pm.Pinned, Fingerprint(cert))
	}

	// Trusting the new one (what `benchpod lan-tls trust` does) makes it work again.
	got, err := PeerCertificate(ctx, addr)
	if err != nil || Fingerprint(got) != pm.Got {
		t.Fatalf("PeerCertificate = %v; want the new certificate %s", err, pm.Got)
	}
	if err := SetPin(addr, got); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
		t.Fatalf("after trusting the new certificate: %v", err)
	}
	if ok, err := DeletePin(addr); !ok || err != nil {
		t.Fatalf("DeletePin = %v, %v", ok, err)
	}
	if _, ok := GetPin(addr); ok {
		t.Fatal("pin still there after DeletePin")
	}
}

func TestAutoFallsBackOnlyWithoutPin(t *testing.T) {
	PinStorePath = filepath.Join(t.TempDir(), "known_pods.json")
	defer func() { PinStorePath = "" }()
	// A plaintext pod on 127.0.0.1:<port>; nothing on 8443 there. planDial's fallback only
	// exists for port 8080, so drive the fallback rule directly.
	p := planDial("127.0.0.1:8080", TLSAuto)
	if !p.fallback || p.tlsAddr != "127.0.0.1:8443" {
		t.Fatalf("plan = %+v", p)
	}
	if pinned(p.tlsAddr) {
		t.Fatal("pinned in an empty store")
	}
	if err := SetPinFingerprint("127.0.0.1:8080", "ab", ""); err != nil {
		t.Fatal(err)
	}
	if !pinned(p.tlsAddr) {
		t.Fatal("a pin for the pod is keyed by its TLS address")
	}
}
