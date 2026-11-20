package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// fakeLAPins answers la_pins like the firmware: "la" selects pins when subset is true (firmware
// after 3.10.12), the full listing fails with "reply too large" when tooLarge is true.
type fakeLAPins struct {
	subset, tooLarge bool
	sent             []map[string]any
}

func (f *fakeLAPins) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	f.sent = append(f.sent, req)
	want, has := req["la"].([]int)
	if !f.subset {
		has = false
	}
	if !has && f.tooLarge {
		return nil, tcpclient.RefusalError("", "BP0053", nil)
	}
	var pins []map[string]any
	for la := 1; la <= 14; la++ {
		ok := !has
		for _, w := range want {
			ok = ok || w == la
		}
		if ok {
			pins = append(pins, map[string]any{"la": la, "function": "none"})
		}
	}
	return json.Marshal(map[string]any{"pins": pins, "pull_dirs": []string{"up", "down"}, "levels": nil})
}

func pinNumbers(t *testing.T, raw json.RawMessage) []int {
	t.Helper()
	var rep laPinsReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, p := range rep.Pins {
		out = append(out, p.LA)
	}
	return out
}

func TestLAPinsSubset(t *testing.T) {
	f := &fakeLAPins{subset: true}
	raw, err := laPinsFetch(context.Background(), f, []int{9, 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := pinNumbers(t, raw); !reflect.DeepEqual(got, []int{3, 9}) {
		t.Fatalf("pins %v", got)
	}
	// Older firmware ignores "la": the CLI keeps the pins asked for.
	f = &fakeLAPins{}
	raw, _ = laPinsFetch(context.Background(), f, []int{5})
	if got := pinNumbers(t, raw); !reflect.DeepEqual(got, []int{5}) {
		t.Fatalf("old firmware pins %v", got)
	}
	// Without PINS the request is as before.
	_, _ = laPinsFetch(context.Background(), f, nil)
	if last := f.sent[len(f.sent)-1]; !reflect.DeepEqual(last, map[string]any{"cmd": "la_pins"}) {
		t.Fatalf("plain request %v", last)
	}
}

func TestLAPinsTooLargeReadsInTwoParts(t *testing.T) {
	f := &fakeLAPins{subset: true, tooLarge: true}
	raw, err := laPinsFetch(context.Background(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	if got := pinNumbers(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("pins %v", got)
	}
	if len(f.sent) != 3 {
		t.Fatalf("requests %v", f.sent)
	}
	// Firmware without the subset cannot help: the error stands.
	f = &fakeLAPins{tooLarge: true}
	_, err = laPinsFetch(context.Background(), f, nil)
	if err == nil || !strings.Contains(err.Error(), "did not fit") ||
		!strings.Contains(err.Error(), "firmware after 3.10.12 reads this list in parts") {
		t.Fatalf("want the too-large error with the update hint, got %v", err)
	}
}
