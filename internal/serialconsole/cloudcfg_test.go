package serialconsole

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	shaA = "8f2a6e0a3c5b0f1e9d7c6b5a4938271605f4e3d2c1b0a9988776655443322110"
	shaB = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// cfgPod answers ca, ca-clear, proxy, proxy-set and proxy-clear like the firmware's console:
// the line is echoed, a log line may interleave, then the reply and the prompt. old makes it
// answer like firmware without the commands.
type cfgPod struct {
	certs    []CACert
	proxy    string
	auth     bool
	old      bool
	failNext string // the next ca-clear or proxy-set answers "<cmd> error <failNext>"
	lines    []string
}

func (p *cfgPod) console() *Console {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		p.lines = append(p.lines, line)
		out.WriteString(line + "\r\n")
		out.WriteString("[cloud] reconnect in 5 s\r\n")
		f := strings.Fields(line)
		switch {
		case p.old:
			fmt.Fprintf(out, "  unknown command '%s' (try 'help')\r\n", f[0])
		case f[0] == "ca":
			if len(p.certs) == 0 {
				out.WriteString("ca none\r\n")
			}
			for _, c := range p.certs {
				fmt.Fprintf(out, "ca %s %s\r\n", c.Subject, c.SHA256)
			}
		case f[0] == "ca-clear" && p.failNext != "":
			fmt.Fprintf(out, "ca-clear error %s\r\n", p.failNext)
			p.failNext = ""
		case f[0] == "ca-clear":
			p.certs = nil
			out.WriteString("ca-clear ok\r\n")
		case f[0] == "proxy-set" && p.failNext != "":
			fmt.Fprintf(out, "proxy error %s\r\n", p.failNext)
			p.failNext = ""
		case f[0] == "proxy-set":
			p.proxy, p.auth = f[1], len(f) == 4
			fallthrough
		case f[0] == "proxy":
			if p.proxy == "" {
				out.WriteString("proxy none\r\n")
			} else if p.auth {
				fmt.Fprintf(out, "proxy %s auth\r\n", p.proxy)
			} else {
				fmt.Fprintf(out, "proxy %s noauth\r\n", p.proxy)
			}
		case f[0] == "proxy-clear":
			p.proxy, p.auth = "", false
			out.WriteString("proxy none\r\n")
		}
		out.WriteString("> ")
	}}
	return newConsole(fc)
}

func TestCloudCAShowAndClear(t *testing.T) {
	pod := &cfgPod{certs: []CACert{{"CN=Acme Root CA, O=Acme Corp", shaA}, {"CN=Acme Issuing CA 2", shaB}}}
	c := pod.console()

	certs, err := c.CloudCA(testContext(t))
	if err != nil {
		t.Fatalf("ca: %v", err)
	}
	if len(certs) != 2 || certs[0] != pod.certs[0] || certs[1] != pod.certs[1] {
		t.Fatalf("ca = %+v", certs)
	}

	if err := c.CloudCAClear(testContext(t)); err != nil {
		t.Fatalf("ca-clear: %v", err)
	}
	certs, err = c.CloudCA(testContext(t))
	if err != nil || len(certs) != 0 {
		t.Fatalf("ca after clear = %+v, %v", certs, err)
	}

	pod.failNext = "w25q write failed"
	err = c.CloudCAClear(testContext(t))
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Reason != "w25q write failed" {
		t.Fatalf("ca-clear refusal = %v", err)
	}
}

func TestCloudProxySetShowClear(t *testing.T) {
	pod := &cfgPod{}
	c := pod.console()
	var logs bytes.Buffer
	c.logf = func(format string, a ...any) { fmt.Fprintf(&logs, format+"\n", a...) }

	p, err := c.CloudProxy(testContext(t))
	if err != nil || p.Addr != "" {
		t.Fatalf("proxy = %+v, %v", p, err)
	}
	p, err = c.CloudProxySet(testContext(t), "proxy.corp:3128", "alice", "s3cret")
	if err != nil || p.Addr != "proxy.corp:3128" || !p.Auth {
		t.Fatalf("proxy-set = %+v, %v", p, err)
	}
	if got := pod.lines[len(pod.lines)-1]; got != "proxy-set proxy.corp:3128 alice s3cret" {
		t.Fatalf("sent %q", got)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Fatalf("the password reached the log:\n%s", logs.String())
	}
	p, err = c.CloudProxySet(testContext(t), "10.0.0.1:8080", "", "")
	if err != nil || p.Addr != "10.0.0.1:8080" || p.Auth {
		t.Fatalf("proxy-set noauth = %+v, %v", p, err)
	}
	p, err = c.CloudProxyClear(testContext(t))
	if err != nil || p.Addr != "" {
		t.Fatalf("proxy-clear = %+v, %v", p, err)
	}

	pod.failNext = "cannot resolve host"
	_, err = c.CloudProxySet(testContext(t), "nowhere:1", "", "")
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Cmd != "proxy-set" || ce.Reason != "cannot resolve host" {
		t.Fatalf("proxy-set refusal = %v", err)
	}
}

func TestCloudProxySetLocalChecks(t *testing.T) {
	pod := &cfgPod{}
	c := pod.console()
	for _, tc := range []struct{ addr, user, pass, want string }{
		{"a b:1", "", "", "bad address"},
		{"p:1", "u", "", "both a user and a password"},
		{"p:1", "u", "pa ss", "spaces"},
		{"p:1", "u", strings.Repeat("x", 120), "the USB console takes 127"},
	} {
		_, err := c.CloudProxySet(context.Background(), tc.addr, tc.user, tc.pass)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v, want %q", tc, err, tc.want)
		}
		if err != nil && tc.pass != "" && strings.Contains(err.Error(), tc.pass) {
			t.Errorf("the password is in the error: %v", err)
		}
	}
	if len(pod.lines) != 0 {
		t.Fatalf("a refused value reached the pod: %v", pod.lines)
	}
}

func TestCloudConfigOldFirmware(t *testing.T) {
	c := (&cfgPod{old: true}).console()
	if _, err := c.CloudCA(testContext(t)); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("ca: %v", err)
	}
	if err := c.CloudCAClear(testContext(t)); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("ca-clear: %v", err)
	}
	if _, err := c.CloudProxy(testContext(t)); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("proxy: %v", err)
	}
	if _, err := c.CloudProxySet(testContext(t), "p:1", "u", "pw"); !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("proxy-set: %v", err)
	}
}

func TestParseCA(t *testing.T) {
	raw := "ca\r\n[log] x\r\nca CN=Root  With  Spaces " + strings.ToUpper(shaA) + "\r\nca garbage\r\n> "
	certs, err := parseCA(raw)
	if err != nil || len(certs) != 1 || certs[0].Subject != "CN=Root  With  Spaces" || certs[0].SHA256 != shaA {
		t.Fatalf("parseCA = %+v, %v", certs, err)
	}
	if _, err := parseCA("ca\r\n> "); !errors.Is(err, errNoCAReply) {
		t.Fatalf("echo only: %v", err)
	}
	var ce *CommandError
	if _, err := parseCA("ca\r\nca error w25q read failed\r\n> "); !errors.As(err, &ce) || ce.Reason != "w25q read failed" {
		t.Fatalf("error reply: %v", err)
	}
}

func TestUploadCATarget(t *testing.T) {
	sim := newPodUploadSim()
	c := newConsole(sim)
	pem := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	if err := c.Upload(context.Background(), "ca", pem, 0, nil); err != nil {
		t.Fatalf("upload ca: %v", err)
	}
	if !bytes.Equal(sim.committed["ca"], pem) {
		t.Fatalf("committed %q", sim.committed["ca"])
	}
	for _, cmd := range sim.commands {
		if strings.HasPrefix(cmd, "upload-sig") {
			t.Fatalf("a CA upload sent %q", cmd)
		}
	}
}
