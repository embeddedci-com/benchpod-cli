package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// jsonPod is a pod's JSON API that answers every line of every connection with handle.
type jsonPod struct {
	mu     sync.Mutex
	reqs   []map[string]any
	handle func(req map[string]any) string
}

func startJSONPod(t *testing.T, handle func(req map[string]any) string) (*jsonPod, string) {
	t.Helper()
	p := &jsonPod{handle: handle}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					var req map[string]any
					_ = json.Unmarshal([]byte(line), &req)
					p.mu.Lock()
					p.reqs = append(p.reqs, req)
					p.mu.Unlock()
					if _, err := c.Write([]byte(p.handle(req) + "\n")); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return p, ln.Addr().String()
}

func (p *jsonPod) cmds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.reqs {
		c, _ := r["cmd"].(string)
		out = append(out, c)
	}
	return out
}

func (p *jsonPod) last(cmd string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.reqs) - 1; i >= 0; i-- {
		if p.reqs[i]["cmd"] == cmd {
			return p.reqs[i]
		}
	}
	return nil
}

func fastWifiJoin(t *testing.T) {
	oldWait, oldPoll, oldScan, oldIP := wifiJoinWait, wifiJoinPoll, wifiScanPoll, consolePodIP
	wifiJoinWait, wifiJoinPoll, wifiScanPoll = 2*time.Second, 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { wifiJoinWait, wifiJoinPoll, wifiScanPoll, consolePodIP = oldWait, oldPoll, oldScan, oldIP })
	consolePodIP = func(*globalFlags) string { return "" }
}

// set-wifi: wifi_set over the USB network adapter, then wifi_status until the pod joined.
func TestSetWifiOverJSON(t *testing.T) {
	fastWifiJoin(t)
	var mu sync.Mutex
	joined := false
	pod, addr := startJSONPod(t, func(req map[string]any) string {
		mu.Lock()
		defer mu.Unlock()
		switch req["cmd"] {
		case "wifi_status":
			if joined {
				return okReply(map[string]any{"state": "connected", "connected": true, "configured": true, "ssid": "lab", "rssi_dbm": -60})
			}
			return okReply(map[string]any{"state": "disabled", "connected": false, "configured": false, "ssid": ""})
		case "blob_status":
			return okReply(map[string]any{"blobs": []any{map[string]any{"name": "esp", "state": "ok", "present": true}}})
		case "wifi_set":
			joined = true
			return okReply(map[string]any{"ssid": "lab"})
		case "status":
			return okReply(map[string]any{"ip": "192.168.1.77", "api": 2, "min_client_api": 2})
		}
		return codeReply("BP0002", map[string]any{"cmd": req["cmd"]})
	})
	g := &globalFlags{connection: "usb", usbAddr: addr}
	var ip string
	var err error
	out := captureStderr(t, func() { ip, err = runSetWifi(g, "lab", "s3cret", false) })
	if err != nil || ip != "192.168.1.77" {
		t.Fatalf("ip %q, err %v\n%s", ip, err, out)
	}
	if set := pod.last("wifi_set"); set["ssid"] != "lab" || set["password"] != "s3cret" {
		t.Fatalf("wifi_set %v", set)
	}
	if got := strings.Join(pod.cmds(), ","); !strings.HasPrefix(got, "wifi_status,blob_status,wifi_set,wifi_status") {
		t.Fatalf("requests %s", got)
	}
	if !strings.Contains(out, `Joined "lab", ip=192.168.1.77`) || strings.Contains(out, "s3cret") {
		t.Fatalf("stderr:\n%s", out)
	}
}

// When the USB link goes down with wifi_set, set-wifi follows the join on the address the
// console's status reports.
func TestSetWifiFollowsTheJoinOnTheNewAddress(t *testing.T) {
	fastWifiJoin(t)
	var mu sync.Mutex
	set := false
	_, usbAddr := startJSONPod(t, func(req map[string]any) string {
		mu.Lock()
		defer mu.Unlock()
		switch req["cmd"] {
		case "wifi_status":
			if set {
				return "the USB link is gone" // not a reply: the read fails
			}
			return okReply(map[string]any{"state": "disabled"})
		case "wifi_set":
			set = true
			return okReply(map[string]any{"ssid": "lab"})
		}
		return codeReply("BP0002", map[string]any{"cmd": req["cmd"]})
	})
	_, wifiAddr := startJSONPod(t, func(req map[string]any) string {
		switch req["cmd"] {
		case "wifi_status":
			return okReply(map[string]any{"state": "connected", "connected": true, "ssid": "lab"})
		case "status":
			return okReply(map[string]any{"ip": "127.0.0.1", "api": 2, "min_client_api": 2})
		}
		return codeReply("BP0002", map[string]any{"cmd": req["cmd"]})
	})
	g := &globalFlags{connection: "usb", usbAddr: usbAddr}
	host, port, _ := net.SplitHostPort(wifiAddr)
	oldPort := wifiPodPort
	wifiPodPort = port
	t.Cleanup(func() { wifiPodPort = oldPort })
	consolePodIP = func(*globalFlags) string { return host }
	var ip string
	var err error
	captureStderr(t, func() { ip, err = runSetWifi(g, "lab", "", true) })
	if err != nil || ip != "127.0.0.1" {
		t.Fatalf("ip %q, err %v", ip, err)
	}
}

// Through a Wi-Fi bridge: the bridge keeps the credentials; a wrong password is an error.
func TestSetWifiThroughABridge(t *testing.T) {
	fastWifiJoin(t)
	pod, addr := startJSONPod(t, func(req map[string]any) string {
		switch req["cmd"] {
		case "wifi_status":
			return okReply(map[string]any{"state": "auth-failed", "ssid": "lab", "via": "wifi-bridge",
				"bridge": map[string]any{"paired": true, "problem": nil}})
		case "wifi_set":
			return okReply(map[string]any{"ssid": "lab", "via": "wifi-bridge"})
		}
		return codeReply("BP0002", map[string]any{"cmd": req["cmd"]})
	})
	var err error
	captureStderr(t, func() { _, err = runSetWifi(&globalFlags{connection: "usb", usbAddr: addr}, "lab", "bad", false) })
	if err == nil || !strings.Contains(err.Error(), "wrong password") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range pod.cmds() {
		if c == "blob_status" {
			t.Fatal("checked the ESP32-C3 image through a bridge")
		}
	}
}

func TestShowNetworkScanAndClearWifi(t *testing.T) {
	fastWifiJoin(t)
	scans := 0
	pod, addr := startJSONPod(t, func(req map[string]any) string {
		switch req["cmd"] {
		case "wifi_status":
			return okReply(map[string]any{"state": "connected", "connected": true, "ssid": "lab", "rssi_dbm": -61})
		case "status":
			return okReply(map[string]any{"ip": "192.168.1.77", "api": 2, "min_client_api": 2})
		case "wifi_scan":
			scans++
			if scans < 3 {
				return okReply(map[string]any{"state": "scanning", "id": 3})
			}
			return okReply(map[string]any{"state": "done", "id": 3, "networks": []any{
				map[string]any{"ssid": "lab", "rssi": -48, "channel": 6, "auth": "wpa2/wpa3"},
				map[string]any{"ssid": "", "rssi": -80, "channel": 1, "auth": "open"}}})
		case "wifi_clear":
			return okReply("cleared")
		}
		return codeReply("BP0002", map[string]any{"cmd": req["cmd"]})
	})
	out := t.TempDir() + "/out.txt"
	read := func() string { b, _ := os.ReadFile(out); return string(b) }
	if code := run([]string{"show-network", "--connection", addr, "--output-filename", out}); code != exitOK {
		t.Fatalf("show-network: exit %d", code)
	}
	if got := read(); got != "IP:    192.168.1.77\nSSID:  lab\nState: connected\nRSSI:  -61 dBm\n" {
		t.Fatalf("show-network:\n%s", got)
	}
	captureStderr(t, func() {
		if code := run([]string{"scan-wifi", "--connection", addr, "--output-filename", out}); code != exitOK {
			t.Fatalf("scan-wifi: exit %d", code)
		}
	})
	if got := read(); !strings.Contains(got, "-48    6    wpa2/wpa3  lab\n") || !strings.Contains(got, "(hidden)") {
		t.Fatalf("scan-wifi:\n%s", got)
	}
	if r := pod.last("wifi_scan"); r["id"] != float64(3) {
		t.Fatalf("poll %v", r)
	}
	captureStderr(t, func() {
		if code := run([]string{"clear-wifi", "--connection", addr}); code != exitOK {
			t.Fatalf("clear-wifi: exit %d", code)
		}
	})
	if pod.last("wifi_clear") == nil {
		t.Fatal("no wifi_clear")
	}
}
