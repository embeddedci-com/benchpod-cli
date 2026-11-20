package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// statusFormPod is a pod on TCP whose status has two forms, as the firmware's: the frozen caps
// list without "caps":false, features_rev and local_lease with it. featuresRev 0 is firmware that
// ignores the flag (always the caps list) and has no lease_acquire.
type statusFormPod struct {
	addr        string
	featuresRev int
	mu          sync.Mutex
	reqs        []map[string]any
}

func startStatusFormPod(t *testing.T, featuresRev int) *statusFormPod {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	p := &statusFormPod{addr: ln.Addr().String(), featuresRev: featuresRev}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					var req map[string]any
					_ = json.Unmarshal(sc.Bytes(), &req)
					p.mu.Lock()
					p.reqs = append(p.reqs, req)
					p.mu.Unlock()
					_, _ = conn.Write(append(p.reply(req), '\n'))
				}
			}()
		}
	}()
	return p
}

func (p *statusFormPod) reply(req map[string]any) []byte {
	ok := func(data any) []byte { b, _ := json.Marshal(map[string]any{"status": "ok", "data": data}); return b }
	switch req["cmd"] {
	case "status":
		st := map[string]any{"version": "4.0.0", "board_rev": "v3", "gateware": 58, "analog": true, "api": 2, "min_client_api": 2,
			"lease": map[string]any{"held": false, "holder": "", "left_s": 0}}
		if caps, set := req["caps"]; p.featuresRev > 0 && set && caps == false {
			st["features_rev"] = p.featuresRev
			st["fpga_image"] = "loop"
			st["hw"] = []string{"wifi"}
			st["local_lease"] = map[string]any{"held": false}
		} else {
			st["caps"] = []string{"signal", "gpio", "power", "swd"}
		}
		return ok(st)
	case "lease_acquire", "lease_renew":
		if p.featuresRev < 5 {
			b, _ := json.Marshal(map[string]any{"status": "error", "code": "BP0002", "args": map[string]any{"cmd": req["cmd"]}, "message": "unknown cmd"})
			return b
		}
		return ok(map[string]any{"lease": map[string]any{"id": req["id"], "left_s": 300, "source": "local"}})
	case "lease_release":
		return ok(map[string]any{"released": true})
	}
	return ok(map[string]any{})
}

func (p *statusFormPod) cmds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.reqs {
		out = append(out, r["cmd"].(string))
	}
	return out
}

func TestCIStatusReadsTheFeaturesForm(t *testing.T) {
	p := startStatusFormPod(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pod := &networkPod{client: &tcpclient.Client{Addr: p.addr}}
	st, _, err := readPodStatus(ctx, pod)
	if err != nil {
		t.Fatal(err)
	}
	if st.FeaturesRev != 5 || st.LocalLease == nil {
		t.Fatalf("status read without \"caps\":false: %+v", st)
	}
	if free, _, err := podIsFree(podCandidate{Addr: p.addr}); err != nil || !free {
		t.Errorf("podIsFree: %v %v", free, err)
	}
}

func TestCILeaseFromFeaturesRev(t *testing.T) {
	t.Cleanup(func() { tcpclient.SetLeaseJoin(nil) })
	cases := []struct {
		rev       int
		supported bool
		acquire   bool
		warn      string
	}{
		{5, true, true, ""},
		{4, false, false, "features_rev 4; the lease needs features_rev 5"},
		{0, false, true, "the caps list"}, // older firmware: lease_acquire answers unknown command
	}
	for _, c := range cases {
		p := startStatusFormPod(t, c.rev)
		var warn bytes.Buffer
		conn := &ciConnected{pod: &networkPod{client: &tcpclient.Client{Addr: p.addr}}}
		join := &tcpclient.LeaseJoin{ID: "L-t", Holder: "benchpod ci: t", Purpose: "s", TTL: 300}
		l, err := takeCILease(context.Background(), conn, join, 0, &warn)
		if err != nil {
			t.Fatalf("rev %d: %v", c.rev, err)
		}
		if l.Supported() != c.supported {
			t.Errorf("rev %d: supported %v\n%s", c.rev, l.Supported(), warn.String())
		}
		cmds := strings.Join(p.cmds(), ",")
		if strings.Contains(cmds, "lease_acquire") != c.acquire {
			t.Errorf("rev %d: commands %s", c.rev, cmds)
		}
		if c.warn != "" && !strings.Contains(warn.String(), c.warn) {
			t.Errorf("rev %d: warning %q lacks %q", c.rev, warn.String(), c.warn)
		}
		if c.supported && !strings.Contains(warn.String(), "lease: L-t held (status: features_rev 5)") {
			t.Errorf("rev %d: log %q", c.rev, warn.String())
		}
		l.Release(context.Background())
	}
}

func TestStatusJSONHasLocalLease(t *testing.T) {
	p := startStatusFormPod(t, 5)
	out := filepath.Join(t.TempDir(), "status.json")
	if code := run([]string{"--connection", p.addr, "--tls", "off", "status", "--json", "--output-filename", out}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), `"local_lease"`) || !strings.Contains(string(data), `"features_rev": 5`) || strings.Contains(string(data), `"caps"`) {
		t.Errorf("status --json:\n%s", data)
	}
}
