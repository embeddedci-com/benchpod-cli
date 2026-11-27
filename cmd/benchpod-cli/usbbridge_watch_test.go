package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeBridge stubs everything --watch touches: the local links, the pod's JSON API and the one
// console call. It records every console use.
type fakeBridge struct {
	links    []usbLink
	proxy    string // the pod's current proxy "host:port", "" = none
	apiCalls int
	console  []string // proxy addresses set over the console ("" = clear)
	onSet    func()   // called after the first console call that sets a proxy
}

func withFakeBridge(t *testing.T, f *fakeBridge) {
	t.Helper()
	oldLinks, oldAPI, oldSet := localUSBLinks, bridgePodAPI, bridgeSetProxy
	localUSBLinks = func() []usbLink { return f.links }
	bridgePodAPI = func(_ context.Context, _ string, cmd map[string]any) (json.RawMessage, error) {
		f.apiCalls++
		if cmd["cmd"] != "cloud_proxy" {
			return nil, errors.New("unexpected")
		}
		if f.proxy == "" {
			return json.RawMessage(`{}`), nil
		}
		host, port, _ := net.SplitHostPort(f.proxy)
		return json.RawMessage(fmt.Sprintf(`{"host":%q,"port":%s,"auth":false}`, host, port)), nil
	}
	bridgeSetProxy = func(_ string, addr string) error {
		f.console = append(f.console, addr)
		f.proxy = addr
		if addr != "" && f.onSet != nil {
			f.onSet()
		}
		return nil
	}
	t.Cleanup(func() { localUSBLinks, bridgePodAPI, bridgeSetProxy = oldLinks, oldAPI, oldSet })
}

// freeBridgePort returns a port nothing listens on now. Fixed ports collide with the OS's ephemeral
// ports (and with a listener of the test before) on a busy runner: "bind: address already in use".
func freeBridgePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func runWatchFor(t *testing.T, d time.Duration, port int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := watchBridge(ctx, &globalFlags{connection: "usb"}, port, false, 5*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
}

// Idle (no pod, or Wi-Fi configured on it so its USB networking is off): never the console, never
// the pod.
func TestWatchBridgeNeverTouchesTheConsoleWhileIdle(t *testing.T) {
	f := &fakeBridge{}
	withFakeBridge(t, f)
	runWatchFor(t, 100*time.Millisecond, freeBridgePort(t))
	if len(f.console) != 0 || f.apiCalls != 0 {
		t.Fatalf("idle watch used the console %d times and the API %d times", len(f.console), f.apiCalls)
	}
}

// The pod's proxy already points here (set by an earlier run): no console at all.
func TestWatchBridgeKeepsAProxyThatIsAlreadyOurs(t *testing.T) {
	port := freeBridgePort(t)
	f := &fakeBridge{links: []usbLink{{host: "127.0.0.1", pod: "127.0.0.2"}}, proxy: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	withFakeBridge(t, f)
	runWatchFor(t, 100*time.Millisecond, port)
	if len(f.console) != 0 {
		t.Fatalf("console used %v", f.console)
	}
}

// A new pod: one console call to set the proxy, one to clear it on stop, and nothing in between.
func TestWatchBridgeSetsTheProxyOnceAndClearsItOnStop(t *testing.T) {
	f := &fakeBridge{links: []usbLink{{host: "127.0.0.1", pod: "127.0.0.2"}}}
	withFakeBridge(t, f)
	// Stop the watch once the proxy is set (event driven); the long deadline is only a safety net.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f.onSet = cancel
	port := freeBridgePort(t)
	if err := watchBridge(ctx, &globalFlags{connection: "usb"}, port, false, 5*time.Millisecond, false); err != nil {
		t.Fatal(err)
	}
	if len(f.console) != 2 || f.console[0] != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) || f.console[1] != "" {
		t.Fatalf("console calls %q, want set once then clear", f.console)
	}
}

// Another proxy on the pod: left alone, and no console.
func TestWatchBridgeLeavesSomeoneElsesProxy(t *testing.T) {
	f := &fakeBridge{links: []usbLink{{host: "127.0.0.1", pod: "127.0.0.2"}}, proxy: "proxy.corp:3128"}
	withFakeBridge(t, f)
	runWatchFor(t, 60*time.Millisecond, freeBridgePort(t))
	if len(f.console) != 0 || f.proxy != "proxy.corp:3128" {
		t.Fatalf("console %q proxy %q", f.console, f.proxy)
	}
}

func TestShortIDOf(t *testing.T) {
	if got := shortIDOf("uuoGf1v6n9yeBZZVzswU4MDC1Lndgx6OXY5FG9_dUJQ"); got != "baea06" {
		t.Fatalf("got %q", got)
	}
}

// Without --watch the bridge stops with the reason instead of waiting.
func TestBridgeWithoutWatchSaysWhyItCannotStart(t *testing.T) {
	withFakeBridge(t, &fakeBridge{})
	err := watchBridge(context.Background(), &globalFlags{connection: "usb"}, freeBridgePort(t), false, time.Millisecond, true)
	if err == nil || !strings.Contains(err.Error(), "clear-wifi") || !strings.Contains(err.Error(), "usb-reattach") {
		t.Fatalf("err = %v", err)
	}
}
