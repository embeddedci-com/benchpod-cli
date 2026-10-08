package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// explainRefusal is the one place a policy refusal gets its hint; the texts come from the
// firmware (internal/fwrefusals).
func TestExplainRefusal(t *testing.T) {
	for _, c := range []struct {
		id      string
		overLAN bool
		want    string
	}{
		{"lan_locked", true, lockedUSBHint},
		{"lan_locked", false, ""},
		{"lease_busy", true, leaseBusyHint},
		{"lease_busy", false, ""},
		{"heavy_busy", true, heavyBusyHint},
		{"heavy_busy", false, heavyBusyHint},
		{"tunnel_forbidden", false, forbiddenHint},
		{"psram_bus_busy", true, ""}, // says what to do already
		{"swd_busy_spi_session", true, ""},
		{"unknown_cmd", true, ""},
	} {
		if got := explainRefusal(fwrefusals.Example(c.id), c.overLAN); got != c.want {
			t.Errorf("%s (LAN %v): got %q, want %q", c.id, c.overLAN, got, c.want)
		}
	}
}

func TestWithRefusalHintAddsTheHintOnce(t *testing.T) {
	msg := fwrefusals.Example("lan_locked")
	err := withRefusalHint(fmt.Errorf("la pullup: %w", &tcpclient.PodError{Reason: msg}))
	if got := err.Error(); got != "la pullup: "+msg+" ("+lockedUSBHint+")" {
		t.Fatalf("got %q", got)
	}
	if _, ok := tcpclient.AsPodError(err); !ok {
		t.Fatal("the hint hid the refusal")
	}
	// Already explained (refusedError): left alone.
	once := refusedError("cloud ca set", &tcpclient.PodError{Reason: msg}, true)
	if got := withRefusalHint(once).Error(); strings.Count(got, lockedUSBHint) != 1 {
		t.Fatalf("got %q", got)
	}
	// A console refusal did not come over the LAN, and other errors pass through.
	usb := &tcpclient.PodError{Cmd: "proxy-set", Reason: msg}
	if got := withRefusalHint(usb); got != error(usb) {
		t.Fatalf("got %v", got)
	}
	plain := errors.New("connect to bench pod at 10.0.0.1:8080: i/o timeout")
	if got := withRefusalHint(plain); got != plain {
		t.Fatalf("got %v", got)
	}
}
