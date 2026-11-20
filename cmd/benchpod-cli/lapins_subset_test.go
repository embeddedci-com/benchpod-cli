package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// fakeLAPins answers la_pins like the firmware: "la" selects pins when subset is true (firmware
// after 3.10.12), the full listing fails with "reply too large" when tooLarge is true. With page
// > 0 (firmware with la_pins_paged, features rev 12) a reply carries at most page pins and says
// "next"; "from" continues there. badNext makes next point back at LA1.
type fakeLAPins struct {
	subset, tooLarge bool
	tooLargeCode     string
	page             int
	badNext          bool
	sent             []map[string]any
}

func (f *fakeLAPins) Command(_ context.Context, req map[string]any) (json.RawMessage, error) {
	if req["cmd"] == "status" {
		rev := 11
		if f.page > 0 {
			rev = 12
		}
		return json.Marshal(map[string]any{"version": "4.0.1", "features_rev": rev, "gateware": 60})
	}
	f.sent = append(f.sent, req)
	want, has := req["la"].([]int)
	if !f.subset {
		has = false
	}
	if !has && f.tooLarge {
		code := f.tooLargeCode
		if code == "" {
			code = "BP0053"
		}
		return nil, tcpclient.RefusalError("", code, nil)
	}
	from := 1
	if v, ok := req["from"].(int); ok && f.page > 0 {
		from = v
	}
	var pins []map[string]any
	for la := from; la <= 14; la++ {
		ok := !has
		for _, w := range want {
			ok = ok || w == la
		}
		if ok {
			pins = append(pins, map[string]any{"la": la, "function": "none"})
		}
	}
	rep := map[string]any{"pull_dirs": []string{"up", "down"}, "levels": len(f.sent)}
	if f.page > 0 && len(pins) > f.page {
		rep["next"] = pins[f.page]["la"]
		if f.badNext {
			rep["next"] = 1
		}
		pins = pins[:f.page]
	}
	rep["pins"] = pins
	return json.Marshal(rep)
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
	_, _ = laPinsFetch(context.Background(), f, nil)
	if last := f.sent[len(f.sent)-1]; !reflect.DeepEqual(last, map[string]any{"cmd": "la_pins"}) {
		t.Fatalf("plain request %v", last)
	}
}

// A full listing too large for one reply (BP0053) is read in two halves and joined.
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
}

// Over the cloud the refusal is BP0054: the same two halves.
func TestLAPinsTooLargeOverCloudReadsInTwoParts(t *testing.T) {
	f := &fakeLAPins{subset: true, tooLarge: true, tooLargeCode: "BP0054"}
	raw, err := laPinsFetch(context.Background(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := pinNumbers(t, raw); len(got) != 14 || len(f.sent) != 3 {
		t.Fatalf("pins %v, requests %v", got, f.sent)
	}
}

// Firmware with la_pins_paged: follow next with from, join the pins, levels from the last reply.
func TestLAPinsPaged(t *testing.T) {
	f := &fakeLAPins{subset: true, page: 5}
	raw, err := laPinsFetch(context.Background(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	if got := pinNumbers(t, raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("pins %v", got)
	}
	wantSent := []map[string]any{{"cmd": "la_pins"}, {"cmd": "la_pins", "from": 6}, {"cmd": "la_pins", "from": 11}}
	if !reflect.DeepEqual(f.sent, wantSent) {
		t.Fatalf("requests %v", f.sent)
	}
	var rep map[string]any
	_ = json.Unmarshal(raw, &rep)
	if _, has := rep["next"]; has || rep["levels"] != float64(3) {
		t.Fatalf("joined reply %v", rep)
	}
}

// A subset pages with the same la.
func TestLAPinsPagedSubset(t *testing.T) {
	f := &fakeLAPins{subset: true, page: 2}
	raw, err := laPinsFetch(context.Background(), f, []int{2, 4, 6, 8, 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := pinNumbers(t, raw); !reflect.DeepEqual(got, []int{2, 4, 6, 8, 10}) {
		t.Fatalf("pins %v", got)
	}
	la := []int{2, 4, 6, 8, 10}
	wantSent := []map[string]any{{"cmd": "la_pins", "la": la}, {"cmd": "la_pins", "la": la, "from": 6},
		{"cmd": "la_pins", "la": la, "from": 10}}
	if !reflect.DeepEqual(f.sent, wantSent) {
		t.Fatalf("requests %v", f.sent)
	}
}

// A listing that fits is one request, and a next that does not move forward stops the loop.
func TestLAPinsPagedStops(t *testing.T) {
	f := &fakeLAPins{subset: true, page: 14}
	if _, err := laPinsFetch(context.Background(), f, nil); err != nil || len(f.sent) != 1 {
		t.Fatalf("err %v, requests %v", err, f.sent)
	}
	f = &fakeLAPins{subset: true, page: 5, badNext: true}
	raw, err := laPinsFetch(context.Background(), f, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := pinNumbers(t, raw); !reflect.DeepEqual(got, []int{1, 2, 3, 4, 5}) || len(f.sent) != 1 {
		t.Fatalf("pins %v, requests %v", got, f.sent)
	}
}
