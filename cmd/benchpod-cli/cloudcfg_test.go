package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// testCertPEM makes a self-signed certificate; isCA sets the CA basic constraint and cert-sign
// key usage.
func testCertPEM(t *testing.T, cn string, isCA bool) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"Acme Corp"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemSHA(t *testing.T, p []byte) string {
	t.Helper()
	b, _ := pem.Decode(p)
	sum := sha256.Sum256(b.Bytes)
	return hex.EncodeToString(sum[:])
}

func tempPEM(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckCAPEM(t *testing.T) {
	root := testCertPEM(t, "Acme Root CA", true)
	leaf := testCertPEM(t, "proxy.acme.example", false)

	file := append([]byte("Bag Attributes\n    friendlyName: Acme\n"), root...)
	file = append(file, []byte("subject=/CN=leaf\n")...)
	file = append(file, leaf...)
	certs, upload, err := checkCAPEM(file)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if len(certs) != 2 || certs[0].Subject != "CN=Acme Root CA,O=Acme Corp" || !certs[0].IsCA || certs[1].IsCA {
		t.Fatalf("certs = %+v", certs)
	}
	if certs[0].SHA256 != pemSHA(t, root) {
		t.Fatalf("sha = %s", certs[0].SHA256)
	}
	if want := string(root) + string(leaf); string(upload) != want {
		t.Fatalf("upload keeps the text around the blocks:\n%s", upload)
	}

	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1, 2, 3}})
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"private key": {append(append([]byte{}, root...), key...), `"EC PRIVATE KEY" block`},
		"not pem":     {[]byte("hello"), "no CERTIFICATE block"},
		"der garbage": {pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x30, 0x03, 1, 2, 3}}), "certificate 1 does not parse"},
		"too big":     {bytes.Repeat(root, 30), "the pod holds at most 16384"},
	} {
		if _, _, err := checkCAPEM(tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// ── USB ─────────────────────────────────────────────────────────────────────

// fakeCloudConsole stands in for the pod's USB console.
func runCACapture(g *globalFlags, action, file string) (string, string, error) {
	var out, warn bytes.Buffer
	err := runCloudCA(g, action, file, &out, &warn)
	return out.String(), warn.String(), err
}

func runProxyCapture(g *globalFlags, action, addr, user, password string) (string, string, error) {
	var out, warn bytes.Buffer
	err := runCloudProxy(g, action, addr, user, password, &out, &warn)
	return out.String(), warn.String(), err
}

// Over USB the company CA and the proxy go over the pod's USB network adapter (JSON, the USB link
// may change them).
func TestCloudCAUSB(t *testing.T) {
	pod := &lanCfgPod{t: t}
	addr := pod.start()
	g := &globalFlags{connection: "usb", usbAddr: addr}

	out, _, err := runCACapture(g, "show", "")
	if err != nil || out != "Company CA of "+addr+": none (the pod trusts only its built-in roots)\n" {
		t.Fatalf("show = %q, %v", out, err)
	}
	root := testCertPEM(t, "Acme Root CA", true)
	out, warn, err := runCACapture(g, "set", tempPEM(t, "acme.pem", root))
	if err != nil || !strings.HasPrefix(out, "Company CA of "+addr+" is now 1 certificate\n") {
		t.Fatalf("set = %q, %v", out, err)
	}
	if !strings.Contains(warn, "reconnects to the cloud") || strings.Contains(warn, "Warning") {
		t.Fatalf("warn = %q", warn)
	}
	if !strings.Contains(pod.cmds(), "ota_begin,ota_data,ota_end,ota_commit") {
		t.Fatalf("requests = %s", pod.cmds())
	}
	out, _, err = runCACapture(g, "clear", "")
	if err != nil || !strings.HasSuffix(out, "is now none (the pod trusts only its built-in roots)\n") {
		t.Fatalf("clear = %q, %v", out, err)
	}
}

func TestCloudCANotMarkedCAWarns(t *testing.T) {
	pod := &lanCfgPod{t: t}
	g := &globalFlags{connection: "usb", usbAddr: pod.start()}
	_, warn, err := runCACapture(g, "set", tempPEM(t, "leaf.pem", testCertPEM(t, "leaf", false)))
	if !strings.Contains(warn, "Warning: CN=leaf,O=Acme Corp is not marked as a CA") {
		t.Fatalf("warn = %q, %v", warn, err)
	}
}

func TestCloudCABadPEMTouchesNoPod(t *testing.T) {
	pod := &lanCfgPod{t: t}
	g := &globalFlags{connection: "usb", usbAddr: pod.start()}
	_, _, err := runCACapture(g, "set", tempPEM(t, "bad.pem", []byte("not a certificate")))
	if err == nil || !strings.Contains(err.Error(), "no CERTIFICATE block") {
		t.Fatalf("err = %v", err)
	}
	if pod.cmds() != "" {
		t.Fatalf("a bad PEM reached the pod: %v", pod.cmds())
	}
	if _, _, err := runCACapture(g, "set", filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("a missing file passed")
	}
}

// Without the USB network adapter (and no network address that answers), the command says how to
// turn it on.
func TestCloudCfgUSBWithoutTheNetwork(t *testing.T) {
	old := usbNetwork
	usbNetwork = func(*globalFlags, string) (serialconsole.USBNet, error) {
		return serialconsole.USBNet{PodIP: "10.237.8.85", HostIP: "10.237.8.86", OffBySetting: true}, nil
	}
	t.Cleanup(func() { usbNetwork = old })
	_, _, err := runProxyCapture(&globalFlags{connection: "usb"}, "show", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "usb-network on") {
		t.Fatalf("err = %v", err)
	}
}

func TestCloudProxyUSB(t *testing.T) {
	pod := &lanCfgPod{t: t}
	addr := pod.start()
	g := &globalFlags{connection: "usb", usbAddr: addr}

	out, _, err := runProxyCapture(g, "show", "", "", "")
	if err != nil || out != "HTTP proxy of "+addr+": none (the pod connects to the cloud directly)\n" {
		t.Fatalf("show = %q, %v", out, err)
	}
	out, warn, err := runProxyCapture(g, "set", "Proxy.Corp:3128", "alice", "s3cret")
	if err != nil || out != "HTTP proxy of "+addr+" is now proxy.corp:3128 (with a user and password)\n" {
		t.Fatalf("set = %q, %v", out, err)
	}
	if warn != "The pod reconnects to the cloud through proxy.corp:3128.\n" || strings.Contains(out+warn, "s3cret") {
		t.Fatalf("warn = %q", warn)
	}
	out, _, err = runProxyCapture(g, "clear", "", "", "")
	if err != nil || !strings.HasSuffix(out, "is now none (the pod connects to the cloud directly)\n") {
		t.Fatalf("clear = %q, %v", out, err)
	}
}

func TestCheckProxyAddr(t *testing.T) {
	for in, want := range map[string]string{
		"proxy.corp:3128": "proxy.corp:3128", " 10.0.0.1:8080 ": "10.0.0.1:8080", "[fe80::1]:3128": "[fe80::1]:3128",
	} {
		if got, err := checkProxyAddr(in); err != nil || got != want {
			t.Errorf("%q = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"proxy.corp", "http://proxy:3128", ":3128", "proxy:0", "proxy:99999", "proxy:http", "a b:1", "u@p:1"} {
		if _, err := checkProxyAddr(in); err == nil {
			t.Errorf("%q passed", in)
		}
	}
	pod := &lanCfgPod{t: t}
	g := &globalFlags{connection: "usb", usbAddr: pod.start()}
	if _, _, err := runProxyCapture(g, "set", "http://p:1", "", ""); err == nil {
		t.Fatal("a bad address passed")
	}
	if _, _, err := runProxyCapture(g, "set", "p:1", "alice", ""); err == nil {
		t.Fatal("a user without a password passed")
	}
	if pod.cmds() != "" {
		t.Fatalf("a bad value reached the pod: %v", pod.cmds())
	}
}

// ── LAN ─────────────────────────────────────────────────────────────────────

// lanCfgPod is a pod's LAN JSON API for cloud_ca, cloud_proxy and the ota_* upload, serving many
// lines per connection.
type lanCfgPod struct {
	t      *testing.T
	mu     sync.Mutex
	locked string // the T2/T3 verbs refused with "locked: ..."
	gated  bool   // current firmware: every change refused from the LAN (pod_policy_cloud_link_gate)
	certs  []podCACert
	proxy  proxyReply
	staged []byte
	sha    string
	state  string
	reqs   []map[string]any
	chunks []int
}

func (p *lanCfgPod) start() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					_ = json.Unmarshal([]byte(line), &req)
					fmt.Fprintln(conn, p.handle(req))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func okReply(data any) string {
	b, _ := json.Marshal(map[string]any{"status": "ok", "data": data})
	return string(b)
}

func codeReply(code string, args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"status": "error", "code": code, "args": args})
	return string(b)
}

func errReply(msg string) string {
	b, _ := json.Marshal(map[string]any{"status": "error", "message": msg})
	return string(b)
}

func (p *lanCfgPod) handle(req map[string]any) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	cmd, _ := req["cmd"].(string)
	changes := req["set"] != nil || req["clear"] != nil || strings.HasPrefix(cmd, "ota_") && cmd != "ota_abort"
	if p.locked != "" && changes {
		return codeReply("BP0040", map[string]any{"cmd": cmd})
	}
	if p.gated && (cmd == "cloud_ca" || cmd == "cloud_proxy") && changes {
		return codeReply("BP0042", map[string]any{"setting": cmd})
	}
	if p.gated && cmd == "ota_begin" && req["target"] == "ca" {
		return codeReply("BP0042", map[string]any{"setting": "cloud_ca"})
	}
	switch cmd {
	case "cloud_ca":
		if req["clear"] == true {
			p.certs = nil
		}
		return okReply(map[string]any{"present": len(p.certs) > 0, "certs": p.certs})
	case "cloud_proxy":
		if req["clear"] == true {
			p.proxy = proxyReply{}
		}
		if s, ok := req["set"].(string); ok {
			h, port, _ := net.SplitHostPort(s)
			n := 0
			fmt.Sscan(port, &n)
			p.proxy = proxyReply{Host: h, Port: n, Auth: req["password"] != nil}
		}
		if p.proxy.Host == "" {
			return okReply(map[string]any{})
		}
		return okReply(p.proxy)
	case "ota_begin":
		if req["target"] != "ca" {
			return codeReply("BP0060", map[string]any{"target": req["target"]})
		}
		p.staged = make([]byte, int(req["size"].(float64)))
		p.sha, p.state = req["sha256"].(string), "receiving"
	case "ota_data":
		off := int(req["offset"].(float64))
		raw, err := base64.RawURLEncoding.DecodeString(req["data"].(string))
		if err != nil {
			return errReply("invalid data")
		}
		p.chunks = append(p.chunks, len(raw))
		copy(p.staged[off:], raw)
	case "ota_end":
		sum := sha256.Sum256(p.staged)
		p.state = "verified"
		if hex.EncodeToString(sum[:]) != p.sha {
			p.state = "error"
		}
	case "ota_commit":
		certs, _, err := checkCAPEM(p.staged)
		if err != nil {
			return errReply("not a CA certificate")
		}
		p.certs = nil
		for _, c := range certs {
			p.certs = append(p.certs, podCACert{Subject: c.Subject, SHA256: c.SHA256})
		}
		p.state = "installed"
	case "ota_abort":
		p.state = "idle"
	default:
		return codeReply("BP0002", map[string]any{"cmd": cmd})
	}
	return okReply(map[string]any{"state": p.state, "target": "ca", "error": ""})
}

func (p *lanCfgPod) cmds() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var s []string
	for _, r := range p.reqs {
		s = append(s, r["cmd"].(string))
	}
	return strings.Join(s, ",")
}

func TestCloudCALAN(t *testing.T) {
	pod := &lanCfgPod{t: t}
	addr := pod.start()
	g := &globalFlags{connection: addr}

	pemData := append(testCertPEM(t, "Acme Root CA", true), testCertPEM(t, "Acme Issuing CA", true)...)
	out, _, err := runCACapture(g, "set", tempPEM(t, "acme.pem", pemData))
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.HasPrefix(out, "Company CA of "+addr+" is now 2 certificates\n  CN=Acme Root CA,O=Acme Corp\n") {
		t.Fatalf("set = %q", out)
	}
	nChunks := (len(pemData) + caLANChunk - 1) / caLANChunk
	// status first: whether the pod's USB link answers here (the change would go over it).
	want := "status,cloud_ca,ota_begin" + strings.Repeat(",ota_data", nChunks) + ",ota_end,ota_commit,cloud_ca"
	if got := pod.cmds(); got != want {
		t.Fatalf("requests = %s\nwant %s", got, want)
	}
	if pod.chunks[0] != caLANChunk {
		t.Fatalf("first chunk %d bytes", pod.chunks[0])
	}

	out, _, err = runCACapture(g, "show", "")
	if err != nil || !strings.HasPrefix(out, "Company CA of "+addr+": 2 certificates\n") {
		t.Fatalf("show = %q, %v", out, err)
	}
	out, _, err = runCACapture(g, "clear", "")
	if err != nil || !strings.HasSuffix(out, "is now none (the pod trusts only its built-in roots)\n") {
		t.Fatalf("clear = %q, %v", out, err)
	}
}

func TestCloudCALANLocked(t *testing.T) {
	pod := &lanCfgPod{t: t, locked: "yes"}
	addr := pod.start()
	_, _, err := runCACapture(&globalFlags{connection: addr}, "set", tempPEM(t, "a.pem", testCertPEM(t, "A", true)))
	want := "cloud ca set: the pod refused: BP0040: The pod's LAN is locked: ota_begin needs the cloud or USB; run this over USB with --connection usb, or from the cloud"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v\nwant %s", err, want)
	}
	if !strings.HasSuffix(pod.cmds(), "ota_begin,ota_abort") {
		t.Fatalf("requests = %s", pod.cmds())
	}
	_, _, err = runProxyCapture(&globalFlags{connection: addr}, "clear", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "BP0040: The pod's LAN is locked: cloud_proxy needs") || !strings.Contains(err.Error(), "--connection usb") {
		t.Fatalf("proxy clear: err = %v", err)
	}
	// Showing still works on a locked LAN.
	if _, _, err := runProxyCapture(&globalFlags{connection: addr}, "show", "", "", ""); err != nil {
		t.Fatalf("show on a locked LAN: %v", err)
	}
}

func TestCloudLANGated(t *testing.T) {
	pod := &lanCfgPod{t: t, gated: true}
	addr := pod.start()
	g := &globalFlags{connection: addr}

	_, _, err := runCACapture(g, "set", tempPEM(t, "a.pem", testCertPEM(t, "A", true)))
	want := "cloud ca set: the pod refused: BP0042: cloud_ca cannot be changed over the LAN: change it from the cloud or over USB; run this over USB with --connection usb, or from the cloud"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("ca set: err = %v\nwant %s", err, want)
	}
	if !strings.HasSuffix(pod.cmds(), "ota_begin,ota_abort") {
		t.Fatalf("requests = %s", pod.cmds())
	}
	_, _, err = runCACapture(g, "clear", "")
	want = "cloud ca clear: the pod refused: BP0042: cloud_ca cannot be changed"
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("ca clear: err = %v\nwant %s", err, want)
	}
	for _, action := range []string{"set", "clear"} {
		addrArg, user, pw := "", "", ""
		if action == "set" {
			addrArg, user, pw = "proxy.corp:3128", "alice", "s3cret"
		}
		_, _, err = runProxyCapture(g, action, addrArg, user, pw)
		want = "cloud proxy " + action + ": the pod refused: BP0042: cloud_proxy cannot be changed over the LAN"
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("proxy %s: err = %v\nwant %s", action, err, want)
		}
	}
	// Reading still works from the LAN.
	if _, _, err := runCACapture(g, "show", ""); err != nil {
		t.Fatalf("ca show: %v", err)
	}
	if _, _, err := runProxyCapture(g, "show", "", "", ""); err != nil {
		t.Fatalf("proxy show: %v", err)
	}
}

func TestCloudProxyLAN(t *testing.T) {
	pod := &lanCfgPod{t: t}
	addr := pod.start()
	g := &globalFlags{connection: addr}

	out, _, err := runProxyCapture(g, "show", "", "", "")
	if err != nil || out != "HTTP proxy of "+addr+": none (the pod connects to the cloud directly)\n" {
		t.Fatalf("show = %q, %v", out, err)
	}
	out, warn, err := runProxyCapture(g, "set", "proxy.corp:3128", "alice", "pa ss")
	if err != nil || out != "HTTP proxy of "+addr+" is now proxy.corp:3128 (with a user and password)\n" ||
		warn != "The pod reconnects to the cloud through proxy.corp:3128.\n" {
		t.Fatalf("set = %q, %q, %v", out, warn, err)
	}
	var set map[string]any // each command reads status first (is the pod's USB link here?)
	for _, r := range pod.reqs {
		if _, ok := r["set"]; ok {
			set = r
			break
		}
	}
	if set["set"] != "proxy.corp:3128" || set["user"] != "alice" || set["password"] != "pa ss" {
		t.Fatalf("set request = %v", set)
	}
	out, _, err = runProxyCapture(g, "set", "10.1.2.3:8080", "", "")
	if err != nil || !strings.HasSuffix(out, "is now 10.1.2.3:8080 (no authentication)\n") {
		t.Fatalf("set noauth = %q, %v", out, err)
	}
	if last := lastSet(pod.reqs); last["password"] != nil {
		t.Fatalf("a password went out without --user: %v", last)
	}
	out, _, err = runProxyCapture(g, "clear", "", "", "")
	if err != nil || !strings.HasSuffix(out, "is now none (the pod connects to the cloud directly)\n") {
		t.Fatalf("clear = %q, %v", out, err)
	}
}

// lastSet is the last cloud_proxy set request.
func lastSet(reqs []map[string]any) map[string]any {
	var last map[string]any
	for _, r := range reqs {
		if _, ok := r["set"]; ok {
			last = r
		}
	}
	return last
}

func TestCloudCfgNeedsATarget(t *testing.T) {
	g := &globalFlags{configFile: filepath.Join(t.TempDir(), "none.yaml")}
	if _, _, err := runProxyCapture(g, "show", "", "", ""); err == nil || !strings.Contains(err.Error(), "--connection usb") {
		t.Fatalf("err = %v", err)
	}
}

func TestProxyPasswordOverTheLANIsWarnedAbout(t *testing.T) {
	var warn bytes.Buffer
	warnPlainLANPassword(&globalFlags{connection: "192.168.1.220"}, &warn)
	if !strings.Contains(warn.String(), "plain TCP") || !strings.Contains(warn.String(), "--connection usb") {
		t.Fatalf("LAN: %q", warn.String())
	}
	for _, conn := range []string{"usb", "/dev/cu.usbmodem1", "embeddedci:bench"} {
		warn.Reset()
		warnPlainLANPassword(&globalFlags{connection: conn}, &warn)
		if warn.Len() != 0 {
			t.Fatalf("%s: %q", conn, warn.String())
		}
	}
}
