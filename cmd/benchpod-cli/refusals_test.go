package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/fwrefusals"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// The hints the CLI adds to a refusal hang on the firmware's exact wording
// (internal/fwrefusals pins it against the firmware source).

func TestALockedLANRefusalGetsTheUSBHint(t *testing.T) {
	msg := fwrefusals.Example("lan_locked")
	err := cloudCfgError("cloud ca set", caMissing, true, &tcpclient.PodError{Reason: msg})
	if !strings.Contains(err.Error(), msg) || !strings.Contains(err.Error(), lockedUSBHint) {
		t.Fatalf("got %v", err)
	}
	// Not over the cloud or USB: there the refusal stands alone.
	if err := cloudCfgError("cloud ca set", caMissing, false, &tcpclient.PodError{Reason: msg}); strings.Contains(err.Error(), lockedUSBHint) {
		t.Fatalf("got %v", err)
	}
}

func TestTheCloudLinkGateRefusalsGetTheLANHint(t *testing.T) {
	for _, id := range []string{"cloud_ca_from_lan", "cloud_proxy_from_lan"} {
		msg := fwrefusals.Example(id)
		if !isLANGateRefusal(msg) {
			t.Errorf("%s: %q not recognized", id, msg)
		}
		if err := cloudCfgError("cloud", caMissing, true, &tcpclient.PodError{Reason: msg}); !strings.Contains(err.Error(), lanGateHint) {
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

func TestTheLAVoltageRefusalNamesTheCLICommand(t *testing.T) {
	pe := &tcpclient.PodError{Reason: fwrefusals.Example("la_voltage_unset")}
	err := withRefusalHint(fmt.Errorf("flash: %w", pe))
	if !strings.Contains(err.Error(), "benchpod la voltage 3.3V") || !strings.Contains(err.Error(), "Wiring tab") {
		t.Fatalf("got %v", err)
	}
	if exitCode(err) != exitRefused {
		t.Fatalf("exit code %d", exitCode(err))
	}
}

func TestPinConflictHintsAreTranslated(t *testing.T) {
	for _, tc := range []struct{ owner, hintID, want string }{
		{"gpio", "pin_hint_gpio", "holds LA4 as a GPIO"},
		{"uart_rx", "pin_hint_uart", "UART session"},
		{"swd_clk", "pin_hint_swd", "SWD session"},
		{"i2c_sda", "pin_hint_sensor", "disable_i2c_sensor"},
		{"spi_sck", "pin_hint_spi", "SPI session"},
		{"gps_tx", "pin_hint_gps", "disable_gps"},
	} {
		msg := "pin conflict: LA4 is in use by " + tc.owner + "; " + fwrefusals.Example(tc.hintID)
		err := withRefusalHint(fmt.Errorf("la step: %w", &tcpclient.PodError{Reason: msg}))
		if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), `{"cmd"`) {
			t.Errorf("%s: got %v", tc.hintID, err)
		}
		if !strings.Contains(err.Error(), "pin conflict: LA4 is in use by "+tc.owner) || exitCode(err) != exitRefused {
			t.Errorf("%s: got %v (exit %d)", tc.hintID, err, exitCode(err))
		}
	}
	// The pinned example renders the gpio case.
	if shown, hint := translateRefusal(fwrefusals.Example("pin_conflict")); shown != "pin conflict: LA4 is in use by gpio" || hint == "" {
		t.Fatalf("got %q / %q", shown, hint)
	}
	// A hint the CLI does not know (the step train's is already plain) stays as the pod wrote it.
	msg := "pin conflict: LA2 is in use by step; " + fwrefusals.Example("pin_hint_step")
	if err := withRefusalHint(&tcpclient.PodError{Reason: msg}); err.Error() != (&tcpclient.PodError{Reason: msg}).Error() {
		t.Fatalf("got %v", err)
	}
	// refusedError shows the translated text too.
	pe := &tcpclient.PodError{Reason: fwrefusals.Example("pin_conflict")}
	if err := refusedError("flash", pe, true); strings.Contains(err.Error(), `{"cmd"`) || !strings.Contains(err.Error(), "gpio_release") {
		t.Fatalf("got %v", err)
	}
}
