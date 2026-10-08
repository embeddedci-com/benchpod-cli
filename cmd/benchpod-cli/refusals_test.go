package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// The hints the CLI adds to a refusal hang on the firmware's exact wording
// (internal/fwrefusals pins it against the firmware source).

func TestALockedLANRefusalGetsTheUSBHint(t *testing.T) {
	msg := fwrefusals.Example("lan_locked")
	err := cloudCfgError("cloud ca set", caMissing, true, &podRefusal{msg: msg})
	if !strings.Contains(err.Error(), msg) || !strings.Contains(err.Error(), lockedUSBHint) {
		t.Fatalf("got %v", err)
	}
	// Not over the cloud or USB: there the refusal stands alone.
	if err := cloudCfgError("cloud ca set", caMissing, false, &podRefusal{msg: msg}); strings.Contains(err.Error(), lockedUSBHint) {
		t.Fatalf("got %v", err)
	}
}

func TestTheCloudLinkGateRefusalsGetTheLANHint(t *testing.T) {
	for _, id := range []string{"cloud_ca_from_lan", "cloud_proxy_from_lan"} {
		msg := fwrefusals.Example(id)
		if !isLANGateRefusal(msg) {
			t.Errorf("%s: %q not recognized", id, msg)
		}
		if err := cloudCfgError("cloud", caMissing, true, &podRefusal{msg: msg}); !strings.Contains(err.Error(), lanGateHint) {
			t.Errorf("%s: got %v", id, err)
		}
	}
	// The policy commands use the same tail, but they are not cloud link settings.
	for _, id := range []string{"sig_policy_from_lan", "lan_policy_from_lan", "lan_locked"} {
		if isLANGateRefusal(fwrefusals.Example(id)) {
			t.Errorf("%s: %q taken for a cloud link refusal", id, fwrefusals.Example(id))
		}
	}
}

func TestOldFirmwareRefusalsMeanTheFeatureIsMissing(t *testing.T) {
	for _, id := range []string{"unknown_cmd", "unknown_target"} {
		err := cloudCfgError("cloud ca set", caMissing, true, errors.New(fwrefusals.Example(id)))
		if err.Error() != caMissing {
			t.Errorf("%s: got %v", id, err)
		}
	}
}

func TestACloudRefusalToLoosenTheSignaturePolicyPointsAtUSB(t *testing.T) {
	msg := fwrefusals.Example("sig_policy_loosen")
	err := cloudPolicyError(sigPolicy, "audit", &serverapi.APIError{Status: http.StatusConflict, Message: msg})
	if !strings.Contains(err.Error(), "only the USB console can loosen") ||
		!strings.Contains(err.Error(), "benchpod sig-policy set audit --connection usb") {
		t.Fatalf("got %v", err)
	}
	// Other refusals get no USB recipe.
	err = cloudPolicyError(sigPolicy, "audit", &serverapi.APIError{Status: http.StatusConflict,
		Message: fwrefusals.Example("sig_policy_unknown")})
	if strings.Contains(err.Error(), "--connection usb") {
		t.Fatalf("got %v", err)
	}
}

func TestFirmwareRefusalsKeepTheirPrefixes(t *testing.T) {
	// The SDK and MCP classify by these prefixes (locked:, busy:, forbidden:); the CLI shows them
	// as they are. Pin the prefix of each policy refusal.
	for id, prefix := range map[string]string{
		"lan_locked":       "locked: ",
		"lease_busy":       "busy: ",
		"ota_owner_busy":   "busy: ",
		"psram_bus_busy":   "busy: ",
		"tunnel_forbidden": "forbidden: ",
	} {
		if msg := fwrefusals.Example(id); !strings.HasPrefix(msg, prefix) {
			t.Errorf("%s: %q does not start with %q", id, msg, prefix)
		}
	}
}
