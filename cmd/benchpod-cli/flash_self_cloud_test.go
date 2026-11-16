package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// otaServer is embeddedci.com for flash-self through the cloud: the pod's status over the command
// route, and an OTA job that walks through phases on each GET.
type otaServer struct {
	t      *testing.T
	mu     sync.Mutex
	refuse func(w http.ResponseWriter) // answers POST .../ota instead of starting a job
	phases []map[string]any            // what successive GET .../ota return (the last repeats)

	query, sig string
	body       []byte
	gets       int
	aborts     int
}

func (o *otaServer) start() *serverapi.Client {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /api/benchpod/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"id": "id-1", "name": "benchpod-7a32b5"}}})
		case "POST /api/benchpod/devices/id-1/command":
			_, _ = w.Write([]byte(`{"status":"ok","data":{"version":"3.10.1","flash_kb":1024}}`))
		case "GET /api/benchpod/firmware/latest":
			_, _ = w.Write([]byte(`{"release":{"version":"3.10.2","tag":"v3.10.2","installable":true}}`))
		case "POST /api/benchpod/devices/id-1/ota":
			if o.refuse != nil {
				o.refuse(w)
				return
			}
			o.query, o.sig = r.URL.RawQuery, r.Header.Get(serverapi.OTASignatureHeader)
			o.body, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"job_id":"j1","phase":"streaming","live":true}`))
		case "GET /api/benchpod/devices/id-1/ota":
			i := min(o.gets, len(o.phases)-1)
			o.gets++
			_ = json.NewEncoder(w).Encode(o.phases[i])
		case "POST /api/benchpod/devices/id-1/ota/abort":
			o.aborts++
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	o.t.Cleanup(ts.Close)
	api := serverapi.New(ts.URL)
	orig, poll := resolveCloudPodForFlash, cloudOTAPoll
	resolveCloudPodForFlash = func(ctx context.Context, name string) (*cloudpod.Client, error) {
		return cloudpod.Resolve(ctx, api, "tok", name)
	}
	cloudOTAPoll = time.Millisecond
	o.t.Cleanup(func() { resolveCloudPodForFlash, cloudOTAPoll = orig, poll })
	return api
}

var cloudFlashGlobals = &globalFlags{connection: "embeddedci:benchpod-7a32b5", noFirmwareReport: true}

func TestFlashSelfCloudUploadsImageAndSig(t *testing.T) {
	images, sigs := sigVectors(t)
	fw := filepath.Join(t.TempDir(), "bench_pod_stm32.bin")
	writeFile(t, fw, images["firmware"])
	writeFile(t, fw+".sig", sigs["good firmware"])
	o := &otaServer{t: t, phases: []map[string]any{
		{"phase": "streaming", "live": true, "percent": 40},
		{"phase": "committing", "live": true, "percent": 100},
		{"phase": "done", "version_after": "3.10.2-dev",
			"signatures": []map[string]any{{"part": "firmware", "server": "ok", "key_id": "ed4242ead4ac6948", "device": "ok"}}},
	}}
	o.start()
	var err error
	out := captureStderr(t, func() {
		err = runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{network: true}, noFlagChanged, []string{fw}, "benchpod-7a32b5")
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if string(o.body) != string(images["firmware"]) || o.query != "" {
		t.Errorf("uploaded %d bytes with query %q", len(o.body), o.query)
	}
	if o.sig != base64.RawURLEncoding.EncodeToString(sigs["good firmware"]) {
		t.Errorf("signature header %q", o.sig)
	}
	for _, want := range []string{
		"benchpod-7a32b5 on embeddedci.com, running firmware 3.10.1",
		"streaming 40%", "committing (the pod writes its flash",
		"firmware signature: ok on embeddedci.com (key ed4242ead4ac6948), ok on the pod",
		"success, benchpod-7a32b5 runs firmware 3.10.2-dev",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFlashSelfCloudLatestRelease(t *testing.T) {
	o := &otaServer{t: t, phases: []map[string]any{{"phase": "done", "version_after": "3.10.2"}}}
	o.start()
	if err := runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{network: true}, noFlagChanged, nil, "benchpod-7a32b5"); err != nil {
		t.Fatal(err)
	}
	if o.query != "release=v3.10.2" || len(o.body) != 0 {
		t.Errorf("query %q, %d bytes", o.query, len(o.body))
	}
	// A pinned tag goes as is, with --dry-run as commit=false.
	o.phases = []map[string]any{{"phase": "verified"}}
	if err := runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{network: true, firmwareVer: "v3.9.0", dryRun: true},
		noFlagChanged, nil, "benchpod-7a32b5"); err != nil {
		t.Fatal(err)
	}
	if o.query != "commit=false&release=v3.9.0" {
		t.Errorf("query %q", o.query)
	}
	if o.aborts != 1 {
		t.Errorf("the dry run's staged image was not dropped (%d aborts)", o.aborts)
	}
	err := runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{}, func(n string) bool { return n == "blobs-dir" }, nil, "benchpod-7a32b5")
	if err == nil || !strings.Contains(err.Error(), "--blobs-dir do not apply to a release") {
		t.Fatalf("err = %v", err)
	}
}

func TestFlashSelfCloudRefusals(t *testing.T) {
	fw, _ := fwImage(t, 4096, 1024)
	for _, tc := range []struct {
		status     int
		body, want string
	}{
		{http.StatusForbidden, `{"error":"forbidden","code":"forbidden_tier"}`,
			"needs an owner or admin of benchpod-7a32b5's organization"},
		{http.StatusForbidden, `{"error":"API key is missing the required scope: benchpod:control"}`,
			"the API key cannot do this"},
		{http.StatusConflict, `{"error":"held","code":"lease_held"}`, "another session (the web app, a CI job or the SDK) holds its lease"},
		{http.StatusConflict, `{"error":"offline","code":"device_offline"}`, "benchpod-7a32b5 is offline"},
		{http.StatusConflict, `{"error":"a firmware update is already running on this pod"}`, "a firmware update is already running"},
	} {
		o := &otaServer{t: t, refuse: func(w http.ResponseWriter) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}}
		o.start()
		var err error
		captureStderr(t, func() {
			err = runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{network: true}, noFlagChanged, []string{fw}, "benchpod-7a32b5")
		})
		if err == nil || !strings.Contains(err.Error(), "did not start the update") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v", tc.body, err)
		}
	}
}

func TestFlashSelfCloudFailedJob(t *testing.T) {
	fw, _ := fwImage(t, 4096, 1024)
	o := &otaServer{t: t, phases: []map[string]any{{"phase": "error", "error": "signature refused by the pod's policy"}}}
	o.start()
	var err error
	captureStderr(t, func() {
		err = runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{network: true}, noFlagChanged, []string{fw}, "benchpod-7a32b5")
	})
	if err == nil || !strings.Contains(err.Error(), "the update failed: signature refused by the pod's policy") {
		t.Fatalf("err = %v", err)
	}
}

func TestFlashSelfCloudTooBigAndDFUFlags(t *testing.T) {
	(&otaServer{t: t}).start()
	fw, _ := fwImage(t, 4096, 2048)
	err := runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{}, noFlagChanged, []string{fw}, "benchpod-7a32b5")
	if err == nil || !strings.Contains(err.Error(), "needs 2048 KB of flash and the pod has 1024 KB") {
		t.Fatalf("err = %v", err)
	}
	err = runFlashSelfCloud(cloudFlashGlobals, &flashSelfFlags{}, func(n string) bool { return n == "enter-dfu" }, nil, "x")
	if err == nil || !strings.Contains(err.Error(), "--enter-dfu is for USB DFU") {
		t.Fatalf("err = %v", err)
	}
}

func TestAbortCloudOTA(t *testing.T) {
	o := &otaServer{t: t}
	api := o.start()
	pod := &cloudpod.Client{API: api, Credential: "tok", DeviceID: "id-1", Name: "benchpod-7a32b5"}
	err := abortCloudOTA(pod, &serverapi.OTAJob{Phase: serverapi.OTAPhaseStreaming})
	if err == nil || !strings.Contains(err.Error(), "aborted and the pod keeps its firmware") || o.aborts != 1 {
		t.Fatalf("err = %v, aborts %d", err, o.aborts)
	}
	err = abortCloudOTA(pod, &serverapi.OTAJob{Phase: serverapi.OTAPhaseCommitting})
	if err == nil || !strings.Contains(err.Error(), "cannot be stopped") || o.aborts != 1 {
		t.Fatalf("committing: err = %v, aborts %d", err, o.aborts)
	}
}

func TestExplicitCloud(t *testing.T) {
	if name, ok := (&globalFlags{connection: "embeddedci:benchpod-7a32b5"}).explicitCloud(); !ok || name != "benchpod-7a32b5" {
		t.Errorf("%q %v", name, ok)
	}
	for _, c := range []string{"", "usb", "192.168.1.150:8080"} {
		if _, ok := (&globalFlags{connection: c}).explicitCloud(); ok {
			t.Errorf("%q counted as a cloud connection", c)
		}
	}
}
