package serialconsole

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// wifi-show through a Wi-Fi bridge (firmware with bridge support): the state is the bridge's,
// the IP the pod's Ethernet lease, and "via"/"problem" follow the rssi line.
func TestWifiShowThroughBridge(t *testing.T) {
	raw := "wifi-show\r\n" +
		"  ssid: Lab\r\n" +
		"  state: auth-failed\r\n" +
		"  ip: 192.168.1.50\r\n" +
		"  rssi: \r\n" +
		"  via: wifi-bridge 24:0a:c4:00:11:22 fw 0.1.0 hw A, paired\r\n" +
		"  problem: the bridge could not join: wrong password\r\n" +
		"> "
	st := parseWifiShow(raw)
	if !st.ViaBridge() || st.BridgeJoined() {
		t.Fatalf("via=%q state=%q", st.Via, st.State)
	}
	if st.Via != "wifi-bridge 24:0a:c4:00:11:22 fw 0.1.0 hw A, paired" || st.Problem == "" {
		t.Fatalf("via=%q problem=%q", st.Via, st.Problem)
	}
	if !strings.Contains(st.BridgeJoinFailure(), "wrong password") {
		t.Fatalf("failure: %q", st.BridgeJoinFailure())
	}
	st.State = "connected"
	if !st.BridgeJoined() || st.BridgeJoinFailure() != "" {
		t.Fatal("connected should count as joined")
	}
	// Without a bridge nothing changes: no via, and the ESP32-C3 states are not the bridge's.
	plain := parseWifiShow("wifi-show\r\n  ssid: Lab\r\n  state: connected\r\n  ip: 10.0.0.2\r\n  rssi: -50 dBm\r\n> ")
	if plain.ViaBridge() || plain.BridgeJoined() || plain.Via != "" {
		t.Fatalf("plain: %+v", plain)
	}
}

// A refusal ("  wifi-set: <why>", nothing saved) is an error naming the reason.
func TestWifiSetRefused(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("wifi-set \"Lab\" \"***\"\r\n")
		out.WriteString("  wifi-set: not-paired\r\n")
		out.WriteString("> ")
	}}
	res, err := newConsole(fc).WifiSet(testContext(t), "Lab", "secret12")
	if err == nil || !strings.Contains(err.Error(), "not-paired") || res.Persisted {
		t.Fatalf("err=%v persisted=%v", err, res.Persisted)
	}
}

func TestWifiScan(t *testing.T) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString("wifi-scan\r\n")
		out.WriteString("scan -48 6 wpa2/wpa3 Lab Net 5G\r\n")
		out.WriteString("scan -71 11 open \r\n")
		out.WriteString("scan done 2\r\n")
		out.WriteString("> ")
	}}
	nets, err := newConsole(fc).WifiScan(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []WifiNetwork{{SSID: "Lab Net 5G", RSSI: -48, Channel: 6, Auth: "wpa2/wpa3"}, {RSSI: -71, Channel: 11, Auth: "open"}}
	if len(nets) != len(want) || nets[0] != want[0] || nets[1] != want[1] {
		t.Fatalf("got %+v", nets)
	}
}

func TestWifiScanErrors(t *testing.T) {
	if _, err := parseWifiScan("wifi-scan\r\nwifi-scan error no Wi-Fi bridge on the Ethernet port\r\n> "); err == nil ||
		!strings.Contains(err.Error(), "no Wi-Fi bridge") {
		t.Fatalf("err=%v", err)
	}
	if _, err := parseWifiScan("wifi-scan\r\n  unknown command 'wifi-scan' (try 'help')\r\n> "); !errors.Is(err, ErrNoWifiScan) {
		t.Fatalf("err=%v", err)
	}
	if _, err := parseWifiScan("wifi-scan\r\nscan -48 6 wpa2 Lab\r\n> "); err == nil {
		t.Fatal("a list without \"scan done\" should be an error")
	}
	if nets, err := parseWifiScan("wifi-scan\r\nscan done 0\r\n> "); err != nil || len(nets) != 0 {
		t.Fatalf("empty: %v %v", nets, err)
	}
}
