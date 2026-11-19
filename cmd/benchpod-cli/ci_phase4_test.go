package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// A compare reply where boot's mean_ma drifted 30 % against the baseline (allowance 20 %).
const driftCompare = `{"found":true,"a":"r-base","b":"r-1","comparable":true,"reasons":[],"flags":[],"inputs_changed":[],
 "cases":{"newly_failing":[],"fixed":[],"still_failing":[],"new":[],"removed":[],"newly_skipped":[]},
 "measurements":[{"case":"boot","name":"mean_ma","a":10,"b":13,"unit":"mA","change":0.3,"status":"drift"}],
 "slower":[],"calls_slower":[],"a_run":{"id":"r-base"}}`

func TestCIBaselineGating(t *testing.T) {
	cases := []struct {
		name, extra, expect string
		code                int
	}{
		{"fail_on_drift", "", "", 3},
		{"gate drift", "gate: drift\n", "", 3},
		{"not gated", "", "", 0},
		{"fail_on_drift false beats gate", "gate: drift\n", "", 0},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs, _ := startRunsServer(t)
			fs.compareBody = driftCompare
			yaml := "version: 1\nsuite: s\ndut: d\nbaseline: main\n" + c.extra + "steps:\n  - id: boot\n    measure_power: {duration: 10ms}\n"
			switch i {
			case 0:
				yaml += "    expect: {mean_ma: {drift: 20%, fail_on_drift: true}}\n"
			case 3:
				yaml += "    expect: {mean_ma: {drift: 20%, fail_on_drift: false}}\n"
			}
			f := newCIFixture(t, yaml)
			f.withUpload = true
			summary := filepath.Join(t.TempDir(), "summary.md")
			t.Setenv("GITHUB_STEP_SUMMARY", summary)
			exit, run := f.run()
			if exit.code != c.code {
				t.Fatalf("exit %d, want %d\n%s", exit.code, c.code, f.stdout.String())
			}
			if run.Results.Outcome != ci.Passed || fs.compared != "/api/runs/r-1/compare?with=baseline%3Amain" {
				t.Errorf("outcome %s compared %s", run.Results.Outcome, fs.compared)
			}
			out := f.stdout.String()
			if !strings.Contains(out, "Baseline main (run r-base): 0 newly failing, 0 fixed, 1 drifted") {
				t.Errorf("headline:\n%s", out)
			}
			if c.code == 3 && !strings.Contains(out, "boot mean_ma drifted +30.0%") {
				t.Errorf("gated line:\n%s", out)
			}
			md, _ := os.ReadFile(summary)
			text := string(md)
			for _, want := range []string{"### benchpod ci: s on d: passed", "Run: https://example.test/run/r-1", "drift vs main +30.0%", "Baseline main (run r-base)"} {
				if !strings.Contains(text, want) {
					t.Errorf("summary lacks %q:\n%s", want, text)
				}
			}
			if strings.Contains(text, "(fails the run)") != (c.code == 3) {
				t.Errorf("summary gating mark:\n%s", text)
			}
		})
	}
}

func TestCIBaselineNotUploadedOrMissing(t *testing.T) {
	fs, _ := startRunsServer(t)
	fs.compareBody = `{"found":false,"a":null,"b":"r-1","message":"no earlier valid run of s on d to compare with"}`
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nbaseline: main\ngate: drift\nsteps:\n  - wait: 1ms\n")
	f.withUpload = true
	if exit, _ := f.run(); exit.code != 0 || !strings.Contains(f.stdout.String(), "Baseline main: no earlier valid run") {
		t.Errorf("missing baseline: exit %d\n%s", exit.code, f.stdout.String())
	}
	f = newCIFixture(t, "version: 1\nsuite: s\ndut: d\nbaseline: main\nupload: false\nsteps:\n  - wait: 1ms\n")
	f.withUpload = true
	if exit, _ := f.run(); exit.code != 0 || !strings.Contains(f.stdout.String(), "not compared: the run was not uploaded") {
		t.Errorf("not uploaded: exit %d\n%s", exit.code, f.stdout.String())
	}
	// A failing run keeps exit 2 whatever the baseline says.
	fs.compareBody = driftCompare
	f = newCIFixture(t, "version: 1\nsuite: s\ndut: d\nbaseline: main\ngate: drift\nsteps:\n  - run: \"false\"\n")
	f.withUpload = true
	if exit, _ := f.run(); exit.code != ci.ExitFailed {
		t.Errorf("failing run: exit %d", exit.code)
	}
}

// fakeGitHub is the Actions OIDC endpoint and embeddedci.com's exchange and cloud routes.
type fakeGitHub struct {
	mu        sync.Mutex
	audience  string
	reqAuth   string
	exchanged string
	cmdPath   string
	cmdAuth   string
	leaseAuth string
	runAuth   string
}

func TestGitHubActionsSignIn(t *testing.T) {
	resetGitHubToken()
	t.Cleanup(resetGitHubToken)
	g := &fakeGitHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/actions/token":
			g.audience, g.reqAuth = r.URL.Query().Get("audience"), r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"value":"oidc-jwt"}`))
		case "/api/github/oidc/token":
			var req map[string]string
			_ = json.Unmarshal(body, &req)
			g.exchanged = req["token"]
			_, _ = w.Write([]byte(`{"access_token":"gha-token","token_type":"Bearer","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","devices":["benchpod-a1b2c3"]}`))
		case "/api/cloud/devices/command":
			g.cmdPath, g.cmdAuth = r.URL.RequestURI(), r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"status":"ok","data":{"version":"3.10.13"}}`))
		case "/api/cloud/devices/lease":
			g.leaseAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"lease_id":"L-1"}`))
		case "/api/runs":
			g.runAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"runs":[],"org_pods":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found: ` + r.URL.Path + `"}`))
		}
	}))
	defer srv.Close()
	noSignIn(t)
	t.Setenv("BENCHPOD_API_BASE", srv.URL)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"/actions/token?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runner-secret")
	ctx := context.Background()

	cp, err := resolveCloudPod(ctx, "benchpod-a1b2c3")
	if err != nil {
		t.Fatal(err)
	}
	if !cp.ByName || cp.Credential != "gha-token" {
		t.Fatalf("cloud pod %+v", cp)
	}
	data, err := cp.Command(ctx, map[string]any{"cmd": "status"})
	if err != nil || !strings.Contains(string(data), "3.10.13") {
		t.Fatalf("command %s %v", data, err)
	}
	lease, err := acquireCloudLease(ctx, cp.API, cp.Credential, cp.Name, "L-1", 0, time.Hour, io.Discard)
	if err != nil || !lease.Supported() {
		t.Fatalf("lease %v", err)
	}
	if _, _, err := runsAPIList(ctx); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.audience != "https://embeddedci.com" || g.reqAuth != "Bearer runner-secret" || g.exchanged != "oidc-jwt" {
		t.Errorf("oidc: audience %q auth %q exchanged %q", g.audience, g.reqAuth, g.exchanged)
	}
	if g.cmdPath != "/api/cloud/devices/command?device=benchpod-a1b2c3" || g.cmdAuth != "Bearer gha-token" {
		t.Errorf("command %s %s", g.cmdPath, g.cmdAuth)
	}
	if g.leaseAuth != "Bearer gha-token" || g.runAuth != "Bearer gha-token" {
		t.Errorf("lease auth %q runs auth %q", g.leaseAuth, g.runAuth)
	}
}

func runsAPIList(ctx context.Context) (string, string, error) {
	api, cred, err := runsAPI(ctx)
	if err != nil {
		return "", "", err
	}
	_, err = api.ListRuns(ctx, cred, serverapi.RunFilter{})
	return "", "", err
}

func TestGitHubActionsSignInNeedsPermission(t *testing.T) {
	resetGitHubToken()
	noSignIn(t)
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "")
	if _, err := cloudCredential(context.Background(), newServerAPI("http://127.0.0.1:1")); err != errNoCloudCredential {
		t.Errorf("without OIDC: %v", err)
	}
}

func TestCIGitHubInputs(t *testing.T) {
	for k, v := range map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2", "GITHUB_WORKFLOW": "hil",
		"GITHUB_ACTOR": "octocat", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "acme/fw"} {
		t.Setenv(k, v)
	}
	f := newCIFixture(t, "version: 1\nsuite: s\ndut: d\nsteps:\n  - wait: 1ms\n")
	_, run := f.run()
	g := run.Inputs.GitHub
	if run.Trigger != "github" || g.RunID != "123" || g.RunAttempt != "2" || g.Workflow != "hil" || g.Actor != "octocat" ||
		g.RunURL != "https://github.com/acme/fw/actions/runs/123" {
		t.Errorf("trigger %s github %+v", run.Trigger, g)
	}
}
