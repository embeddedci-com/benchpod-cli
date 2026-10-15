package main

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
)

// withPodNames replaces the mDNS lookup with a fixed table (short id -> pod) for one test and
// starts from an empty cache. It returns how many lookups ran per name.
func withPodNames(t *testing.T, pods map[string]*namedPod) map[string]int {
	t.Helper()
	calls := map[string]int{}
	old := lookupPodName
	lookupPodName = func(short string) (*namedPod, error) {
		calls[short]++
		return pods[short], nil
	}
	resetCache := func() {
		podNameCache.Lock()
		podNameCache.m = map[string]*namedPod{}
		podNameCache.Unlock()
	}
	resetCache()
	t.Cleanup(func() {
		lookupPodName = old
		resetCache()
	})
	return calls
}

func TestPodShortName(t *testing.T) {
	for in, want := range map[string]string{
		"benchpod-a1c5c7":       "a1c5c7",
		"a1c5c7":                "a1c5c7",
		"BenchPod-A1C5C7":       "a1c5c7",
		"benchpod-a1c5c7.local": "a1c5c7",
		"a1c5c7.local":          "a1c5c7",
		// Real hostnames and anything else keep going to the OS resolver.
		"benchpod.local":         "",
		"benchpod-a1c5c7x":       "",
		"a1c5c":                  "",
		"a1c5c7:8080":            "",
		"lab-a1c5c7":             "",
		"benchpod-g1c5c7":        "",
		"pod.example.com":        "",
		"192.168.1.5":            "",
		"benchpod-a1c5c7.lan":    "",
		"benchpod-a1c5c7.local.": "",
	} {
		if got := podShortName(in); got != want {
			t.Errorf("podShortName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTargetResolvesAPodNameOnTheLAN(t *testing.T) {
	withPodNames(t, map[string]*namedPod{
		"a1c5c7": {addr: "192.168.1.221:8080", lanAddr: "192.168.1.221:8080"},
	})
	for _, in := range []string{"benchpod-a1c5c7", "a1c5c7", "benchpod-a1c5c7.local"} {
		spec, err := parseTarget(in)
		if err != nil || !spec.IsNetwork() || spec.Addr != "192.168.1.221:8080" {
			t.Errorf("parseTarget(%q) = %+v, %v; want network 192.168.1.221:8080", in, spec, err)
		}
	}
}

func TestParseTargetFallsBackToUSBForAPodNotOnTheLAN(t *testing.T) {
	withPodNames(t, nil)
	spec, err := parseTarget("benchpod-a1c5c7")
	if err != nil || !spec.IsSerial() || spec.Pod != "a1c5c7" || spec.Device != "" {
		t.Fatalf("parseTarget = %+v, %v; want usb:benchpod-a1c5c7", spec, err)
	}
	// The network commands still go through it the way usb:<name> does.
	if spec.RequireNetwork("capture") == nil {
		t.Errorf("a USB spec should need the USB network adapter for network commands")
	}
}

func TestParseTargetLooksUpAPodNameOncePerRun(t *testing.T) {
	calls := withPodNames(t, nil)
	for i := 0; i < 3; i++ {
		if _, err := parseTarget("a1c5c7"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := parseTarget("benchpod-a1c5c7"); err != nil {
		t.Fatal(err)
	}
	if calls["a1c5c7"] != 1 {
		t.Errorf("looked up %d times, want 1", calls["a1c5c7"])
	}
}

func TestParseTargetLeavesHostnamesAlone(t *testing.T) {
	calls := withPodNames(t, nil)
	spec, err := parseTarget("benchpod.local")
	if err != nil || !spec.IsNetwork() || spec.Addr != "benchpod.local:8080" {
		t.Fatalf("parseTarget = %+v, %v", spec, err)
	}
	if len(calls) != 0 {
		t.Errorf("a hostname was looked up over mDNS: %v", calls)
	}
}

func TestEntryNamesPod(t *testing.T) {
	for _, c := range []struct {
		instance, hostname string
		want               bool
	}{
		{`BenchPod\ a1c5c7`, "benchpod-a1c5c7.local.", true},
		{"BenchPod a1c5c7", "", true},
		{"", "benchpod-a1c5c7.local.", true},
		{"BenchPod baea06", "benchpod-baea06.local.", false},
	} {
		if got := entryNamesPod(c.instance, c.hostname, "a1c5c7"); got != c.want {
			t.Errorf("entryNamesPod(%q, %q) = %v, want %v", c.instance, c.hostname, got, c.want)
		}
	}
}

func TestNamedPodAt(t *testing.T) {
	lan, link := net.IPv4(192, 168, 1, 221), net.IPv4(10, 237, 4, 2)
	if p := namedPodAt(nil, 8080); p != nil {
		t.Errorf("no address: %+v, want nil", p)
	}
	p := namedPodAt([]net.IP{lan, link}, 8080)
	if p == nil || p.addr != "10.237.4.2:8080" || p.lanAddr != "192.168.1.221:8080" {
		t.Errorf("LAN and USB link: %+v, want addr on the link and the LAN address saved", p)
	}
	p = namedPodAt([]net.IP{link}, 8080)
	if p == nil || p.addr != "10.237.4.2:8080" || p.lanAddr != "" {
		t.Errorf("USB link only: %+v", p)
	}
}

func TestSetConnectionWithAPodName(t *testing.T) {
	for _, c := range []struct {
		name string
		pods map[string]*namedPod
		arg  string
		want string
	}{
		{"on the LAN", map[string]*namedPod{"a1c5c7": {addr: "10.237.4.2:8080", lanAddr: "192.168.1.221:8080"}},
			"benchpod-a1c5c7", "192.168.1.221:8080"},
		{"on its USB link only", map[string]*namedPod{"a1c5c7": {addr: "10.237.4.2:8080"}},
			"a1c5c7", "usb:benchpod-a1c5c7"},
		{"not on the LAN", nil, "a1c5c7", "usb:benchpod-a1c5c7"},
	} {
		t.Run(c.name, func(t *testing.T) {
			withPodNames(t, c.pods)
			cfg := filepath.Join(t.TempDir(), "config.json")
			if code := run([]string{"set-connection", c.arg, "--config-file", cfg}); code != exitOK {
				t.Fatalf("exit %d", code)
			}
			saved, err := benchpodconfig.Load(cfg)
			if err != nil || saved.Connection != c.want {
				t.Fatalf("saved %+v, %v; want %q", saved, err, c.want)
			}
		})
	}
}
