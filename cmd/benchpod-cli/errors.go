package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── refusals ────────────────────────────────────────────────────────────────
//
// Every refusal from the pod, over the JSON API (LAN, USB network, embeddedci.com) or the USB
// console, is a *tcpclient.PodError. A refusal with a stable code (BP0001...) is classified by
// tcpclient's code table (the exit code) and worded from the catalog (internal/errcatalog: its
// message and fix.cli, codedRefusal). A refusal without a code is shown as the pod wrote it: the
// CLI never matches on a refusal's text.

// refusalHint is what to do about a refusal: the catalog's fix.cli for a coded one, "" otherwise.
func refusalHint(pe *tcpclient.PodError) string {
	if pe.Code != "" {
		if e, ok := errcatalog.Lookup(pe.Code); ok {
			return errcatalog.Fill(e.Fix["cli"], pe.Args)
		}
	}
	return ""
}

// refusedError is the one line for a refusal from a command: "<what>: the pod refused: <why>"
// (a coded refusal: the catalog's message and fix.cli).
func refusedError(what string, pe *tcpclient.PodError) error {
	return refusedWithHint(what, pe, "")
}

// isUnknownCommand says the pod does not know the command it was sent (BP0002).
func isUnknownCommand(err error) bool { return tcpclient.IsUnknownCommand(err) }

// refusedWithHint is refusedError with a hint of the caller's for a refusal without a code ("" for
// none); a coded refusal always shows the catalog's fix.
func refusedWithHint(what string, pe *tcpclient.PodError, hint string) error {
	if coded, ok := codedRefusal(pe); ok {
		return &shownRefusal{msg: what + ": the pod refused: " + coded, pe: pe}
	}
	msg := what + ": the pod refused: " + strings.TrimSpace(pe.Reason)
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

// withRefusalHint words a coded refusal from the catalog in an error that does not show it yet.
// Execute applies it to every command's error, so a command that just wraps the client's error
// (%w) still gets the catalog's message and fix.
func withRefusalHint(err error) error {
	// embeddedci.com's own failures (a held lease, a missing role) say what to do already.
	var ce *cloudpod.Error
	if errors.As(err, &ce) {
		return err
	}
	pe, ok := tcpclient.AsPodError(err)
	if !ok {
		return err
	}
	coded, ok := codedRefusal(pe)
	if !ok || strings.Contains(err.Error(), coded) {
		return err
	}
	msg := strings.Replace(err.Error(), strings.TrimSpace(pe.Reason), coded, 1)
	if msg == err.Error() {
		msg = err.Error() + ": " + coded
	}
	return &shownRefusal{msg: msg, pe: pe}
}

// withSavedConnectionHint adds where the address came from when the pod did not answer at the
// saved default (saved, "" when the run did not use it): after a pod is unplugged or another pod
// takes its place, that default is stale and only `discover` / `set-connection` fix it.
func withSavedConnectionHint(err error, saved string) error {
	// Only when it is the saved address that did not answer: a command that read the default and
	// then went another way (flash-self over USB DFU, say) fails for its own reasons.
	if saved == "" || !errors.Is(err, tcpclient.ErrUnreachable) || !strings.Contains(err.Error(), saved) {
		return err
	}
	return fmt.Errorf("%w\n%s is the saved default connection. If that pod is unplugged or you use another one, "+
		"run `benchpod discover` to list the pods, then `benchpod set-connection <address|usb:<name>>`, "+
		"or pass --connection", err, saved)
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
	exitUnreachable = 5 // the pod did not answer on its address, no pod on USB, or offline on embeddedci.com
	exitUpdateCLI   = 6 // the pod's firmware needs a newer CLI (its API level is above this CLI's, or BP0030)
	exitUpdatePod   = 7 // the pod's firmware is too old for this CLI or the command (below firmware 4.0, BP0002, BP0017)
)

// exitCodesHelp is the exit-code table for the root command's help.
const exitCodesHelp = "Exit codes:\n" +
	"  0  success\n" +
	"  1  any other error\n" +
	"  2  usage: a bad flag, argument or subcommand\n" +
	"  3  refused: the pod refused the command (a locked LAN, a missing role, a bad request)\n" +
	"  4  busy: the pod or one of its engines is in use (a cloud job's lease, another session)\n" +
	"  5  unreachable: nothing answered on the pod's address, no pod found on USB, or the\n" +
	"     pod is offline on embeddedci.com\n" +
	"  6  update the CLI: the pod's firmware needs a newer benchpod CLI\n" +
	"  7  update the pod: its firmware is too old for this CLI (firmware before 4.0:\n" +
	"     `benchpod flash-self`) or lacks what the command needs"

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
	var upd *tcpclient.UpdateRequiredError
	if errors.As(err, &upd) {
		return exitUpdateCLI
	}
	var old *tcpclient.FirmwareTooOldError
	if errors.As(err, &old) {
		return exitUpdatePod
	}
	if pe, ok := tcpclient.AsPodError(err); ok {
		switch pe.Kind() {
		case tcpclient.Busy:
			return exitBusy
		case tcpclient.FirmwareTooOld:
			return exitUpdatePod
		case tcpclient.ClientTooOld:
			return exitUpdateCLI
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

// codedRefusal is a refusal with a stable error code, worded from the catalog: "BP0009: <what went
// wrong>; <what to do in the CLI> (<docs>)". ok is false without a code; a code this CLI's catalog
// does not know (newer firmware) shows the pod's text with the code and its docs link.
func codedRefusal(pe *tcpclient.PodError) (string, bool) {
	if pe == nil || pe.Code == "" {
		return "", false
	}
	if text, ok := errcatalog.Render(pe.Code, pe.Args, "cli"); ok {
		return text, true
	}
	return fmt.Sprintf("%s: %s (%s)", strings.ToUpper(pe.Code), strings.TrimSpace(pe.Reason), errcatalog.DocsURL(pe.Code)), true
}
