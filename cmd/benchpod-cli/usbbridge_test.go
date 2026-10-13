package main

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// startProxy runs a connectProxy on a loopback listener that serves 127.0.0.1 and tunnels to
// allowPort. It returns the proxy's address.
func startProxy(t *testing.T, allowFrom string, allowPort int) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &connectProxy{allowFrom: allowFrom, allowPort: allowPort, logf: t.Logf}
	go p.serve(ln)
	t.Cleanup(func() { ln.Close(); p.wait() })
	return ln.Addr().String()
}

// echoServer answers every line with "echo: <line>".
func echoServer(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					io.WriteString(c, "echo: "+sc.Text()+"\n")
				}
			}()
		}
	}()
	return ln.Addr().String(), ln.Addr().(*net.TCPAddr).Port
}

func connect(t *testing.T, proxy, target string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	// The way the pod's firmware asks (altcp_bp_proxy.c), with the first bytes of the
	// tunnel in the same write.
	io.WriteString(c, "CONNECT "+target+" HTTP/1.1\r\nHost: "+target+"\r\n\r\n")
	br := bufio.NewReader(c)
	status, _ := br.ReadString('\n')
	for {
		l, err := br.ReadString('\n')
		if err != nil || l == "\r\n" {
			break
		}
	}
	return c, br, status
}

func TestConnectProxyTunnels(t *testing.T) {
	target, port := echoServer(t)
	proxy := startProxy(t, "127.0.0.1", port)
	c, br, status := connect(t, proxy, target)
	defer c.Close()
	if !strings.Contains(status, " 200 ") {
		t.Fatalf("status %q, want 200", status)
	}
	io.WriteString(c, "hello\n")
	if got, _ := br.ReadString('\n'); got != "echo: hello\n" {
		t.Fatalf("tunnel answered %q", got)
	}
}

func TestConnectProxyRefusesOtherPorts(t *testing.T) {
	target, port := echoServer(t)
	proxy := startProxy(t, "127.0.0.1", port+1)
	c, _, status := connect(t, proxy, target)
	defer c.Close()
	if !strings.Contains(status, " 403 ") {
		t.Fatalf("status %q, want 403 for a port other than %d", status, port+1)
	}
}

func TestConnectProxyServesOnlyThePod(t *testing.T) {
	target, port := echoServer(t)
	proxy := startProxy(t, "10.237.8.85", port)
	c, _, status := connect(t, proxy, target)
	defer c.Close()
	if status != "" {
		t.Fatalf("a client other than the pod got %q, want the connection closed", status)
	}
}

func TestConnectProxyRefusesPlainRequests(t *testing.T) {
	proxy := startProxy(t, "127.0.0.1", 443)
	c, err := net.Dial("tcp", proxy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	status, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(status, " "+strconv.Itoa(405)+" ") {
		t.Fatalf("status %q, want 405", status)
	}
}

func TestUSBBridgeReady(t *testing.T) {
	if err := usbBridgeReady(serialconsole.USBNet{On: true, Link: true}); err != nil {
		t.Fatal(err)
	}
	if err := usbBridgeReady(serialconsole.USBNet{}); err == nil || !strings.Contains(err.Error(), "clear-wifi") {
		t.Fatalf("net off: %v", err)
	}
	if err := usbBridgeReady(serialconsole.USBNet{On: true}); err == nil || !strings.Contains(err.Error(), "usb-reattach") {
		t.Fatalf("link down: %v", err)
	}
}
