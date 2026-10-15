package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/libp2p/zeroconf/v2"
)

// A pod's name, as printed on its label and by `discover`: benchpod-a1b2c3, or just a1b2c3 (the
// identity short id), with an optional .local. Only this exact shape is taken as a pod name, so
// real hostnames still go to the OS resolver.
var podNamePattern = regexp.MustCompile(`^(?i:benchpod-)?[0-9a-fA-F]{6}(?i:\.local)?$`)

// podNameQueries are the mDNS browses for a pod by name, one after the other until it answers:
// 3 seconds in all, the default of `discover --wait`. Two, because the pod does not repeat a
// multicast answer within a second of the last one (RFC 6762 rate limit, in lwIP's responder)
// and the browse itself only asks again after 4 seconds: a second command run right after the
// first would otherwise hear nothing. A browse ends as soon as the pod answers.
var podNameQueries = []time.Duration{1200 * time.Millisecond, 1800 * time.Millisecond}

// namedPod is where a pod given by name was heard on the LAN.
type namedPod struct {
	addr    string // host:port to reach it from here (its USB link first, like discover)
	lanAddr string // its Ethernet or Wi-Fi host:port, "" when only heard on its USB link
}

// podShortName returns the pod's short id ("a1b2c3") when raw is a pod name, else "".
func podShortName(raw string) string {
	if !podNamePattern.MatchString(raw) {
		return ""
	}
	return serialconsole.NormalizePodName(raw)
}

// lookupPodName browses the LAN over mDNS (the browse `discover` does) for the pod with the short
// id short. nil, nil when no such pod answered. A variable so tests can replace it.
var lookupPodName = browsePodName

// podNameNote is where a name lookup says which way it took (stderr; tests capture it).
var podNameNote io.Writer = os.Stderr

// podNameCache keeps one lookup per name per run: a command parses its connection several times.
var podNameCache = struct {
	sync.Mutex
	m map[string]*namedPod
}{m: map[string]*namedPod{}}

// findPodByName is lookupPodName, once per name per run, and says on stderr when the pod was not
// heard on the LAN.
func findPodByName(short string) *namedPod {
	podNameCache.Lock()
	defer podNameCache.Unlock()
	if p, ok := podNameCache.m[short]; ok {
		return p
	}
	p, err := lookupPodName(short)
	if err != nil {
		p = nil
	}
	if p == nil {
		fmt.Fprintf(podNameNote, "benchpod-%s did not answer on the LAN over mDNS; looking for it on USB (usb:benchpod-%s).\n", short, short)
	}
	podNameCache.m[short] = p
	return p
}

// resolvePodName turns a pod name into a connection: the pod's network address when it answers on
// the LAN (or on its USB network link), else usb:<name>, which finds it on USB.
func resolvePodName(short string) ConnSpec {
	if p := findPodByName(short); p != nil {
		return ConnSpec{Kind: connNetwork, Addr: p.addr}
	}
	return ConnSpec{Kind: connSerial, Pod: short}
}

// savedPodName is the connection set-connection stores for a pod given by name: its Ethernet or
// Wi-Fi address when it has one (like `discover --save`), else usb:benchpod-<id>, which finds that
// pod on USB on any port.
func savedPodName(short string) string {
	if p := findPodByName(short); p != nil && p.lanAddr != "" {
		return p.lanAddr
	}
	return "usb:benchpod-" + short
}

// entryNamesPod reports whether an mDNS entry (its DNS-SD instance, "BenchPod a1b2c3", and its
// hostname, "benchpod-a1b2c3.local.") is the pod with the short id short.
func entryNamesPod(instance, hostname, short string) bool {
	return serialconsole.NormalizePodName(unescapeDNSSD(instance)) == short ||
		serialconsole.NormalizePodName(strings.TrimSuffix(hostname, ".")) == short
}

// namedPodAt is where a pod heard on ips and port is reached and saved, or nil without an address.
func namedPodAt(ips []net.IP, port int) *namedPod {
	ip := preferredPodIP(ips)
	if ip == nil {
		return nil
	}
	p := &namedPod{addr: benchpodconfig.EnsurePort(net.JoinHostPort(ip.String(), fmt.Sprint(port)))}
	if lan := lanPodIP(ips); lan != nil {
		p.lanAddr = benchpodconfig.EnsurePort(net.JoinHostPort(lan.String(), fmt.Sprint(port)))
	}
	return p
}

// browsePodName is lookupPodName over the network: the browse discoverPods does, stopped as soon
// as the named pod answers, and asked again (podNameQueries) when it does not.
func browsePodName(short string) (*namedPod, error) {
	for _, wait := range podNameQueries {
		p, err := browsePodNameOnce(short, wait)
		if p != nil || err != nil {
			return p, err
		}
	}
	return nil, nil
}

// browsePodNameOnce is one mDNS browse of up to wait for the pod with the short id short. The
// library reports a pod once per browse, with the addresses of the interface it answered on
// first, so the first answer is the one there is.
func browsePodNameOnce(short string, wait time.Duration) (*namedPod, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	entries := make(chan *zeroconf.ServiceEntry, 16)
	drained := make(chan struct{})
	var found *namedPod
	go func() {
		defer close(drained)
		for e := range entries {
			if found == nil && entryNamesPod(e.Instance, e.HostName, short) {
				if found = namedPodAt(e.AddrIPv4, e.Port); found != nil {
					cancel()
				}
			}
		}
	}()

	// The same interface choice as discoverPods, for the same macOS reasons.
	opts := []zeroconf.ClientOption{zeroconf.SelectIPTraffic(zeroconf.IPv4)}
	if ifaces := multicastIfaces(); len(ifaces) > 0 {
		opts = append(opts, zeroconf.SelectIfaces(ifaces))
	}
	if err := zeroconf.Browse(ctx, mdnsService, mdnsDomain, entries, opts...); err != nil {
		return nil, fmt.Errorf("browse %s: %w", mdnsService, err)
	}
	<-drained
	return found, nil
}
