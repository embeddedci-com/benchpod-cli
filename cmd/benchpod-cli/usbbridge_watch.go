package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// usb-bridge --watch, the background service. It never polls the pod's USB console: the console
// is shared with the web app's setup wizard (WebSerial), other benchpod commands and terminals
// (exclusive on Windows), and probing serial ports in a loop would also type into unrelated
// devices (ST-Link virtual COM ports share VID 0483). Instead it watches for this computer's end
// of a pod's USB network link (a 10.237.x.y/30 address on a local interface; the pod is the
// other host), reads the pod's cloud proxy over the JSON API there, and opens the console once,
// only when the proxy has to be set (the LAN tier may read it but not change it) or cleared.
// With Wi-Fi configured the pod's USB networking is off: no address appears, nothing is opened.

// usbWatchPoll is how often --watch looks at the local interfaces (no I/O to the pod).
const usbWatchPoll = 5 * time.Second

// usbLink is one pod's USB network link as this computer sees it.
type usbLink struct{ host, pod string }

// localUSBLinks lists this computer's ends of pods' USB network links: interface addresses in
// 10.237.0.0/16 with a /30 mask. The pod is the first host of the /30, this computer the second.
// A variable so tests can replace it.
var localUSBLinks = func() []usbLink {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []usbLink
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || !usbLinkNet.Contains(ipn.IP) {
			continue
		}
		if ones, bits := ipn.Mask.Size(); ones != 30 || bits != 32 {
			continue
		}
		n := binary.BigEndian.Uint32(ipn.IP.To4()) &^ 3
		pod := make(net.IP, 4)
		binary.BigEndian.PutUint32(pod, n+1)
		out = append(out, usbLink{host: ipn.IP.To4().String(), pod: pod.String()})
	}
	return out
}

// bridgePodAPI is what --watch asks the pod over its USB network. A variable so tests can fake it.
var bridgePodAPI = func(ctx context.Context, addr string, cmd map[string]any) (json.RawMessage, error) {
	return (&tcpclient.Client{Addr: addr}).Command(ctx, cmd)
}

// bridgeConsoleOpens counts console opens by --watch (tests assert it stays 0 while idle).
var bridgeConsoleOpens int

// bridgeSetProxy sets (addr != "") or clears the pod's cloud proxy over its console, once.
// A variable so tests can replace it.
var bridgeSetProxy = func(g *globalFlags, addr string) error {
	bridgeConsoleOpens++
	device, err := g.usbDevice("usb-bridge")
	if err != nil {
		return err
	}
	console, _, ctx, cancel, err := g.openSerialConsole(device, 15*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	defer console.Close()
	if addr == "" {
		_, err = console.CloudProxyClear(ctx)
	} else {
		_, err = console.CloudProxySet(ctx, addr, "", "")
	}
	return err
}

// shortIDOf is a pod's name from its identity public key (base64url): its first 3 bytes in hex.
func shortIDOf(pub string) string {
	b, err := base64.RawURLEncoding.DecodeString(pub)
	if err != nil || len(b) < 3 {
		return ""
	}
	return hex.EncodeToString(b[:3])
}

// pickUSBLink chooses the link to bridge: the one pod there is, or the one usb:<name> names.
func pickUSBLink(ctx context.Context, links []usbLink, name string) (usbLink, error) {
	if name == "" {
		switch len(links) {
		case 0:
			return usbLink{}, errors.New("no pod on a USB network link")
		case 1:
			return links[0], nil
		}
		return usbLink{}, fmt.Errorf("%d pods on USB network links: install the bridge per pod with --connection usb:<name>", len(links))
	}
	for _, l := range links {
		raw, err := bridgePodAPI(ctx, net.JoinHostPort(l.pod, "8080"), map[string]any{"cmd": "identity_public"})
		if err != nil {
			continue
		}
		var rep struct {
			PublicKey string `json:"public"`
		}
		if json.Unmarshal(raw, &rep) == nil && shortIDOf(rep.PublicKey) == name {
			return l, nil
		}
	}
	return usbLink{}, fmt.Errorf("pod %s is not on a USB network link", name)
}

// runUSBBridgeWatch is `usb-bridge --watch`.
func runUSBBridgeWatch(g *globalFlags, port int, keep bool) error {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	defer installSignalHandler(ctx, stop)()
	return watchBridge(ctx, g, port, keep, usbWatchPoll)
}

func watchBridge(ctx context.Context, g *globalFlags, port int, keep bool, poll time.Duration) error {
	name := g.usbPodName()
	var setByUs string // the proxy this run set on the pod, cleared again on stop
	lastWhy := ""
	note := func(why string) {
		if why != lastWhy {
			log.Printf("usb-bridge: waiting: %s", why)
			lastWhy = why
		}
	}
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(poll):
			return true
		}
	}
	for ctx.Err() == nil {
		links := localUSBLinks()
		if len(links) == 0 {
			note("no pod on a USB network link (none plugged in, Wi-Fi configured on it, or the adapter not up yet)")
			if !wait() {
				break
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		link, err := pickUSBLink(cctx, links, name)
		cancel()
		if err != nil {
			note(err.Error())
			if !wait() {
				break
			}
			continue
		}
		proxyAddr := net.JoinHostPort(link.host, strconv.Itoa(port))
		cctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		raw, err := bridgePodAPI(cctx, net.JoinHostPort(link.pod, "8080"), map[string]any{"cmd": "cloud_proxy"})
		cancel()
		if err != nil {
			note(fmt.Sprintf("the pod at %s does not answer yet (%v)", link.pod, err))
			if !wait() {
				break
			}
			continue
		}
		var cur struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		_ = json.Unmarshal(raw, &cur)
		curAddr := ""
		if cur.Host != "" {
			curAddr = net.JoinHostPort(cur.Host, strconv.Itoa(cur.Port))
		}
		if curAddr != "" && curAddr != proxyAddr {
			note(fmt.Sprintf("the pod already has a cloud proxy (%s); remove it with `benchpod cloud proxy clear --connection usb`", curAddr))
			if !wait() {
				break
			}
			continue
		}
		ln, err := net.Listen("tcp", proxyAddr)
		if err != nil {
			note(fmt.Sprintf("listen on %s: %v", proxyAddr, err))
			if !wait() {
				break
			}
			continue
		}
		if curAddr != proxyAddr {
			// The one console use: the LAN tier may read the proxy but not set it.
			if err := bridgeSetProxy(g, proxyAddr); err != nil {
				ln.Close()
				note(fmt.Sprintf("set the pod's cloud proxy over its console: %v", err))
				if !wait() {
					break
				}
				continue
			}
			setByUs = proxyAddr
		}
		lastWhy = ""
		fmt.Fprintf(os.Stderr, "usb-bridge: the pod (%s) reaches embeddedci.com through %s\n", link.pod, proxyAddr)
		s := &bridgeSession{u: serialconsole.USBNet{On: true, Link: true, PodIP: link.pod, HostIP: link.host}, proxyAddr: proxyAddr, ln: ln}
		if serveBridge(ctx, s) {
			break
		}
		log.Printf("usb-bridge: the USB link to the pod (%s) went away", link.pod)
	}
	if setByUs == "" || keep {
		return nil
	}
	if err := bridgeSetProxy(g, ""); err != nil {
		return fmt.Errorf("usb-bridge: stopped, but could not clear the pod's cloud proxy (%v); "+
			"run `benchpod cloud proxy clear --connection usb`", err)
	}
	fmt.Fprintln(os.Stderr, "usb-bridge: stopped; cleared the pod's cloud proxy")
	return nil
}
