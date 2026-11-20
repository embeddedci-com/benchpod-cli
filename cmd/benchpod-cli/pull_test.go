package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// pullPod records the la_pull requests.
type pullPod struct {
	sent []map[string]any
}

func (p *pullPod) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	p.sent = append(p.sent, req)
	return json.Marshal(map[string]any{"la": req["la"], "ok": true})
}

// la pull sends la_pull with the direction; the pod decides (and refuses LA7 up itself).
func TestLAPullSendsTheDirection(t *testing.T) {
	p := &pullPod{}
	for _, want := range []string{"up", "down", "off"} {
		if _, err := setLAPull(context.Background(), p, 7, want); err != nil {
			t.Fatal(err)
		}
	}
	want := []map[string]any{{"cmd": "la_pull", "la": 7, "pull": "up"}, {"cmd": "la_pull", "la": 7, "pull": "down"},
		{"cmd": "la_pull", "la": 7, "pull": "off"}}
	if !reflect.DeepEqual(p.sent, want) {
		t.Fatalf("sent %v", p.sent)
	}
}
