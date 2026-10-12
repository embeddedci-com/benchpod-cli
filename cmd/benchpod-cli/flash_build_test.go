package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeBuilds is embeddedci.com with one pod ("bench-1", id "id-1") and one uploaded build.
type fakeBuilds struct {
	mu      sync.Mutex
	queries []string
	flashes []map[string]any
	reply   string
}

func (f *fakeBuilds) start(t *testing.T) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/benchpod/devices":
			fmt.Fprint(w, `{"devices":[{"id":"id-1","name":"bench-1"}]}`)
		case r.URL.Path == "/api/benchpod/builds/artifact":
			f.queries = append(f.queries, r.URL.RawQuery)
			fmt.Fprint(w, `{"artifact":{"job_id":"job-1","pack_id":"pack-1","name":"out/fw.elf","repo":"acme/fw","ref":"main","sha":"abc","wiring_defaults":{"target":"target/stm32f4x.cfg"}}}`)
		case r.URL.Path == "/api/benchpod/builds/job-1/packs/pack-1/artifacts/out/fw.elf":
			fmt.Fprint(w, "ELFDATA")
		case r.Method == http.MethodPost && r.URL.Path == "/api/benchpod/devices/id-1/flash":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.flashes = append(f.flashes, body)
			fmt.Fprint(w, f.reply)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	t.Setenv("BENCHPOD_API_BASE", ts.URL)
	t.Setenv("BENCHPOD_API_KEY", "eci_test")
}

func TestFlashBuildOverCloudRunsOnTheServer(t *testing.T) {
	f := &fakeBuilds{reply: `{"ok":true,"output":"** Verified OK **"}`}
	f.start(t)
	g := &globalFlags{connection: "embeddedci:bench-1"}
	err := runFlash(g, &flashFlags{swclk: "11", swdio: "12", nreset: true, nresetSet: true,
		repo: "acme/fw", ref: "main", targetPower: 1, noConnectUnderReset: true})
	if err != nil {
		t.Fatalf("runFlash: %v", err)
	}
	if len(f.queries) != 1 || f.queries[0] != "ref=main&repo=acme%2Ffw" {
		t.Fatalf("lookup query = %v", f.queries)
	}
	if len(f.flashes) != 1 {
		t.Fatalf("flashes = %v", f.flashes)
	}
	got, _ := json.Marshal(f.flashes[0])
	want := `{"artifact_name":"out/fw.elf","connect_under_reset":false,"efuse":1,"job_id":"job-1","nreset":true,"pack_id":"pack-1","swclk":11,"swdio":12,"target":"target/stm32f4x.cfg"}`
	if string(got) != want {
		t.Fatalf("flash body\n got %s\nwant %s", got, want)
	}
}

func TestFlashBuildServerFailureIsAnError(t *testing.T) {
	f := &fakeBuilds{reply: `{"ok":false,"output":"Error: no device found","error":"openocd exited 1","target_unreachable":true}`}
	f.start(t)
	err := runFlash(&globalFlags{connection: "embeddedci:bench-1"}, &flashFlags{swclk: "1", swdio: "2", build: "job-1"})
	if err == nil || !strings.Contains(err.Error(), "openocd exited 1") {
		t.Fatalf("err = %v", err)
	}
}

func TestFlashBuildDownloadsForALocalFlash(t *testing.T) {
	f := &fakeBuilds{}
	f.start(t)
	fl := &flashFlags{build: "latest", artifact: "fw.elf"}
	art, cleanup, err := fl.resolveBuild(&globalFlags{}, true)
	if err != nil {
		t.Fatalf("resolveBuild: %v", err)
	}
	b, err := os.ReadFile(fl.file)
	if err != nil || string(b) != "ELFDATA" || !strings.HasSuffix(fl.file, "/fw.elf") {
		t.Fatalf("downloaded %q to %s: %v", b, fl.file, err)
	}
	if art.Target() != "target/stm32f4x.cfg" || f.queries[0] != "artifact=fw.elf&build=latest" {
		t.Fatalf("target %q, query %v", art.Target(), f.queries)
	}
	cleanup()
	if _, err := os.Stat(fl.file); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed: %v", err)
	}
}

func TestFlashBuildRefusals(t *testing.T) {
	cloud := &globalFlags{connection: "embeddedci:bench-1"}
	lan := &globalFlags{connection: "192.0.2.1"}
	cases := []struct {
		name string
		g    *globalFlags
		f    *flashFlags
		want string
	}{
		{"file and build", lan, &flashFlags{file: "a.elf", build: "x"}, "either --file or an uploaded build"},
		{"server-side without build", lan, &flashFlags{serverSideSet: true, serverSide: true}, "--server-side only applies"},
		{"cloud without build", cloud, &flashFlags{file: "a.elf"}, "flashes a build uploaded there"},
		{"cloud local flash", cloud, &flashFlags{build: "x", serverSideSet: true}, "--server-side=false needs a direct link"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runFlash(tc.g, tc.f)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFlashBuildServerRefusesLocalOnlyFlags(t *testing.T) {
	f := &fakeBuilds{reply: `{"ok":true}`}
	f.start(t)
	err := runFlash(&globalFlags{connection: "embeddedci:bench-1"},
		&flashFlags{swclk: "1", swdio: "2", build: "job-1", noVerify: true, extraArgs: []string{"-d3"}})
	if err == nil || !strings.Contains(err.Error(), "--no-verify, --openocd-arg only apply to the local OpenOCD") {
		t.Fatalf("err = %v", err)
	}
	if len(f.flashes) != 0 {
		t.Fatal("nothing should be flashed")
	}
}
