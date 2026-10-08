package main

import (
	"fmt"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── refusals ────────────────────────────────────────────────────────────────
//
// Every refusal from the pod, over the LAN JSON API or the USB console, is a *tcpclient.PodError.
// The policy refusals carry a firmware prefix (locked:, busy:, forbidden:; internal/fwrefusals
// pins the texts) and explainRefusal is the one place that says what to do about each.

const (
	lockedUSBHint = "the pod's LAN policy is locked; run this over the pod's USB console with --connection usb"
	leaseBusyHint = "on the LAN the pod can only be read while a cloud job holds it; try again when the job ends"
	heavyBusyHint = "the pod is running another long command; try again when it ends"
	forbiddenHint = "API keys need the benchpod:admin scope"
)

// explainRefusal returns the hint for a policy refusal, or "" when the message says it all.
// overLAN is whether the refusal came over the LAN JSON API: only the LAN refuses with locked:
// or a cloud lease, so elsewhere those texts stand alone.
func explainRefusal(msg string, overLAN bool) string {
	msg = strings.TrimSpace(msg)
	switch tcpclient.ClassifyRefusal(msg) {
	case tcpclient.Locked:
		if overLAN {
			return lockedUSBHint
		}
	case tcpclient.Busy:
		switch {
		case overLAN && strings.HasPrefix(msg, "busy: a cloud job holds"):
			return leaseBusyHint
		case msg == "busy":
			return heavyBusyHint
		}
	case tcpclient.Forbidden:
		return forbiddenHint
	}
	return ""
}

// refusedError is the one line for a refusal from a command: "<what>: the pod refused: <why>",
// plus explainRefusal's hint.
func refusedError(what string, pe *tcpclient.PodError, overLAN bool) error {
	if hint := explainRefusal(pe.Reason, overLAN); hint != "" {
		return fmt.Errorf("%s: the pod refused: %s (%s)", what, pe.Reason, hint)
	}
	return fmt.Errorf("%s: the pod refused: %s", what, pe.Reason)
}

// withRefusalHint adds explainRefusal's hint to an error that carries a refusal and does not
// show the hint yet. Execute applies it to every command's error, so a command that just wraps
// the client's error (%w) still gets the hint. A JSON reply (no console command) came over the
// LAN.
func withRefusalHint(err error) error {
	pe, ok := tcpclient.AsPodError(err)
	if !ok {
		return err
	}
	hint := explainRefusal(pe.Reason, pe.Cmd == "")
	if hint == "" || strings.Contains(err.Error(), hint) {
		return err
	}
	return fmt.Errorf("%w (%s)", err, hint)
}
