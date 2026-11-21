package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// recordingPod is a fake pod that answers each command by name and keeps every request.
type recordingPod struct {
	addr string
	mu   sync.Mutex
	reqs []map[string]any
}

func (p *recordingPod) cmds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.reqs {
		c, _ := r["cmd"].(string)
		if _, ok := r["add"]; ok {
			c += " add=" + r["add"].(string)
		}
		out = append(out, c)
	}
	return out
}

func newRecordingPod(t *testing.T, replies map[string]string) *recordingPod {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &recordingPod{addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(conn).ReadString('\n')
			var req map[string]any
			_ = json.Unmarshal([]byte(line), &req)
			p.mu.Lock()
			p.reqs = append(p.reqs, req)
			p.mu.Unlock()
			cmd, _ := req["cmd"].(string)
			reply, ok := replies[cmd]
			if !ok {
				reply = replies[""]
			}
			_, _ = io.WriteString(conn, reply+"\n")
			conn.Close()
		}
	}()
	return p
}

// lanStatus is a 4.0.1 pod's status over the LAN, with its USB link up at 10.237.210.109.
func lanStatus(paired bool, usbLink bool) string {
	return `{"status":"ok","data":{"device":"benchpod","version":"4.0.1","api":2,"min_client_api":2,"lan_pairing":true,"lan_paired":` +
		map[bool]string{true: "true", false: "false"}[paired] +
		`,"usb":{"net":true,"enabled":true,"link":` + map[bool]string{true: "true", false: "false"}[usbLink] +
		`,"ip":"10.237.210.109","host_ip":"10.237.210.110"}}}`
}

const lanClientsReply = `{"status":"ok","data":{"pairing":true,"count":1,"max":24,"clients":[{"name":"mac","spki_sha256":"00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"}]}}`

// withUSBLink points the pod's USB link address at addr ("" for a link that does not answer).
func withUSBLink(t *testing.T, addr string) {
	t.Helper()
	old := usbLinkAddr
	usbLinkAddr = func(ip string) string {
		if ip != "10.237.210.109" {
			t.Errorf("USB link ip = %q", ip)
		}
		if addr == "" {
			return "127.0.0.1:1"
		}
		return addr
	}
	t.Cleanup(func() { usbLinkAddr = old })
}

// The bug: two pods on USB, the app connected over the LAN. lan-clients add with the LAN address
// pairs over that pod's own USB link, not over "usb" (which asks which pod).
func TestLanClientsAddOverLANPairsThroughTheUSBLink(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(false, true), "": `{"status":"error","code":"BP0081","args":{"cmd":"lan_clients"}}`})
	usb := newRecordingPod(t, map[string]string{
		"ping":        `{"status":"ok","data":"pong"}`,
		"lan_tls":     `{"status":"ok","data":{"sha256":"abcd","subject":"CN=benchpod-c2f662"}}`,
		"lan_clients": lanClientsReply,
	})
	withUSBLink(t, usb.addr)

	var out bytes.Buffer
	g := &globalFlags{connection: lan.addr}
	if err := runLanClients(g, lanClientsOp{op: "add", name: "mac"}, &out); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out.String(), "Paired mac with the pod at "+lan.addr+" (over its USB link 127.0.0.1)") {
		t.Errorf("output:\n%s", out.String())
	}
	for _, c := range lan.cmds() {
		if strings.HasPrefix(c, "lan_clients") {
			t.Errorf("lan_clients went to the LAN address: %v", lan.cmds())
		}
	}
	if got := strings.Join(usb.cmds(), ","); !strings.Contains(got, "lan_clients add=mac") {
		t.Errorf("USB link got %s", got)
	}
	// The LAN certificate is pinned from the USB link, for the LAN address.
	if pin, ok := tcpclient.GetPin(lan.addr); !ok || pin.SHA256 != "abcd" {
		t.Errorf("pin = %+v, %v", pin, ok)
	}
}

func TestLanClientsAddOverLANWithoutTheUSBLinkSaysPlugItIn(t *testing.T) {
	for _, link := range []bool{true, false} { // link up but the cable is in another computer; link down
		lan := newRecordingPod(t, map[string]string{"status": lanStatus(false, link)})
		withUSBLink(t, "")
		g := &globalFlags{connection: lan.addr}
		err := runLanClients(g, lanClientsOp{op: "add", name: "mac"}, io.Discard)
		if err == nil {
			t.Fatal("add: no error")
		}
		for _, want := range []string{"not paired with the pod at " + lan.addr, "USB-C", "--connection embeddedci:<pod>", "BP0081"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("link=%v: error %q lacks %q", link, err, want)
			}
		}
		if strings.Contains(err.Error(), "[benchpod]") || strings.Contains(err.Error(), "pods on USB") {
			t.Errorf("error: %v", err)
		}
	}
}

func TestLanClientsAddOverLANWhenPairedAlready(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(true, false), "lan_clients": lanClientsReply})
	withUSBLink(t, "")
	var out bytes.Buffer
	if err := runLanClients(&globalFlags{connection: lan.addr}, lanClientsOp{op: "add", name: "mac"}, &out); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out.String(), "This computer (mac) is already paired with the pod at "+lan.addr) {
		t.Errorf("output:\n%s", out.String())
	}
	for _, c := range lan.cmds() {
		if strings.Contains(c, "add=") {
			t.Errorf("sent an add over the LAN: %v", lan.cmds())
		}
	}
}

func TestLanClientsListOverLANWhenPaired(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(true, true), "lan_clients": lanClientsReply})
	usb := newRecordingPod(t, map[string]string{"": `{"status":"ok","data":"pong"}`})
	withUSBLink(t, usb.addr)
	if err := runLanClients(&globalFlags{connection: lan.addr}, lanClientsOp{}, io.Discard); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(usb.cmds()) != 0 {
		t.Errorf("a paired computer's list went to the USB link: %v", usb.cmds())
	}
}

// Another pod's USB link (its LAN certificate differs from this computer's pin for the address)
// is never paired.
func TestLanClientsAddRefusesAnotherPodsUSBLink(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(false, true)})
	usb := newRecordingPod(t, map[string]string{
		"ping":        `{"status":"ok","data":"pong"}`,
		"lan_tls":     `{"status":"ok","data":{"sha256":"ffff"}}`,
		"lan_clients": lanClientsReply,
	})
	withUSBLink(t, usb.addr)
	if err := tcpclient.SetPinFingerprint(lan.addr, "abcd", ""); err != nil {
		t.Fatal(err)
	}
	err := runLanClients(&globalFlags{connection: lan.addr}, lanClientsOp{op: "add", name: "mac"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is not the pod at "+lan.addr) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range usb.cmds() {
		if strings.HasPrefix(c, "lan_clients") {
			t.Errorf("paired another pod: %v", usb.cmds())
		}
	}
}

// setup, for a pod it found only on the network, pairs through its USB link too.
func TestSetupPairsOverTheUSBLink(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(false, true)})
	usb := newRecordingPod(t, map[string]string{
		"ping":        `{"status":"ok","data":"pong"}`,
		"lan_tls":     `{"status":"ok","data":{"sha256":"abcd"}}`,
		"lan_clients": lanClientsReply,
	})
	withUSBLink(t, usb.addr)
	msg, err := (&realSetupEnv{g: &globalFlags{}}).pair(setupPod{addr: lan.addr})
	if err != nil || !strings.Contains(msg, "over its USB link (10.237.210.109)") {
		t.Fatalf("pair = %q, %v", msg, err)
	}
	if got := strings.Join(usb.cmds(), ","); !strings.Contains(got, "lan_clients add=") {
		t.Errorf("USB link got %s", got)
	}
}

// usbLinkPod is the pod's own USB link: it answers ping, its LAN certificate, and records the rest.
func usbLinkPod(t *testing.T, replies map[string]string) *recordingPod {
	t.Helper()
	all := map[string]string{"ping": `{"status":"ok","data":"pong"}`, "lan_tls": `{"status":"ok","data":{"sha256":"abcd"}}`}
	for k, v := range replies {
		all[k] = v
	}
	return newRecordingPod(t, all)
}

func sent(p *recordingPod, cmd string) bool {
	for _, c := range p.cmds() {
		if c == cmd {
			return true
		}
	}
	return false
}

// lan-pairing off over a LAN address from a computer that is not paired: the pod is plugged into
// this computer, so the change goes over its USB link instead of failing with BP0081.
func TestLanPairingOverLANGoesThroughTheUSBLink(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(false, true), "": `{"status":"error","code":"BP0081","args":{"cmd":"lan_policy"}}`})
	usb := usbLinkPod(t, map[string]string{"lan_policy": `{"status":"ok","data":{"pairing":false}}`})
	withUSBLink(t, usb.addr)

	var out bytes.Buffer
	if err := runLanPairing(&globalFlags{connection: lan.addr}, "off", &out); err != nil {
		t.Fatalf("lan-pairing off: %v", err)
	}
	if want := "LAN pairing on " + lan.addr + " (over its USB link 127.0.0.1) is now off"; !strings.Contains(out.String(), want) {
		t.Errorf("output:\n%s", out.String())
	}
	if sent(lan, "lan_policy") || !sent(usb, "lan_policy") {
		t.Errorf("LAN got %v, USB link got %v", lan.cmds(), usb.cmds())
	}
	// Reading it as a computer that is not paired goes over the link too.
	out.Reset()
	if err := runLanPairing(&globalFlags{connection: lan.addr}, "", &out); err != nil || !strings.Contains(out.String(), "over its USB link") {
		t.Errorf("show: %q, %v", out.String(), err)
	}
}

// A paired computer reads LAN pairing over the LAN; a change still goes over the USB link.
func TestLanPairingPairedReadStaysOnTheLAN(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(true, true), "lan_policy": `{"status":"ok","data":{"pairing":true}}`})
	usb := usbLinkPod(t, map[string]string{"lan_policy": `{"status":"ok","data":{"pairing":true}}`})
	withUSBLink(t, usb.addr)
	if err := runLanPairing(&globalFlags{connection: lan.addr}, "", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !sent(lan, "lan_policy") || len(usb.cmds()) != 0 {
		t.Errorf("LAN got %v, USB link got %v", lan.cmds(), usb.cmds())
	}
	if err := runLanPairing(&globalFlags{connection: lan.addr}, "on", io.Discard); err != nil {
		t.Fatal(err)
	}
	if !sent(usb, "lan_policy") {
		t.Errorf("USB link got %v", usb.cmds())
	}
}

// lan-tls switches over a LAN address go over the USB link; regenerate pins the new certificate
// for the LAN address, not the link.
func TestLanTLSChangesGoThroughTheUSBLink(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(true, true), "": `{"status":"error","code":"BP0042","args":{"setting":"lan_tls"}}`})
	usb := usbLinkPod(t, map[string]string{"lan_tls": `{"status":"ok","data":{"port":8443,"https_only":true,"scpi":true,"sha256":"beef","subject":"CN=benchpod-c2f662"}}`})
	withUSBLink(t, usb.addr)
	for _, change := range []map[string]any{{"scpi": true}, {"https_only": true}, {"regenerate": true}} {
		var out bytes.Buffer
		if err := runLanTLS(&globalFlags{connection: lan.addr}, change, &out, io.Discard); err != nil {
			t.Fatalf("%v: %v", change, err)
		}
		if !strings.Contains(out.String(), "LAN TLS of "+lan.addr+" (over its USB link 127.0.0.1)") {
			t.Errorf("%v: output:\n%s", change, out.String())
		}
	}
	if sent(lan, "lan_tls") {
		t.Errorf("a change went to the LAN: %v", lan.cmds())
	}
	if pin, ok := tcpclient.GetPin(lan.addr); !ok || pin.SHA256 != "beef" {
		t.Errorf("pin for the LAN address = %+v, %v", pin, ok)
	}
}

// Without the pod's USB link here, the change goes to the LAN and its refusal says what to do.
func TestLanTLSChangeWithoutTheUSBLinkIsRefused(t *testing.T) {
	lan := newRecordingPod(t, map[string]string{"status": lanStatus(true, false), "": `{"status":"error","code":"BP0042","args":{"setting":"lan_tls"}}`})
	withUSBLink(t, "")
	err := runLanTLS(&globalFlags{connection: lan.addr}, map[string]any{"scpi": true}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "BP0042") {
		t.Fatalf("err = %v", err)
	}
}

// BP0081 from a pod whose USB-C cable is in this computer: the hint says to pair now, not to
// plug it in.
func TestNotPairedHintWhenThePodIsPluggedInHere(t *testing.T) {
	refusal := `{"status":"error","code":"BP0081","args":{"cmd":"log_level"}}`
	for _, link := range []bool{true, false} {
		lan := newRecordingPod(t, map[string]string{"status": lanStatus(false, link), "": refusal})
		usb := usbLinkPod(t, nil)
		if link {
			withUSBLink(t, usb.addr)
		} else {
			withUSBLink(t, "")
		}
		pairHereFixes = map[string]string{}
		var logged strings.Builder
		log.SetOutput(&logged)
		code := run([]string{"log-level", "debug", "--connection", lan.addr})
		log.SetOutput(os.Stderr)
		if code != exitRefused {
			t.Fatalf("link=%v: exit %d", link, code)
		}
		got := logged.String()
		pairNow := "pair this computer now with `benchpod lan-clients add --connection " + lan.addr + "`"
		if link && (!strings.Contains(got, pairNow) || strings.Contains(got, "plug the pod into")) {
			t.Errorf("link up: %s", got)
		}
		if !link && (strings.Contains(got, "pair this computer now") || !strings.Contains(got, "plug the pod into this computer's USB-C port")) {
			t.Errorf("link down: %s", got)
		}
	}
}
