package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/authstore"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// policyServer fakes the embeddedci.com device list and policy endpoints for one device
// (id-b, "benchpod-baea06"). put, when set, answers a PUT instead of the default success.
type policyServer struct {
	t         *testing.T
	policy    map[string]string // per kind
	supported bool
	put       func(w http.ResponseWriter, kind, policy string) bool
	puts      []string
}

func (s *policyServer) start() *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer acc" {
			s.t.Errorf("Authorization = %q", got)
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/benchpod/devices" {
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []serverapi.DeviceResponse{
				{ID: "id-a", Name: "bench-a"}, {ID: "id-b", Name: "benchpod-baea06"},
			}})
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/api/benchpod/devices/id-b/")
		if !ok || (rest != "lan-policy" && rest != "sig-policy") {
			s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"policy": s.policy[rest], "supported": s.supported})
		case http.MethodPut:
			var body struct {
				Policy string `json:"policy"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.puts = append(s.puts, rest+"="+body.Policy)
			if s.put != nil && s.put(w, rest, body.Policy) {
				return
			}
			s.policy[rest] = body.Policy
			_ = json.NewEncoder(w).Encode(map[string]any{"policy": body.Policy, "supported": true})
		}
	}))
	s.t.Cleanup(ts.Close)
	return ts
}

func cloudTarget(t *testing.T, url string) policyTarget {
	future := time.Now().Add(time.Hour)
	return policyTarget{serverURL: url, deviceName: "benchpod-baea06", tokenFile: writeTokens(t, &authstore.Tokens{
		AccessToken: "acc", RefreshToken: "ref", AccessExpiresAt: future, RefreshExpiresAt: future,
		SessionID: "s1", UserID: "u1",
	})}
}

func runPolicyCapture(t *testing.T, g *globalFlags, p podPolicy, target policyTarget, set string) (string, string, error) {
	t.Helper()
	var out, warn bytes.Buffer
	err := runPolicy(g, p, target, set, &out, &warn)
	return out.String(), warn.String(), err
}

func TestPolicyCloudShowAndSet(t *testing.T) {
	srv := &policyServer{t: t, supported: true, policy: map[string]string{"lan-policy": "open", "sig-policy": "required"}}
	target := cloudTarget(t, srv.start().URL)

	out, _, err := runPolicyCapture(t, &globalFlags{}, lanPolicy, target, "")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if want := "LAN policy of benchpod-baea06: open (full access on the LAN)\n"; out != want {
		t.Fatalf("show = %q, want %q", out, want)
	}

	out, warn, err := runPolicyCapture(t, &globalFlags{}, lanPolicy, target, "locked")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if want := "LAN policy of benchpod-baea06 is now locked (read and instrument control only on the LAN)\n"; out != want {
		t.Fatalf("set = %q, want %q", out, want)
	}
	if warn != "" {
		t.Fatalf("unexpected warning %q", warn)
	}

	_, warn, err = runPolicyCapture(t, &globalFlags{}, lanPolicy, target, "off")
	if err != nil || !strings.Contains(warn, "SCPI/VISA") {
		t.Fatalf("set off: err %v, warning %q", err, warn)
	}

	out, _, err = runPolicyCapture(t, &globalFlags{}, sigPolicy, target, "")
	if err != nil || out != "Signature policy of benchpod-baea06: required (only signed images are installed)\n" {
		t.Fatalf("sig show = %q, %v", out, err)
	}
	// The signature policy has no set: it is always required.
	if _, _, err = runPolicyCapture(t, &globalFlags{}, sigPolicy, target, "permissive"); exitCode(err) != exitUsage {
		t.Fatalf("sig set: %v", err)
	}
	if got := strings.Join(srv.puts, ","); got != "lan-policy=locked,lan-policy=off" {
		t.Fatalf("puts = %s", got)
	}
}

// --connection embeddedci:<name> goes through embeddedci.com, like --device-name <name>.
func TestPolicyCloudByConnection(t *testing.T) {
	srv := &policyServer{t: t, supported: true, policy: map[string]string{"lan-policy": "open", "sig-policy": "audit"}}
	target := cloudTarget(t, srv.start().URL)
	target.deviceName = ""

	g := &globalFlags{connection: "embeddedci:benchpod-baea06"}
	out, _, err := runPolicyCapture(t, g, lanPolicy, target, "locked")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !strings.HasPrefix(out, "LAN policy of benchpod-baea06 is now locked") {
		t.Fatalf("set = %q", out)
	}
	if got := strings.Join(srv.puts, ","); got != "lan-policy=locked" {
		t.Fatalf("puts = %s", got)
	}
}

func TestPolicyCloudForbidden(t *testing.T) {
	srv := &policyServer{t: t, supported: true, policy: map[string]string{"lan-policy": "open"},
		put: func(w http.ResponseWriter, _, _ string) bool {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"this needs an organization owner or admin (API keys: the benchpod:admin scope)"}`))
			return true
		}}
	_, _, err := runPolicyCapture(t, &globalFlags{}, lanPolicy, cloudTarget(t, srv.start().URL), "off")
	want := "lan-policy set: this needs an organization owner or admin (API keys: the benchpod:admin scope)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// sig-policy has no set subcommand any more; a relayed BP0074 is worded from the catalog.
func TestSigPolicyCannotBeLoosened(t *testing.T) {
	g := &globalFlags{}
	cmd := newSigPolicyCmd(g)
	for _, c := range cmd.Commands() {
		if c.Name() == "set" {
			t.Fatal("sig-policy still has set")
		}
	}
	if code := run([]string{"sig-policy", "set", "audit"}); code != exitUsage {
		t.Fatalf("sig-policy set: exit %d", code)
	}
	err := cloudPolicyError(lanPolicy, "off", &serverapi.APIError{Status: http.StatusConflict, Code: "BP0074", Message: "BP0074"})
	if err == nil || !strings.Contains(err.Error(), "BP0074: ") || !strings.Contains(err.Error(), "this firmware installs only signed images") {
		t.Fatalf("err = %v", err)
	}
}

func TestPolicyCloudUnsupported(t *testing.T) {
	srv := &policyServer{t: t, supported: false, policy: map[string]string{}}
	_, _, err := runPolicyCapture(t, &globalFlags{}, lanPolicy, cloudTarget(t, srv.start().URL), "locked")
	if err == nil || err.Error() != "this firmware has no LAN policy; update the pod's firmware" {
		t.Fatalf("err = %v", err)
	}
	if len(srv.puts) != 0 {
		t.Fatalf("a pod without the policy got a PUT: %v", srv.puts)
	}
}

// fakePolicyConsole stands in for the pod's USB console.
type fakePolicyConsole struct {
	rep   serialconsole.PolicyReply
	err   error
	calls []string
}

func (f *fakePolicyConsole) Policy(_ context.Context, cmd, set string) (serialconsole.PolicyReply, error) {
	f.calls = append(f.calls, strings.TrimSpace(cmd+" "+set))
	if f.err != nil {
		return serialconsole.PolicyReply{}, f.err
	}
	if set != "" {
		f.rep.Policy = set
	}
	return f.rep, nil
}

func (f *fakePolicyConsole) Close() error { return nil }

func withFakeConsole(t *testing.T, fc *fakePolicyConsole) {
	t.Helper()
	saved := openPolicyConsole
	openPolicyConsole = func(_ *globalFlags, device string, _ time.Duration) (policyConsole, string, context.Context, context.CancelFunc, error) {
		if device == "" {
			device = "/dev/cu.usbmodem1101"
		}
		return fc, device, context.Background(), func() {}, nil
	}
	t.Cleanup(func() { openPolicyConsole = saved })
}

func TestPolicyUSBShowAndSet(t *testing.T) {
	fc := &fakePolicyConsole{rep: serialconsole.PolicyReply{Policy: "open", Keys: -1}}
	withFakeConsole(t, fc)
	// The signature policy goes over the pod's USB network adapter, not the console.
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"policy":"required","keys":3}}`)
	out, _, err := runPolicyCapture(t, &globalFlags{connection: "usb", usbAddr: addr}, sigPolicy, policyTarget{}, "")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if want := "Signature policy of " + addr + ": required (only signed images are installed), 3 trusted keys\n"; out != want {
		t.Fatalf("show = %q, want %q", out, want)
	}
	if req := <-got; req != `{"cmd":"sig_policy"}` {
		t.Fatalf("request %s", req)
	}

	out, warn, err := runPolicyCapture(t, &globalFlags{connection: "/dev/cu.usbmodem9"}, lanPolicy, policyTarget{}, "off")
	if err != nil {
		t.Fatalf("lan set: %v", err)
	}
	if want := "LAN policy of the pod on /dev/cu.usbmodem9 is now off (no LAN access; cloud and USB only)\n"; out != want {
		t.Fatalf("lan set = %q, want %q", out, want)
	}
	for _, w := range []string{"SCPI/VISA", "hwe2e TestHW", "benchpod discover", "benchpod lan-policy set open"} {
		if !strings.Contains(warn, w) {
			t.Fatalf("warning %q does not mention %q", warn, w)
		}
	}
	if got := strings.Join(fc.calls, ","); got != "lan-policy off" {
		t.Fatalf("console calls = %s", got)
	}
}

func TestPolicyUSBOldFirmwareAndRefusal(t *testing.T) {
	fc := &fakePolicyConsole{err: serialconsole.ErrPolicyUnsupported}
	withFakeConsole(t, fc)
	g := &globalFlags{connection: "usb"}

	_, _, err := runPolicyCapture(t, g, lanPolicy, policyTarget{}, "")
	if err == nil || err.Error() != "this firmware has no LAN policy; update the pod's firmware" {
		t.Fatalf("old firmware: err = %v", err)
	}

	fc.err = &serialconsole.PolicyError{Cmd: "lan-policy", Reason: "config flash write failed"}
	_, _, err = runPolicyCapture(t, g, lanPolicy, policyTarget{}, "locked")
	if err == nil || err.Error() != "lan-policy set: the pod refused: config flash write failed" {
		t.Fatalf("refusal: err = %v", err)
	}
}

// fakeLANPod answers one JSON line per connection with reply.
func fakeLANPod(t *testing.T, reply string) (addr string, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			got <- strings.TrimSpace(line)
			_, _ = io.WriteString(conn, reply+"\n")
			conn.Close()
		}
	}()
	return ln.Addr().String(), got
}

func TestPolicyLANShowOnly(t *testing.T) {
	addr, got := fakeLANPod(t, `{"status":"ok","data":{"policy":"required","enforces":true,"keys":1}}`)
	g := &globalFlags{connection: addr}

	out, _, err := runPolicyCapture(t, g, sigPolicy, policyTarget{}, "")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if want := "Signature policy of " + addr + ": required (only signed images are installed), 1 trusted key\n"; out != want {
		t.Fatalf("show = %q, want %q", out, want)
	}
	if req := <-got; req != `{"cmd":"sig_policy"}` {
		t.Fatalf("request = %s", req)
	}

	_, _, err = runPolicyCapture(t, g, lanPolicy, policyTarget{}, "open")
	if err == nil || !strings.Contains(err.Error(), "refuses policy changes from the LAN") {
		t.Fatalf("LAN set: err = %v", err)
	}
	select {
	case req := <-got:
		t.Fatalf("a LAN set reached the pod: %s", req)
	default:
	}
}

func TestPolicyLANOldFirmware(t *testing.T) {
	for _, reply := range unknownCmdReplies {
		addr, _ := fakeLANPod(t, reply)
		_, _, err := runPolicyCapture(t, &globalFlags{connection: addr}, lanPolicy, policyTarget{}, "")
		if err == nil || err.Error() != "this firmware has no LAN policy; update the pod's firmware" {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestPolicyNeedsATarget(t *testing.T) {
	_, _, err := runPolicyCapture(t, &globalFlags{configFile: t.TempDir() + "/none.yaml"}, lanPolicy, policyTarget{}, "")
	if err == nil || !strings.Contains(err.Error(), "--device-name") {
		t.Fatalf("err = %v", err)
	}
}
