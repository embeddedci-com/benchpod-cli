package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// An unreachable pod at the saved default says the address is the saved one and how to change it;
// an address the user passed, or another error, is left as it is.
func TestSavedConnectionHint(t *testing.T) {
	dial := fmt.Errorf("status: connect to pod at 10.237.8.85:8080: %w", tcpclient.ErrUnreachable)
	got := withSavedConnectionHint(dial, "10.237.8.85:8080").Error()
	for _, want := range []string{"pod unreachable", "10.237.8.85:8080 is the saved default connection", "benchpod discover", "set-connection"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if !errors.Is(withSavedConnectionHint(dial, "10.237.8.85:8080"), tcpclient.ErrUnreachable) {
		t.Error("the hint must keep the error's identity (exit code)")
	}
	if got := withSavedConnectionHint(dial, ""); got != dial {
		t.Errorf("no saved default used: %v", got)
	}
	// The default was read, but what failed was another path (flash-self's USB console): no hint.
	usb := fmt.Errorf("no USB ports found: %w", tcpclient.ErrUnreachable)
	if got := withSavedConnectionHint(usb, "10.237.8.85:8080"); got != usb {
		t.Errorf("another path failed: %v", got)
	}
	other := errors.New("refused: busy")
	if got := withSavedConnectionHint(other, "10.0.0.1:8080"); got != other {
		t.Errorf("not unreachable: %v", got)
	}
}
