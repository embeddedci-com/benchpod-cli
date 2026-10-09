package cloudpod

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// fakeServer answers the device list and the command route like embeddedci-server.
type fakeServer struct {
	auth    string
	command map[string]any
	timeout float64
	reply   func(w http.ResponseWriter)
}

func (f *fakeServer) start(t *testing.T) *serverapi.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.auth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/benchpod/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{
				{"id": "id-a", "name": "bench-a"}, {"id": "id-b", "name": "bench-b"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/benchpod/devices/id-b/command":
			var body struct {
				Command   map[string]any `json:"command"`
				TimeoutMs float64        `json:"timeout_ms"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.command, f.timeout = body.Command, body.TimeoutMs
			f.reply(w)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return serverapi.New(ts.URL)
}

func jsonError(w http.ResponseWriter, status int, msg, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

func TestResolveAndCommand(t *testing.T) {
	f := &fakeServer{reply: func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"request_id":"r","device_id":"id-b","status":"ok","data":{"version":"3.8.0"}}`))
	}}
	api := f.start(t)
	c, err := Resolve(context.Background(), api, "ApiKey eci_test", "bench-b")
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID != "id-b" || c.Label() != "bench-b on embeddedci.com" {
		t.Fatalf("resolved %+v", c)
	}
	if f.auth != "ApiKey eci_test" {
		t.Errorf("auth = %q, want the API key scheme", f.auth)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	data, err := c.Command(ctx, map[string]any{"cmd": "status"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"version":"3.8.0"}` || f.command["cmd"] != "status" {
		t.Errorf("data %s, sent %v", data, f.command)
	}
	if f.timeout < 15000 || f.timeout > 20000 {
		t.Errorf("timeout_ms = %v, want just under the context's 20 s", f.timeout)
	}

	// A bearer token (the `benchpod login` session) goes as Bearer.
	c.Credential = "jwt-1"
	if _, err := c.Command(ctx, map[string]any{"cmd": "ping"}); err != nil {
		t.Fatal(err)
	}
	if f.auth != "Bearer jwt-1" {
		t.Errorf("auth = %q", f.auth)
	}
}

func TestResolveUnknownName(t *testing.T) {
	api := (&fakeServer{}).start(t)
	_, err := Resolve(context.Background(), api, "tok", "nope")
	if err == nil || !strings.Contains(err.Error(), `no pod named "nope"`) || !strings.Contains(err.Error(), "bench-a, bench-b") {
		t.Fatalf("err = %v", err)
	}
	// The id works too.
	if c, err := Resolve(context.Background(), api, "tok", "id-a"); err != nil || c.Name != "bench-a" {
		t.Fatalf("by id: %+v %v", c, err)
	}
}

func TestPodRefusalIsAPodError(t *testing.T) {
	f := &fakeServer{reply: func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"error","error":"la voltage not set; set it first"}`))
	}}
	c := &Client{API: f.start(t), Credential: "tok", DeviceID: "id-b", Name: "bench-b"}
	_, err := c.Command(context.Background(), map[string]any{"cmd": "la"})
	pe, ok := tcpclient.AsPodError(err)
	if !ok || pe.Reason != "la voltage not set; set it first" || pe.Cmd != "" {
		t.Fatalf("err = %#v", err)
	}
}

func TestTransportFailures(t *testing.T) {
	for _, c := range []struct {
		status      int
		code        string
		want        string
		busy        bool
		unreachable bool
		refused     bool
	}{
		{409, "lease_held", "holds its lease", true, false, false},
		{409, "device_busy", "busy with another operation", true, false, false},
		{503, "device_offline", "is offline", false, true, false},
		{503, "device_reconnecting", "try again in a few seconds", false, true, false},
		{504, "device_timeout", "did not answer", false, false, false},
		{400, "not_capable", "firmware is too old", false, false, false},
		{403, "forbidden_tier", "owner or admin", false, false, true},
		{401, "", "benchpod login", false, false, false},
	} {
		f := &fakeServer{reply: func(w http.ResponseWriter) { jsonError(w, c.status, "server says no", c.code) }}
		cl := &Client{API: f.start(t), Credential: "tok", DeviceID: "id-b", Name: "bench-b"}
		_, err := cl.Command(context.Background(), map[string]any{"cmd": "status"})
		var ce *Error
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%d %s: err = %v, want %q", c.status, c.code, err, c.want)
			continue
		}
		pe, isPE := tcpclient.AsPodError(err)
		if got := isPE && pe.Kind() == tcpclient.Busy; got != c.busy {
			t.Errorf("%s: busy = %v", c.code, got)
		}
		if got := isPE && pe.Kind() == tcpclient.Forbidden; got != c.refused {
			t.Errorf("%s: forbidden = %v", c.code, got)
		}
		if got := errors.Is(err, tcpclient.ErrUnreachable); got != c.unreachable {
			t.Errorf("%s: unreachable = %v", c.code, got)
		}
	}
}
