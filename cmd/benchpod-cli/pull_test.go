package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// pullPod answers status (features_rev) and records the la requests.
type pullPod struct {
	rev  int
	sent []map[string]any
}

func (p *pullPod) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	if req["cmd"] == "status" {
		return json.Marshal(map[string]any{"version": "3.10.11", "features_rev": p.rev, "gateware": 58,
			"fpga_image": "deep", "hw": []string{"nrst_pin"}, "api": 2, "min_client_api": 2})
	}
	p.sent = append(p.sent, req)
	return json.Marshal(map[string]any{"la": req["la"], "ok": true})
}

func TestLAPullSendsTheDirectionToNewFirmware(t *testing.T) {
	p := &pullPod{rev: 3}
	for _, want := range []string{"up", "down", "off"} {
		if _, err := setLAPull(context.Background(), p, 7, want); err != nil {
			t.Fatal(err)
		}
	}
	want := []map[string]any{{"cmd": "la", "la": 7, "pull": "up"}, {"cmd": "la", "la": 7, "pull": "down"},
		{"cmd": "la", "la": 7, "pull": "off"}}
	if !reflect.DeepEqual(p.sent, want) {
		t.Fatalf("sent %v", p.sent) // the pod decides (and refuses LA7 up itself)
	}
}

func TestLAPullFallsBackToPullupOnOlderFirmware(t *testing.T) {
	p := &pullPod{rev: 2}
	ctx := context.Background()
	if _, err := setLAPull(ctx, p, 1, "up"); err != nil {
		t.Fatal(err)
	}
	if _, err := setLAPull(ctx, p, 7, "down"); err != nil {
		t.Fatal(err)
	}
	if _, err := setLAPull(ctx, p, 7, "off"); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"cmd": "la", "la": 1, "pullup": "on"}, {"cmd": "la", "la": 7, "pullup": "on"},
		{"cmd": "la", "la": 7, "pullup": "off"}}
	if !reflect.DeepEqual(p.sent, want) {
		t.Fatalf("sent %v", p.sent)
	}
	// the CLI refuses what "pullup":"on" would do wrong, with the pod's own codes
	for _, c := range []struct {
		la         int
		want, code string
		text       string
	}{
		{7, "up", "BP0024", "LA7 supports pull-down only on this board"},
		{3, "down", "BP0024", "LA3 supports pull-up only on this board"},
		{9, "up", "BP0023", "LA9 has no pull resistor on this board"},
	} {
		_, err := setLAPull(ctx, p, c.la, c.want)
		var pe *tcpclient.PodError
		if !errors.As(err, &pe) || pe.Code != c.code || !strings.Contains(pe.Reason, c.text) {
			t.Fatalf("LA%d %s: err = %v", c.la, c.want, err)
		}
		if text, ok := codedRefusal(pe); !ok || !strings.Contains(text, c.code) {
			t.Fatalf("codedRefusal = %q", text)
		}
	}
	// off on a pin without a resistor: nothing to switch, nothing sent
	n := len(p.sent)
	data, err := setLAPull(ctx, p, 12, "off")
	if err != nil || len(p.sent) != n || !strings.Contains(string(data), `"state":"off"`) {
		t.Fatalf("data %s err %v sent %d", data, err, len(p.sent)-n)
	}
}

func TestLAPullRefusesABadDirection(t *testing.T) {
	g := &globalFlags{connection: "127.0.0.1:1"}
	cmd := newLAPullCmd(g)
	cmd.SetArgs([]string{"1", "sideways"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || exitCode(err) != exitUsage || !strings.Contains(err.Error(), "up, down or off") {
		t.Fatalf("err = %v", err)
	}
}
