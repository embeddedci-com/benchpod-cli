package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

func TestEspBlobMissing(t *testing.T) {
	slots := func(state string) []serialconsole.BlobSlot {
		return []serialconsole.BlobSlot{{Name: "ice40", State: "ok"}, {Name: "esp", State: state}}
	}
	for _, tc := range []struct {
		state string
		want  bool
	}{{"ok", false}, {"unknown", false}, {"missing", true}, {"outdated", true}} {
		got, err := espBlobMissing(slots(tc.state), nil)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v, %v; want %v", tc.state, got, err, tc.want)
		}
	}
	// Firmware with its blobs built in has nothing to install.
	if got, err := espBlobMissing(nil, serialconsole.ErrNoBlobSlots); err != nil || got {
		t.Errorf("no slots: got %v, %v", got, err)
	}
	// Any other error reading the slots is surfaced, not taken for "nothing to do".
	boom := errors.New("read serial: device not configured")
	got, err := espBlobMissing(nil, boom)
	if got || !errors.Is(err, boom) || !strings.Contains(err.Error(), "--skip-blobs") {
		t.Errorf("read error: got %v, %v", got, err)
	}
}
