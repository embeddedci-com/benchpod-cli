package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/spf13/cobra"
)

// usb-bridge: the pod's cloud link through this computer. The pod's USB network adapter has no
// router (the computer keeps its own default route), so the pod cannot reach embeddedci.com over
// it by itself. The bridge runs an HTTP CONNECT proxy on the computer's end of the USB link and
// sets it as the pod's cloud proxy (the same setting as `benchpod cloud proxy set`); the pod's
// firmware then starts its cloud client on the USB link alone.

const (
	usbBridgeDefaultPort = 3128
	usbBridgeDialTimeout = 15 * time.Second
	// The pod only ever asks for its cloud server over TLS.
	usbBridgeAllowedPort = 443
)

func newUSBBridgeCmd(g *globalFlags) *cobra.Command {
	var port int
	var keep, watch bool
	cmd := &cobra.Command{
		Use:   "usb-bridge",
		Short: "Carry the pod's cloud link over USB through this computer (runs until Ctrl-C)",
		Long: "Carry the pod's cloud link over its USB network adapter: run an HTTP CONNECT proxy on\n" +
			"this computer's end of the USB link and set it as the pod's cloud proxy, so the pod\n" +
			"reaches embeddedci.com through this computer's internet connection. It runs until\n" +
			"Ctrl-C, then clears the pod's proxy again (--keep-proxy leaves it set).\n\n" +
			"--watch keeps going when no pod is plugged in or the USB link goes away, and picks the\n" +
			"pod up again when it comes back: that is how `usb-bridge install` runs it in the\n" +
			"background. It never polls or holds the pod's USB console: it watches for this\n" +
			"computer's USB network address, reads the pod's proxy over that link, and opens the\n" +
			"console once only to set the proxy (or clear it when it stops).\n\n" +
			"Needs firmware 3.9.0 or later and no Wi-Fi configured on the pod. The proxy accepts\n" +
			"only the pod, and only tunnels to port 443. It refuses to replace a proxy the pod\n" +
			"already has (`benchpod cloud proxy clear` removes it).",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runUSBBridge(g, port, keep, watch)
		},
	}
	cmd.Flags().IntVar(&port, "port", usbBridgeDefaultPort, "TCP port of the proxy on this computer's USB address")
	cmd.Flags().BoolVar(&keep, "keep-proxy", false, "leave the pod's cloud proxy set when the bridge stops")
	cmd.Flags().BoolVar(&watch, "watch", false, "wait for the pod and survive unplugging it (for running in the background)")
	cmd.AddCommand(newUSBBridgeInstallCmd(g), newUSBBridgeUninstallCmd(g), newUSBBridgeStatusCmd(g))
	return cmd
}

// bridgeSession is one bridge run: the pod's USB link and the proxy's listener on this
// computer's end of it.
type bridgeSession struct {
	u         serialconsole.USBNet
	proxyAddr string
	ln        net.Listener
}

// hostHasIP reports whether a local interface holds ip: the USB link is gone when it does not.
var hostHasIP = func(ip string) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // cannot tell: keep serving
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.String() == ip {
			return true
		}
	}
	return false
}

// serveBridge runs the proxy until ctx ends (true) or the USB link goes away (false).
func serveBridge(ctx context.Context, s *bridgeSession) bool {
	gone := make(chan struct{})
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.ln.Close()
				return
			case <-done:
				return
			case <-t.C:
				if !hostHasIP(s.u.HostIP) {
					close(gone)
					s.ln.Close()
					return
				}
			}
		}
	}()
	p := &connectProxy{allowFrom: s.u.PodIP, allowPort: usbBridgeAllowedPort, logf: log.Printf}
	p.serve(s.ln)
	close(done)
	p.wait()
	select {
	case <-gone:
		return false
	default:
		return ctx.Err() != nil
	}
}

func runUSBBridge(g *globalFlags, port int, keep, watch bool) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("usb-bridge: --port %d is out of range", port)
	}
	// Never the console (usbbridge_watch.go): the pod's USB network link only.
	return runUSBBridgeWatch(g, port, keep, watch)
}

// connectProxy is a minimal HTTP CONNECT proxy for one client address and one destination port.
type connectProxy struct {
	allowFrom string // the only client IP served
	allowPort int    // the only destination port tunnelled
	logf      func(format string, args ...any)
	dial      func(network, addr string, timeout time.Duration) (net.Conn, error) // nil: net.DialTimeout
	wg        sync.WaitGroup
}

// serve accepts until ln is closed.
func (p *connectProxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
		}()
	}
}

// wait blocks until the open tunnels have ended, or 2 s.
func (p *connectProxy) wait() {
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

func (p *connectProxy) handle(c net.Conn) {
	defer c.Close()
	host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	if host != p.allowFrom {
		p.logf("usb-bridge: refused a connection from %s (only the pod, %s)", host, p.allowFrom)
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		reply(c, http.StatusMethodNotAllowed)
		return
	}
	target := req.RequestURI
	_, tport, err := net.SplitHostPort(target)
	if err != nil || tport != strconv.Itoa(p.allowPort) {
		p.logf("usb-bridge: refused CONNECT %s (only port %d)", target, p.allowPort)
		reply(c, http.StatusForbidden)
		return
	}
	dial := p.dial
	if dial == nil {
		dial = net.DialTimeout
	}
	up, err := dial("tcp", target, usbBridgeDialTimeout)
	if err != nil {
		p.logf("usb-bridge: CONNECT %s: %v", target, err)
		reply(c, http.StatusBadGateway)
		return
	}
	defer up.Close()
	_ = c.SetReadDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	p.logf("usb-bridge: tunnel to %s open", target)
	start := time.Now()
	var toUp, toPod int64
	done := make(chan struct{})
	go func() {
		// What the pod sent after the request line (normally nothing yet) goes first.
		toUp, _ = io.Copy(up, br)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		close(done)
	}()
	toPod, _ = io.Copy(c, up)
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	<-done
	p.logf("usb-bridge: tunnel to %s closed after %s (%d B up, %d B down)",
		target, time.Since(start).Round(time.Second), toUp, toPod)
}

func reply(c net.Conn, status int) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", status, http.StatusText(status))
}

func newUSBReattachCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "usb-reattach",
		Short: "Make the pod reconnect on USB, so this computer sets up its USB network adapter again",
		Long: "Make the pod drop off the USB bus for half a second and connect again (USB console).\n" +
			"This computer then enumerates the console and the USB network adapter anew, as after\n" +
			"replugging the cable. macOS ignores a new network adapter while the screen is locked,\n" +
			"so run this after unlocking when the pod was plugged in or rebooted meanwhile.\n" +
			"Needs firmware 3.9.0 or later.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			device, err := g.usbDevice("usb-reattach")
			if err != nil {
				return err
			}
			console, _, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(10*time.Second))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()
			if err := console.USBReattach(ctx); err != nil {
				return fmt.Errorf("usb-reattach: %w", err)
			}
			fmt.Fprintln(os.Stderr, "The pod reconnected on USB; its network adapter is back in a few seconds.")
			return nil
		},
	}
}
