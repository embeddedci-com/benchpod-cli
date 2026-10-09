package main

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// fakeSetupEnv records what setup did.
type fakeSetupEnv struct {
	pods      []setupPod
	findErr   error
	leaseAddr string // what waitForAddress returns
	wifiAddr  string // what setWifi returns
	laMV      int    // the pod's current LA voltage
	cloud     cloudState
	who       string
	account   map[string]string
	afterReg  cloudState // cloud state after register

	calls      []string
	wifiSSID   string
	wifiPW     string
	setMV      int
	registered string
	saved      string
}

func (f *fakeSetupEnv) find() ([]setupPod, error) {
	f.calls = append(f.calls, "find")
	return f.pods, f.findErr
}
func (f *fakeSetupEnv) waitForAddress(string, time.Duration) (string, error) {
	f.calls = append(f.calls, "wait")
	if f.leaseAddr == "" {
		return "", errors.New("no lease")
	}
	return f.leaseAddr, nil
}
func (f *fakeSetupEnv) setWifi(_, ssid, pw string) (string, error) {
	f.calls = append(f.calls, "wifi")
	f.wifiSSID, f.wifiPW = ssid, pw
	return f.wifiAddr, nil
}
func (f *fakeSetupEnv) readPassword() (string, error) { return "typed-pw", nil }
func (f *fakeSetupEnv) laVoltage(_ string, mv int) (int, error) {
	if mv != 0 {
		f.calls = append(f.calls, "set-la")
		f.setMV, f.laMV = mv, mv
	}
	return f.laMV, nil
}
func (f *fakeSetupEnv) cloudStatus(string) cloudState { return f.cloud }
func (f *fakeSetupEnv) signedIn() string              { return f.who }
func (f *fakeSetupEnv) login() error {
	f.calls = append(f.calls, "login")
	f.who = "you@example.com"
	return nil
}
func (f *fakeSetupEnv) accountDevices() map[string]string { return f.account }
func (f *fakeSetupEnv) register(addr string) error {
	f.calls = append(f.calls, "register")
	f.registered = addr
	f.cloud = f.afterReg
	return nil
}
func (f *fakeSetupEnv) save(target string) error {
	f.calls = append(f.calls, "save")
	f.saved = target
	return nil
}

func (f *fakeSetupEnv) did(call string) bool {
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func runSetupWith(t *testing.T, env *fakeSetupEnv, input string, o setupOpts) (string, error) {
	t.Helper()
	var out bytes.Buffer
	ui := &setupUI{in: bufio.NewReader(strings.NewReader(input)), out: &out, yes: o.yes}
	err := runSetup(env, ui, o)
	return out.String(), err
}

var registeredMine = cloudState{known: true, Configured: true, State: "connected", DeviceID: "dev-1"}

func TestSetupOnADonePodOnlySetsTheRequestedVoltage(t *testing.T) {
	env := &fakeSetupEnv{
		pods:    []setupPod{{device: "/dev/cu.usbmodem1", addr: "192.168.1.221:8080", firmware: "v3.8.0", cloud: registeredMine}},
		laMV:    3300,
		cloud:   registeredMine,
		who:     "you@example.com",
		account: map[string]string{"dev-1": "benchpod-baea06"},
	}
	out, err := runSetupWith(t, env, "", setupOpts{laVoltageMV: 3300, yes: true})
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, c := range []string{"wait", "wifi", "login", "register"} {
		if env.did(c) {
			t.Errorf("setup ran %s on a pod that needs nothing\n%s", c, out)
		}
	}
	if env.setMV != 3300 || env.saved != "192.168.1.221:8080" {
		t.Errorf("set %d, saved %q", env.setMV, env.saved)
	}
	for _, want := range []string{"done: the pod is on the network", "registered to your account (you@example.com) as benchpod-baea06",
		"--benchpod-connection=192.168.1.221", "LA11", "benchpod-getting-started"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSetupKeepsAVoltageAlreadySet(t *testing.T) {
	env := &fakeSetupEnv{pods: []setupPod{{addr: "10.0.0.5:8080", cloud: registeredMine}}, laMV: 1800, cloud: registeredMine}
	out, err := runSetupWith(t, env, "", setupOpts{yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if env.did("set-la") || !strings.Contains(out, "already 1.8 V") {
		t.Fatalf("calls %v\n%s", env.calls, out)
	}
	// Registered, not signed in: no login, no register, just the advice.
	if env.did("login") || env.did("register") || !strings.Contains(out, "activation email") {
		t.Fatalf("calls %v\n%s", env.calls, out)
	}
}

func TestSetupAsksForTheVoltageWhenUnset(t *testing.T) {
	env := &fakeSetupEnv{pods: []setupPod{{addr: "10.0.0.5:8080"}}}
	out, err := runSetupWith(t, env, "2\n", setupOpts{noRegister: true})
	if err != nil {
		t.Fatal(err)
	}
	if env.setMV != 1800 {
		t.Fatalf("set %d\n%s", env.setMV, out)
	}
	// Under --yes there is nobody to ask: fail and name the flag.
	env = &fakeSetupEnv{pods: []setupPod{{addr: "10.0.0.5:8080"}}}
	_, err = runSetupWith(t, env, "", setupOpts{yes: true})
	if !errors.Is(err, errNeedsAnswer) || !strings.Contains(err.Error(), "--la-voltage") {
		t.Fatalf("got %v", err)
	}
	if env.did("save") {
		t.Fatal("saved after failing")
	}
}

func TestSetupWaitsForAnEthernetLease(t *testing.T) {
	env := &fakeSetupEnv{pods: []setupPod{{device: "/dev/cu.usbmodem1"}}, leaseAddr: "10.0.0.7:8080", laMV: 3300}
	out, err := runSetupWith(t, env, "1\n", setupOpts{noRegister: true, ethernetWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !env.did("wait") || env.did("wifi") || env.saved != "10.0.0.7:8080" {
		t.Fatalf("calls %v saved %q\n%s", env.calls, env.saved, out)
	}
}

func TestSetupJoinsWifi(t *testing.T) {
	// Asked interactively.
	env := &fakeSetupEnv{pods: []setupPod{{device: "/dev/cu.usbmodem1"}}, wifiAddr: "10.0.0.8:8080", laMV: 3300}
	if _, err := runSetupWith(t, env, "2\nlab-net\n", setupOpts{noRegister: true}); err != nil {
		t.Fatal(err)
	}
	if env.wifiSSID != "lab-net" || env.wifiPW != "typed-pw" || env.saved != "10.0.0.8:8080" {
		t.Fatalf("ssid %q pw %q saved %q", env.wifiSSID, env.wifiPW, env.saved)
	}
	// From flags, the password on stdin, and the address only reported afterwards.
	env = &fakeSetupEnv{pods: []setupPod{{device: "/dev/cu.usbmodem1"}}, leaseAddr: "10.0.0.9:8080", laMV: 3300}
	if _, err := runSetupWith(t, env, "s3cret\n", setupOpts{ssid: "lab-net", passwordStdin: true, yes: true, noRegister: true}); err != nil {
		t.Fatal(err)
	}
	if env.wifiPW != "s3cret" || !env.did("wait") || env.saved != "10.0.0.9:8080" {
		t.Fatalf("pw %q calls %v saved %q", env.wifiPW, env.calls, env.saved)
	}
	// --yes with no network and no --ssid cannot pick for the user.
	env = &fakeSetupEnv{pods: []setupPod{{device: "/dev/cu.usbmodem1"}}}
	_, err := runSetupWith(t, env, "", setupOpts{yes: true})
	if !errors.Is(err, errNeedsAnswer) || !strings.Contains(err.Error(), "--ssid") {
		t.Fatalf("got %v", err)
	}
	// --yes with --ssid but no --password-stdin.
	env = &fakeSetupEnv{pods: []setupPod{{device: "/dev/cu.usbmodem1"}}}
	_, err = runSetupWith(t, env, "", setupOpts{yes: true, ssid: "lab-net"})
	if !errors.Is(err, errNeedsAnswer) || !strings.Contains(err.Error(), "--password-stdin") {
		t.Fatalf("got %v", err)
	}
}

func TestSetupRegistersAnUnregisteredPod(t *testing.T) {
	unreg := cloudState{known: true}
	env := &fakeSetupEnv{
		pods: []setupPod{{addr: "10.0.0.5:8080", cloud: unreg}}, cloud: unreg, laMV: 3300,
		afterReg: registeredMine, account: map[string]string{"dev-1": "bench-01"},
	}
	out, err := runSetupWith(t, env, "", setupOpts{yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if !env.did("login") || env.registered != "10.0.0.5:8080" || !strings.Contains(out, "embeddedci:bench-01") {
		t.Fatalf("calls %v\n%s", env.calls, out)
	}
	// Signed in already: no login. Declined: nothing.
	env = &fakeSetupEnv{pods: []setupPod{{addr: "10.0.0.5:8080", cloud: unreg}}, cloud: unreg, laMV: 3300, who: "me@x.com"}
	if _, err := runSetupWith(t, env, "n\n", setupOpts{}); err != nil {
		t.Fatal(err)
	}
	if env.did("login") || env.did("register") || !env.did("save") {
		t.Fatalf("calls %v", env.calls)
	}
	// --no-register.
	env = &fakeSetupEnv{pods: []setupPod{{addr: "10.0.0.5:8080", cloud: unreg}}, cloud: unreg, laMV: 3300}
	if _, err := runSetupWith(t, env, "", setupOpts{yes: true, noRegister: true}); err != nil || env.did("register") {
		t.Fatalf("err %v calls %v", err, env.calls)
	}
}

func TestSetupLeavesAPodOnAnotherAccountAlone(t *testing.T) {
	env := &fakeSetupEnv{
		pods:  []setupPod{{addr: "10.0.0.5:8080", cloud: registeredMine}},
		cloud: registeredMine, laMV: 3300, who: "new@x.com", account: map[string]string{"other": "x"},
	}
	out, err := runSetupWith(t, env, "", setupOpts{yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if env.did("register") || !strings.Contains(out, "benchpod login --force") || !strings.Contains(out, "different embeddedci.com account") {
		t.Fatalf("calls %v\n%s", env.calls, out)
	}
}

func TestSetupNeedsExactlyOnePod(t *testing.T) {
	env := &fakeSetupEnv{}
	if _, err := runSetupWith(t, env, "", setupOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "no BenchPod found") {
		t.Fatalf("got %v", err)
	}
	env = &fakeSetupEnv{pods: []setupPod{{addr: "a:8080"}, {addr: "b:8080"}}}
	if _, err := runSetupWith(t, env, "", setupOpts{yes: true}); err == nil || !strings.Contains(err.Error(), "--connection") {
		t.Fatalf("got %v", err)
	}
}

func TestMergeSetupPodsMatchesUSBAndNetwork(t *testing.T) {
	serialPods := []serialconsole.SerialPod{
		{Device: "/dev/a", Firmware: "v3.8.0", IP: "10.0.0.5", CloudKnown: true},
		{Device: "/dev/b", IP: "0.0.0.0"},
	}
	netPods := []discoveredPod{
		{addr: "10.0.0.5:8080", reachable: true, sameAs: "/dev/a", cloud: registeredMine},
		{addr: "10.0.0.6:8080", reachable: true},
		{addr: "10.0.0.7:8080"}, // a stale record: not a pod to set up
	}
	pods := mergeSetupPods(serialPods, netPods)
	if len(pods) != 3 {
		t.Fatalf("got %+v", pods)
	}
	if p := pods[0]; p.device != "/dev/a" || p.addr != "10.0.0.5:8080" || !p.cloud.Configured || p.firmware != "v3.8.0" {
		t.Errorf("got %+v", p)
	}
	if p := pods[1]; p.device != "/dev/b" || p.addr != "" {
		t.Errorf("got %+v", p)
	}
	if p := pods[2]; p.addr != "10.0.0.6:8080" || p.device != "" {
		t.Errorf("got %+v", p)
	}
}
