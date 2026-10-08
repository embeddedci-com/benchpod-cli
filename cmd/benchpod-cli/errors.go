package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
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
	return refusedWithHint(what, pe, explainRefusal(pe.Reason, overLAN))
}

// refusedWithHint is refusedError with the hint given ("" for none).
func refusedWithHint(what string, pe *tcpclient.PodError, hint string) error {
	msg := what + ": the pod refused: " + pe.Reason
	if hint != "" {
		msg += " (" + hint + ")"
	}
	return &shownRefusal{msg: msg, pe: pe}
}

// shownRefusal is a refusal worded for the user; it still unwraps to the refusal, so the exit
// code follows it.
type shownRefusal struct {
	msg string
	pe  *tcpclient.PodError
}

func (e *shownRefusal) Error() string { return e.msg }
func (e *shownRefusal) Unwrap() error { return e.pe }

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

// ── exit codes ──────────────────────────────────────────────────────────────

// The CLI's exit codes, so a script can tell a refusal from an unreachable pod. Documented in
// the root command's help and the README.
const (
	exitOK          = 0
	exitError       = 1 // anything else
	exitUsage       = 2 // a bad flag, argument or subcommand
	exitRefused     = 3 // the pod refused the command (locked LAN, missing role, bad request)
	exitBusy        = 4 // the pod or one of its engines is in use (a cloud lease, another session)
	exitUnreachable = 5 // the pod did not answer on its address, or no pod on USB
)

// exitCodesHelp is the exit-code table for the root command's help.
const exitCodesHelp = "Exit codes:\n" +
	"  0  success\n" +
	"  1  any other error\n" +
	"  2  usage: a bad flag, argument or subcommand\n" +
	"  3  refused: the pod refused the command (a locked LAN, a missing role, a bad request)\n" +
	"  4  busy: the pod or one of its engines is in use (a cloud job's lease, another session)\n" +
	"  5  unreachable: nothing answered on the pod's address, or no pod found on USB"

// usageError marks a command-line mistake (exit code 2).
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// usageErrorf is a usage error built like fmt.Errorf.
func usageErrorf(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

// exitCode maps a command's error to the CLI's exit code.
func exitCode(err error) int {
	if err == nil {
		return exitOK
	}
	var ue *usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	// Cobra's own complaint about an unknown subcommand is not a typed error.
	if strings.HasPrefix(err.Error(), "unknown command ") {
		return exitUsage
	}
	if pe, ok := tcpclient.AsPodError(err); ok {
		if pe.Kind() == tcpclient.Busy {
			return exitBusy
		}
		return exitRefused
	}
	if errors.Is(err, tcpclient.ErrUnreachable) {
		return exitUnreachable
	}
	return exitError
}

// markUsageErrors makes every flag and argument error of cmd and its subcommands a usageError.
func markUsageErrors(cmd *cobra.Command) {
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return &usageError{err: err} })
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if args := c.Args; args != nil {
			c.Args = func(c *cobra.Command, a []string) error {
				if err := args(c, a); err != nil {
					return &usageError{err: err}
				}
				return nil
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(cmd)
}
