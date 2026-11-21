package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// Pairing through the pod's USB link when the connection is a LAN address.
//
// A computer that is not paired still gets `status` over the LAN, and its reply carries the pod's
// own USB network address (`usb.ip`, 10.237.x.y) when the pod's USB-C cable is in a computer. That
// address is unique per pod and only answers from the computer the cable is in, and the pod
// trusts the USB link, so `lan_clients` changes work there. So `lan-clients add --connection
// 192.168.1.164` (or a pod name, or a .local name) pairs the right pod even with several pods on
// USB, without asking which.

// lanPodState is what a pod's status says about pairing and its USB link.
type lanPodState struct {
	known   bool   // the pod has LAN pairing (firmware 4.0.1+) and status answered
	pairing bool   // pairing is on
	paired  *bool  // this computer is paired; nil when the connection needs no pairing
	usbIP   string // the pod's address on its USB link, "" when the link is down
}

// readLanPodState reads status at addr. known is false when status does not answer or the
// firmware has no pairing.
func readLanPodState(ctx context.Context, addr string) lanPodState {
	data, err := (&tcpclient.Client{Addr: addr}).Command(ctx, map[string]any{"cmd": "status", "caps": false})
	if err != nil {
		return lanPodState{}
	}
	var st struct {
		Pairing *bool `json:"lan_pairing"`
		Paired  *bool `json:"lan_paired"`
		USB     *struct {
			Link bool   `json:"link"`
			IP   string `json:"ip"`
		} `json:"usb"`
	}
	if json.Unmarshal(data, &st) != nil {
		return lanPodState{}
	}
	var s lanPodState
	if st.Pairing != nil {
		s = lanPodState{known: true, pairing: *st.Pairing, paired: st.Paired}
	}
	// The USB link is there on firmware without pairing too (LAN TLS changes go over it).
	if st.USB != nil && st.USB.Link && tcpclient.IsUSBLinkAddr(strings.TrimSpace(st.USB.IP)) {
		s.usbIP = strings.TrimSpace(st.USB.IP)
	}
	return s
}

// usbLinkAddr is where the pod's USB link address is dialed (plaintext 8080). A variable so tests
// can point it at a fake pod.
var usbLinkAddr = func(ip string) string { return net.JoinHostPort(ip, tcpclient.PlainPort) }

// reachUSBLink returns the dial address of the pod's USB link when it answers from this computer
// and is the same pod as the one at lanAddr (its LAN certificate matches this computer's pin for
// lanAddr, when there is one), else "". A different pod is an error.
func reachUSBLink(ctx context.Context, lanAddr, ip string) (string, error) {
	if ip == "" {
		return "", nil
	}
	addr := usbLinkAddr(ip)
	c := &tcpclient.Client{Addr: addr}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err := c.Command(pctx, map[string]any{"cmd": "ping"})
	cancel()
	if err != nil {
		return "", nil // the cable is in another computer, or the link is not up here
	}
	if pin, ok := tcpclient.GetPin(lanAddr); ok {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if data, err := c.Command(tctx, map[string]any{"cmd": "lan_tls"}); err == nil {
			var r lanTLSReply
			if json.Unmarshal(data, &r) == nil && r.SHA256 != "" && !strings.EqualFold(r.SHA256, pin.SHA256) {
				return "", fmt.Errorf("the pod on this computer's USB link (%s) is not the pod at %s (their LAN certificates differ), "+
					"so this computer was not paired. Plug the pod at %s into this computer over USB-C, or pass --connection usb:<its name>",
					ip, lanAddr, lanAddr)
			}
		}
	}
	return addr, nil
}

// pinLanCertOverUSBLink pins the pod's LAN certificate (read on its USB link at usbAddr) for
// lanAddr, when this computer has no pin for it yet: later LAN connections then check they reach
// this pod. Best effort.
func pinLanCertOverUSBLink(ctx context.Context, usbAddr, lanAddr string) {
	if _, ok := tcpclient.GetPin(lanAddr); ok {
		return
	}
	data, err := (&tcpclient.Client{Addr: usbAddr}).Command(ctx, map[string]any{"cmd": "lan_tls"})
	if err != nil {
		return
	}
	var r lanTLSReply
	if json.Unmarshal(data, &r) == nil && r.SHA256 != "" {
		_ = tcpclient.SetPinFingerprint(lanAddr, r.SHA256, r.Subject)
	}
}

// overUSBLink is where a change the pod takes only over USB or the cloud (LAN pairing, LAN TLS,
// the cloud CA and proxy) goes when the connection is the LAN address lanAddr: the same pod's USB
// link when it answers from this computer, with a label that names both; else lanAddr itself,
// and the pod's refusal then says what to do. A USB link address stays as it is. need, when not
// nil, decides from the pod's status whether to go over the USB link at all.
func overUSBLink(ctx context.Context, lanAddr string, need func(lanPodState) bool) (dial, label string, err error) {
	if isUSBLinkAddr(lanAddr) {
		return lanAddr, tcpclient.ShownAddr(lanAddr), nil
	}
	st := readLanPodState(ctx, lanAddr)
	if need != nil && !need(st) {
		return lanAddr, tcpclient.ShownAddr(lanAddr), nil
	}
	usb, err := reachUSBLink(ctx, lanAddr, st.usbIP)
	if err != nil || usb == "" {
		return lanAddr, tcpclient.ShownAddr(lanAddr), err
	}
	return usb, usbLinkLabel(lanAddr, usb), nil
}

func usbLinkLabel(lanAddr, usb string) string {
	return tcpclient.ShownAddr(lanAddr) + " (over its USB link " + hostOf(usb) + ")"
}

// notPairedHere says the pod has pairing on and this computer is not paired with it (it may
// only read status over the LAN).
func notPairedHere(st lanPodState) bool {
	return st.known && st.pairing && st.paired != nil && !*st.paired
}

// targetOverUSBLink is overUSBLink for a podClient target: a LAN target becomes the USB link's
// when it answers here. lanAddr is the LAN address it stands for ("" when t is unchanged).
func targetOverUSBLink(ctx context.Context, t *podTarget, need func(lanPodState) bool) (nt *podTarget, lanAddr string, err error) {
	if !t.spec.IsNetwork() || isUSBLinkAddr(t.spec.Addr) {
		return t, "", nil
	}
	dial, label, err := overUSBLink(ctx, t.spec.Addr, need)
	if err != nil || dial == t.spec.Addr {
		return t, "", err
	}
	return &podTarget{spec: ConnSpec{Kind: connNetwork, Addr: dial}, client: &tcpclient.Client{Addr: dial}, label: label}, t.spec.Addr, nil
}

// errNeedsUSBLink is what lan-clients says when a LAN connection cannot reach the pod's USB link.
func errNeedsUSBLink(what, lanAddr string, notPaired bool) error {
	if notPaired {
		return fmt.Errorf("%s: this computer is not paired with the pod at %s, and the pod is not plugged into this computer over USB, "+
			"so it cannot pair it from here. Plug the pod's USB-C port (USB-C IN, on the back) into this computer and run this again, "+
			"or pair it through the cloud: --connection embeddedci:<pod> (BP0081)", what, lanAddr)
	}
	return fmt.Errorf("%s: the paired computers are changed over USB or the cloud, never over the network, and the pod at %s "+
		"is not plugged into this computer over USB. Plug the pod's USB-C port (USB-C IN, on the back) into this computer and run this again, "+
		"or use the cloud: --connection embeddedci:<pod>", what, lanAddr)
}

// errAlreadyPaired tells lan-clients add that this computer is paired already (nothing to change).
var errAlreadyPaired = errors.New("already paired")

// lanClientsRoute decides where lan-clients sends op for the LAN address lanAddr: the pod's USB
// link address (a change, or a list this computer may not read over the LAN), "" for lanAddr
// itself, errAlreadyPaired for `add` of this computer when it is paired and the USB link is not
// here, or an error saying what to do.
func lanClientsRoute(ctx context.Context, lanAddr string, o lanClientsOp) (string, error) {
	st := readLanPodState(ctx, lanAddr)
	if !st.known {
		return "", nil // no pairing on this firmware, or no answer: the command says so itself
	}
	notPaired := notPairedHere(st)
	if o.op == "" && !notPaired {
		return "", nil // a paired computer lists over the LAN
	}
	usb, err := reachUSBLink(ctx, lanAddr, st.usbIP)
	if err != nil {
		return "", err
	}
	if usb != "" {
		return usb, nil
	}
	if o.op == "add" && !o.newKey && st.pairing && st.paired != nil && *st.paired {
		return "", errAlreadyPaired
	}
	what := "lan-clients"
	if o.op != "" {
		what += " " + o.op
	}
	return "", errNeedsUSBLink(what, lanAddr, notPaired)
}

// pairOverUSBLink is setup's pairing for a pod found only at the LAN address lanAddr: through the
// pod's USB link when it answers from here. ok is false when the link is not here.
func pairOverUSBLink(lanAddr string) (msg string, ok bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := readLanPodState(ctx, lanAddr)
	if !st.known {
		return "", false, nil
	}
	usb, err := reachUSBLink(ctx, lanAddr, st.usbIP)
	if err != nil || usb == "" {
		return "", err != nil, err
	}
	key, err := tcpclient.ClientKeyHash()
	if err != nil {
		return "", true, fmt.Errorf("pair: %w", err)
	}
	name := tcpclient.PairingName()
	if _, err := lanClientsAll(ctx, &tcpclient.Client{Addr: usb}, map[string]any{"cmd": "lan_clients", "add": name, "spki_sha256": key}); err != nil {
		return "", true, fmt.Errorf("pair this computer over the pod's USB link: %w", err)
	}
	pinLanCertOverUSBLink(ctx, usb, lanAddr)
	return fmt.Sprintf("Paired this computer (%s) with it over its USB link (%s), so it can drive the pod over the network too.", name, st.usbIP), true, nil
}
