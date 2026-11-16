package main

import (
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

func TestCodedRefusalUsesTheCLIFix(t *testing.T) {
	pe := &tcpclient.PodError{Reason: "cannot delete the default profile", Code: "BP0013",
		Args: map[string]any{"name": "k66"}}
	text, ok := codedRefusal(pe)
	if !ok || !strings.HasPrefix(text, "BP0013: k66 is the pod's default wiring profile") ||
		!strings.Contains(text, "`benchpod wiring default <other>`") || !strings.Contains(text, "docs/errors#BP0013") {
		t.Fatalf("coded refusal = %q", text)
	}
	err := withRefusalHint(pe)
	if !strings.Contains(err.Error(), "BP0013") {
		t.Errorf("withRefusalHint = %q", err)
	}
	// A code this CLI does not know (newer firmware): the pod's text, the code and its link.
	text, ok = codedRefusal(&tcpclient.PodError{Reason: "something new", Code: "BP9999"})
	if !ok || text != "BP9999: something new (https://embeddedci.com/docs/errors#BP9999)" {
		t.Errorf("unknown code = %q", text)
	}
	if _, ok := codedRefusal(&tcpclient.PodError{Reason: "busy"}); ok {
		t.Error("a refusal without a code is not coded")
	}
}

func TestExplainKnowsTheCatalog(t *testing.T) {
	text, ok := errcatalog.Explain("bp0001")
	if !ok || !strings.Contains(text, "la_voltage_not_set") || !strings.Contains(text, "Fix: set the board's I/O voltage") {
		t.Fatalf("explain = %q", text)
	}
	if _, ok := errcatalog.Explain("BP9999"); ok {
		t.Error("BP9999 explained")
	}
}

func TestUpgradeProfileFillsTheReferenceBench(t *testing.T) {
	p, changed, err := upgradeProfile(map[string]any{"uart_rx": float64(7)})
	if err != nil || !changed || p["version"] != 2 || p["uart_rx"] != float64(7) || p["swd_swclk"] != 11 {
		t.Fatalf("v1 upgrade = %v %v %v", p, changed, err)
	}
	p, changed, _ = upgradeProfile(map[string]any{"version": float64(2), "uart_rx": float64(7)})
	if changed || p["swd_swclk"] != nil {
		t.Errorf("v2 must stay as it is: %v", p)
	}
	if _, _, err := upgradeProfile(map[string]any{"version": float64(3)}); err == nil {
		t.Error("version 3 accepted")
	}
}
