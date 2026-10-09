package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/benchpodconfig"
)

// TestMain keeps every test off the real embeddedci.com session and config: the CLI's files go
// to a temporary XDG_CONFIG_HOME and no API key or server override leaks in from the shell.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "benchpod-cli-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	for _, k := range []string{"BENCHPOD_API_KEY", "BENCHPOD_API_BASE", "BENCHPOD_CONNECTION", "BENCHPOD_CONFIG_FILE"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeCloud is embeddedci.com for one pod, "bench-1" (id "id-1"): the device list, the command
// route (answered by reply) and the wiring routes.
type fakeCloud struct {
	mu       sync.Mutex
	auth     []string
	commands []map[string]any
	reply    func(cmd map[string]any) (status int, body string)
	profile  string // the effective profile; "" = no profile route (404)
	stored   string // GET .../wiring
}

func (f *fakeCloud) start(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/benchpod/devices":
			fmt.Fprint(w, `{"devices":[{"id":"id-1","name":"bench-1"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/benchpod/devices/id-1/command":
			var body struct {
				Command map[string]any `json:"command"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.commands = append(f.commands, body.Command)
			status, out := f.reply(body.Command)
			w.WriteHeader(status)
			fmt.Fprint(w, out)
		case r.Method == http.MethodGet && r.URL.Path == "/api/benchpod/devices/id-1/wiring/profile" && f.profile != "":
			fmt.Fprintf(w, `{"version":1,"profile":%s}`, f.profile)
		case r.Method == http.MethodGet && r.URL.Path == "/api/benchpod/devices/id-1/wiring" && f.stored != "":
			fmt.Fprint(w, f.stored)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	t.Setenv("BENCHPOD_API_BASE", ts.URL)
	t.Setenv("BENCHPOD_API_KEY", "eci_test")
	return ts.URL
}

func (f *fakeCloud) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.commands {
		b, _ := json.Marshal(c)
		out = append(out, string(b))
	}
	return out
}

// podReplies answers the command route like a pod with these replies per cmd.
func podReplies(replies map[string]string) func(map[string]any) (int, string) {
	return func(cmd map[string]any) (int, string) {
		name, _ := cmd["cmd"].(string)
		if r, ok := replies[name]; ok {
			return http.StatusOK, `{"status":"ok","data":` + r + `}`
		}
		return http.StatusOK, `{"status":"error","error":"unknown cmd"}`
	}
}

// runOut runs the CLI with args, sending command output to a file, and returns the exit code
// and that output.
func runOut(t *testing.T, args ...string) (int, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.txt")
	code := run(append(args, "--output-filename", out))
	b, _ := os.ReadFile(out)
	return code, string(b)
}

func TestCloudPingStatusAndLAVoltage(t *testing.T) {
	old := latestFirmwareRelease
	latestFirmwareRelease = func() string { return "" }
	t.Cleanup(func() { latestFirmwareRelease = old })

	f := &fakeCloud{reply: podReplies(map[string]string{
		"ping":          `"pong"`,
		"status":        `{"version":"3.8.0","ip":"192.168.1.220","net":"eth","la_vccio_mv":3300,"gateware":48}`,
		"cloud_status":  `{"state":"connected","configured":true,"device_id":"id-1"}`,
		"target_status": `{"efuse1":{"enabled":1,"fault":0},"efuse2":{"enabled":0,"fault":0}}`,
		"la_voltage":    `{"mv":3300,"readback_mv":3300}`,
		"la":            `{"pullups":0}`,
	})}
	f.start(t)
	conn := "embeddedci:bench-1"

	if code, out := runOut(t, "ping", "--connection", conn); code != exitOK || strings.TrimSpace(out) != "pong" {
		t.Fatalf("ping: exit %d, %q", code, out)
	}
	code, out := runOut(t, "status", "--connection", conn)
	if code != exitOK {
		t.Fatalf("status: exit %d", code)
	}
	for _, want := range []string{"BenchPod bench-1 on embeddedci.com", "3.8.0", "3.3 V", "internal 5 V on", "on your account as bench-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if code, out := runOut(t, "la", "voltage", "--connection", conn); code != exitOK || !strings.Contains(out, "LA voltage of bench-1 on embeddedci.com: 3.3 V") {
		t.Errorf("la voltage: exit %d, %q", code, out)
	}
	if code, _ := runOut(t, "la", "status", "--connection", conn); code != exitOK {
		t.Errorf("la status: exit %d", code)
	}
	// The saved default and BENCHPOD_CONNECTION take the cloud form too.
	t.Setenv("BENCHPOD_CONNECTION", conn)
	if code, out := runOut(t, "ping"); code != exitOK || strings.TrimSpace(out) != "pong" {
		t.Errorf("ping via BENCHPOD_CONNECTION: exit %d, %q", code, out)
	}
	for _, a := range f.auth {
		if a != "ApiKey eci_test" {
			t.Errorf("auth header %q, want the API key", a)
		}
	}
	sent := strings.Join(f.sent(), " ")
	for _, want := range []string{`{"cmd":"ping"}`, `{"cmd":"status"}`, `{"cmd":"cloud_status"}`, `{"cmd":"la_voltage"}`} {
		if !strings.Contains(sent, want) {
			t.Errorf("commands sent %s, want %s", sent, want)
		}
	}
}

func TestSetConnectionSavesTheCloudForm(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	if code := run([]string{"set-connection", "embeddedci:bench-1", "--config-file", cfg}); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	c, err := benchpodconfig.Load(cfg)
	if err != nil || c.Connection != "embeddedci:bench-1" || c.Version != benchpodconfig.Version {
		t.Fatalf("saved %+v, %v", c, err)
	}
}

func TestCloudExitCodes(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   int
	}{
		{409, `{"error":"lease held","code":"lease_held"}`, exitBusy},
		{503, `{"error":"offline","code":"device_offline"}`, exitUnreachable},
		{403, `{"error":"no","code":"forbidden_tier"}`, exitRefused},
		{200, `{"status":"error","error":"locked: nope"}`, exitRefused},
		{504, `{"error":"timeout","code":"device_timeout"}`, exitError},
	} {
		f := &fakeCloud{reply: func(map[string]any) (int, string) { return c.status, c.body }}
		f.start(t)
		if code, _ := runOut(t, "ping", "--connection", "embeddedci:bench-1"); code != c.want {
			t.Errorf("%d %s: exit %d, want %d", c.status, c.body, code, c.want)
		}
	}
}

func TestCloudNeedsASignIn(t *testing.T) {
	t.Setenv("BENCHPOD_API_KEY", "")
	_, _, _, err := (&globalFlags{connection: "embeddedci:bench-1"}).podClient("ping", 0)
	if err == nil || !strings.Contains(err.Error(), "benchpod login") || !strings.Contains(err.Error(), "BENCHPOD_API_KEY") {
		t.Fatalf("err = %v", err)
	}
}

// Commands that need the pod's own port, a stream or USB refuse a cloud connection up front, and
// say what to use instead. None of them may reach the server.
func TestCloudRefusals(t *testing.T) {
	f := &fakeCloud{reply: func(map[string]any) (int, string) { return 500, "{}" }}
	f.start(t)
	conn := "embeddedci:bench-1"
	for _, args := range [][]string{
		{"capture"}, {"stream"}, {"test"}, {"measure", "--waveform", "sine"},
		{"flash", "--swclk", "1", "--swdio", "2", "--target", "target/x.cfg"},
		{"spi-flash", "id", "--sck", "1", "--mosi", "2", "--miso", "3", "--cs", "4"},
		{"register"},
		{"cloud", "ca", "set", "testdata-missing.pem"},
	} {
		g := &globalFlags{connection: conn}
		var err error
		switch args[0] {
		case "flash":
			err = runFlash(g, &flashFlags{swclk: "1", swdio: "2", target: "target/x.cfg"})
		case "cloud":
			err = runCloudCA(g, "set", "", nil, &bytes.Buffer{})
		default:
			code := run(append(args, "--connection", conn))
			if code == exitOK {
				t.Errorf("%v: exit 0 over embeddedci.com", args)
			}
			_, _, _, err = g.networkClient(args[0], 0)
		}
		if err == nil || !strings.Contains(err.Error(), "embeddedci.com") {
			t.Errorf("%v: err = %v, want a refusal naming embeddedci.com", args, err)
		}
	}
	for _, cmd := range []string{"set-wifi", "show-network", "clear-wifi", "install-blobs", "bootsel", "dfu", "flash-self"} {
		_, err := (&globalFlags{connection: conn}).usbDevice(cmd)
		if err == nil || !strings.Contains(err.Error(), "USB console only") || !strings.Contains(err.Error(), cmd) {
			t.Errorf("%s: err = %v", cmd, err)
		}
	}
	// A saved embeddedci: default does not stop the USB commands: they auto-detect, as with a
	// saved network address.
	cfg := filepath.Join(t.TempDir(), "config.json")
	_ = benchpodconfig.Save(cfg, &benchpodconfig.Config{Connection: conn})
	if dev, err := (&globalFlags{configFile: cfg}).usbDevice("set-wifi"); err != nil || dev != "" {
		t.Errorf("saved cloud default: %q, %v", dev, err)
	}
	if err := runDiscover(&globalFlags{connection: conn}, discoverOpts{usb: true}); err == nil || !strings.Contains(err.Error(), "benchpod status --connection embeddedci:bench-1") {
		t.Errorf("discover: %v", err)
	}
	if len(f.sent()) != 0 {
		t.Errorf("refused commands reached the server: %v", f.sent())
	}
}

func TestCloudProxyAndCA(t *testing.T) {
	f := &fakeCloud{reply: podReplies(map[string]string{
		"cloud_proxy": `{"host":"proxy.corp","port":3128,"auth":true}`,
		"cloud_ca":    `{"present":false,"certs":[]}`,
	})}
	f.start(t)
	g := &globalFlags{connection: "embeddedci:bench-1"}
	var out, warn bytes.Buffer
	if err := runCloudProxy(g, "set", "proxy.corp:3128", "alice", "s3cret", &out, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "HTTP proxy of bench-1 on embeddedci.com is now proxy.corp:3128 (with a user and password)") {
		t.Errorf("out = %q", out.String())
	}
	if strings.Contains(warn.String(), "unencrypted") {
		t.Errorf("warned about a plain LAN password over HTTPS: %q", warn.String())
	}
	out.Reset()
	if err := runCloudCA(g, "clear", "", &out, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Company CA of bench-1 on embeddedci.com is now none") {
		t.Errorf("out = %q", out.String())
	}
	sent := strings.Join(f.sent(), " ")
	if !strings.Contains(sent, `"set":"proxy.corp:3128"`) || !strings.Contains(sent, `{"clear":true,"cmd":"cloud_ca"}`) {
		t.Errorf("sent %s", sent)
	}
}

// ── wiring profile ──────────────────────────────────────────────────────────

func TestWiringProfileOverCloud(t *testing.T) {
	f := &fakeCloud{profile: `{"swd_swclk":11,"swd_swdio":12,"swd_nreset":true,"swd_target":"target/stm32f4x.cfg","spi_sclk":3,"spi_mosi":4,"spi_miso":5,"spi_cs":null}`}
	f.start(t)
	var warn bytes.Buffer
	w := loadPodWiring(&globalFlags{connection: "embeddedci:bench-1"}, &warn)
	if w == nil || *w.SwdSwclk != 11 || *w.SwdSwdio != 12 || !*w.SwdNreset || w.SwdTarget != "target/stm32f4x.cfg" || w.SpiCs != nil {
		t.Fatalf("wiring %+v (warn %q)", w, warn.String())
	}

	// A server without the profile route: the stored map.
	f2 := &fakeCloud{stored: `{"swd_swclk":7}`}
	f2.start(t)
	w = loadPodWiring(&globalFlags{connection: "embeddedci:bench-1"}, &warn)
	if w == nil || *w.SwdSwclk != 7 || w.SwdSwdio != nil {
		t.Fatalf("stored fallback %+v", w)
	}
	if warn.Len() != 0 {
		t.Errorf("warned: %q", warn.String())
	}
}

// Over the network the pod's cloud_status names its id, which must be on the account.
func TestWiringProfileOverNetwork(t *testing.T) {
	f := &fakeCloud{profile: `{"swd_swclk":11,"swd_swdio":12,"swd_nreset":false}`}
	f.start(t)
	for id, want := range map[string]bool{"id-1": true, "id-other": false} {
		addr := fakeCloudStatusPod(t, id)
		var warn bytes.Buffer
		w := loadPodWiring(&globalFlags{connection: addr}, &warn)
		if (w != nil) != want {
			t.Errorf("device %s: wiring %+v, want found=%v", id, w, want)
		}
		if warn.Len() != 0 {
			t.Errorf("device %s: warned %q", id, warn.String())
		}
	}
	// Not signed in: no lookup, no warning.
	t.Setenv("BENCHPOD_API_KEY", "")
	var warn bytes.Buffer
	if w := loadPodWiring(&globalFlags{connection: fakeCloudStatusPod(t, "id-1")}, &warn); w != nil || warn.Len() != 0 {
		t.Errorf("signed out: %+v, %q", w, warn.String())
	}
	// USB: never.
	if w := loadPodWiring(&globalFlags{connection: "usb"}, &warn); w != nil {
		t.Errorf("usb: %+v", w)
	}
}

func TestWiringProfileFailureWarnsOnce(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	t.Setenv("BENCHPOD_API_BASE", ts.URL)
	t.Setenv("BENCHPOD_API_KEY", "eci_test")
	var warn bytes.Buffer
	if w := loadPodWiring(&globalFlags{connection: "embeddedci:bench-1"}, &warn); w != nil {
		t.Fatalf("wiring %+v", w)
	}
	if n := strings.Count(warn.String(), "\n"); n != 1 || !strings.Contains(warn.String(), "using the flags only") {
		t.Errorf("warn = %q", warn.String())
	}
}

func TestFlashTakesSWDFromWiringFlagsWin(t *testing.T) {
	old := loadPodWiring
	t.Cleanup(func() { loadPodWiring = old })
	pin := func(n int) *int { return &n }
	yes := true
	loadPodWiring = func(g *globalFlags, _ io.Writer) *podWiring {
		return &podWiring{SwdSwclk: pin(11), SwdSwdio: pin(12), SwdNreset: &yes, SwdTarget: "target/stm32f4x.cfg"}
	}

	f := &flashFlags{}
	f.fillFromWiring(&globalFlags{})
	if f.swclk != "11" || f.swdio != "12" || !f.nreset || f.target != "target/stm32f4x.cfg" {
		t.Errorf("from wiring: %+v", f)
	}
	f = &flashFlags{swclk: "1", nreset: false, nresetSet: true, target: "target/stm32f1x.cfg"}
	f.fillFromWiring(&globalFlags{})
	if f.swclk != "1" || f.swdio != "12" || f.nreset || f.target != "target/stm32f1x.cfg" {
		t.Errorf("flags must win: %+v", f)
	}

	s := &spiFlags{mosi: "9"}
	loadPodWiring = func(g *globalFlags, _ io.Writer) *podWiring {
		return &podWiring{SpiSclk: pin(3), SpiMosi: pin(4), SpiMiso: pin(5)}
	}
	s.fillFromWiring(&globalFlags{connection: "192.168.1.5"})
	if s.sck != "3" || s.mosi != "9" || s.miso != "5" || s.cs != "" {
		t.Errorf("spi: %+v", s)
	}
}

// fakeCloudStatusPod is a LAN pod that answers cloud_status with this device id.
func fakeCloudStatusPod(t *testing.T, id string) string {
	t.Helper()
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
			_, _ = bufio.NewReader(conn).ReadString('\n')
			fmt.Fprintf(conn, `{"status":"ok","data":{"state":"connected","configured":true,"device_id":%q}}`+"\n", id)
			conn.Close()
		}
	}()
	return ln.Addr().String()
}
