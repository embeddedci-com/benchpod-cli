package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/errcatalog"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// ── refusals ────────────────────────────────────────────────────────────────
//
// Every refusal from the pod, over the LAN JSON API or the USB console, is a *tcpclient.PodError.
// A refusal with a stable code (BP0001...) is classified by tcpclient's code table (the exit code)
// and worded from the catalog (internal/errcatalog: its message and fix.cli, codedRefusal). A
// refusal without one (older firmware, or a refusal with no code yet) falls back to its text: the
// policy refusals carry a firmware prefix (locked:, busy:, forbidden:; internal/fwrefusals pins
// the texts) and explainRefusal is the one place that says what to do about each. Each text match
// below names what replaces it: a code, or "no code yet".

const (
	lockedUSBHint = "the pod's external LAN policy is locked; run this over USB with --connection usb"
	leaseBusyHint = "on the LAN the pod can only be read while a cloud job holds it; try again when the job ends"
	heavyBusyHint = "the pod is running another long command; try again when it ends"
	forbiddenHint = "API keys need the benchpod:admin scope"
)

// laVoltageUnsetHint answers the firmware's "la voltage not set; ..." (it names the raw JSON command
// or the profile key rather than what a CLI user runs). The pod and the tools take the voltage from
// the wiring profile, so the lasting fix is to set it there.
const laVoltageUnsetHint = "the wiring profile sets no I/O voltage: set it in the profile (the I/O voltage " +
	"at the top of the Pins pane in the web or desktop app), or for now `benchpod la voltage 3.3V` (or 1.8V)"

// pinConflictRE matches the firmware's "pin conflict: LA%u is in use by %s; %s" (la_pins.c).
// A sensor pin names its function too: "is in use by sensor:env (i2c_sda); ...", and an I2C
// line several sensors share "sensor:env+1".
var pinConflictRE = regexp.MustCompile(`^pin conflict: (LA\d+) is in use by (\S+)(?: \(([a-z0-9_]+)\))?; (.*)$`)

// emulationImageRE matches the refusal of a feature the emulation image lacks
// (command_handler_dac.c image_hint) and the image-switch refusal (fpga_image).
var emulationImageRE = regexp.MustCompile(`\{"cmd":"sensor_stop","all":true\} then \{"cmd":"fpga_image","image":"(loop|deep)"\}$`)

// pinReleaseHints maps the firmware's release hints (la_pins.c release_hint) to what a CLI user
// does about them. The benchpod CLI has no command that holds a pin past its own run, so a pin in
// use is held by another client of the pod; the hint names where to free it. A hint missing from
// this map (a new owner) is shown as the firmware wrote it.
var pinReleaseHints = []struct {
	prefix string // the firmware hint starts with this
	hint   string // %s = the pin (LA4)
}{
	{`release it with {"cmd":"gpio"`, "another session (the web app, a test or an MCP session) holds %s as a GPIO; " +
		"release it there (gpio_release)"},
	{"stop the uart proxy first", "a UART session (the web app's Terminal, a test or an MCP session) " +
		"is open on %s; close it there"},
	{"end the SWD session first", "an SWD session (a flash or an OpenOCD run) is using %s; wait for it to end"},
	{"stop the sensor emulation first", "an emulated sensor is running on %s; stop it with " +
		"`benchpod sensor stop` (or where it was started: the web app, the SDK or MCP sensor_stop)"},
	{"stop the SPI session first", "an SPI session is open on %s; end it where it was started " +
		"(the web app, the SDK or an MCP session)"},
	{"stop the I2C session first", "the I2C master is using %s; end it with `benchpod i2c stop`"},
}

// i2cPullupRE matches i2c_start's "I2C needs pull-ups: switch on LA1's with
// {"cmd":"la","la":1,"pullup":"on"}, or pass "pullups":"external"".
// Firmware with la_pull_dir names {"cmd":"la","la":1,"pull":"up"} instead.
var i2cPullupRE = regexp.MustCompile(`^(I2C needs pull-ups): switch on LA(\d+)'s with \{"cmd":"la","la":\d+,"(?:pullup":"on|pull":"up)"\}`)

// translateRefusal rewords a refusal whose firmware text names a raw JSON command: it returns the
// text to show and the hint to add ("" for none). Other refusals come back unchanged with no hint.
func translateRefusal(msg string) (shown, hint string) {
	msg = strings.TrimSpace(msg)
	if strings.HasPrefix(msg, "la voltage not set") { // fallback: BP0001 replaces it
		return msg, laVoltageUnsetHint
	}
	if m := i2cPullupRE.FindStringSubmatch(msg); m != nil { // no code yet
		return m[1], fmt.Sprintf("pull LA%s up with `benchpod la pull %s up`, or pass --pullups external "+
			"when the board has its own", m[2], m[2])
	}
	// fallback: BP0017 (gateware_too_old) replaces it
	if strings.HasSuffix(msg, "need gateware v55+: update the pod's firmware") ||
		strings.HasSuffix(msg, "needs gateware v55+: update the pod's firmware") {
		return msg, "update it with `benchpod flash-self`"
	}
	if m := emulationImageRE.FindStringSubmatchIndex(msg); m != nil { // no code yet
		image := msg[m[2]:m[3]]
		return strings.TrimSuffix(strings.TrimSpace(msg[:m[0]]), ":"), fmt.Sprintf(
			"stop the sensors with `benchpod sensor stop`, then switch with `benchpod sensor image %s`", image)
	}
	if strings.HasPrefix(msg, "image switch refused:") { // no code yet
		if i := strings.Index(msg, "; stop them first ("); i > 0 {
			return msg[:i], "stop them with `benchpod sensor stop` first"
		}
	}
	if m := pinConflictRE.FindStringSubmatch(msg); m != nil { // no code yet
		owner := m[2]
		if m[3] != "" {
			owner += " (" + m[3] + ")"
		}
		if inst, ok := strings.CutPrefix(m[2], "sensor:"); ok {
			inst, _, _ = strings.Cut(strings.TrimRight(inst, ";"), "+")
			return "pin conflict: " + m[1] + " is in use by " + owner, fmt.Sprintf(
				"the emulated sensor %s is using %s; stop it with `benchpod sensor stop %s`", inst, m[1], inst)
		}
		for _, h := range pinReleaseHints {
			if strings.HasPrefix(m[4], h.prefix) {
				return "pin conflict: " + m[1] + " is in use by " + owner, fmt.Sprintf(h.hint, m[1])
			}
		}
	}
	return msg, ""
}

// explainRefusal returns the hint for a refusal without a code, by its text, or "" when the
// message says it all. overLAN is whether the refusal came over the LAN JSON API: only the LAN
// refuses with locked: or a cloud lease, so elsewhere those texts stand alone. A coded refusal
// gets the catalog's fix.cli instead (refusalHint).
func explainRefusal(msg string, overLAN bool) string {
	msg = strings.TrimSpace(msg)
	switch tcpclient.ClassifyRefusal(msg) {
	case tcpclient.Locked: // no code yet
		if overLAN {
			return lockedUSBHint
		}
	case tcpclient.Busy:
		switch {
		case overLAN && strings.HasPrefix(msg, "busy: a cloud job holds"): // fallback: BP0025 replaces it
			return leaseBusyHint
		case msg == "busy": // no code yet
			return heavyBusyHint
		}
	case tcpclient.Forbidden: // no code yet
		return forbiddenHint
	}
	_, hint := translateRefusal(msg)
	return hint
}

// refusalHint is what to do about a refusal: the catalog's fix.cli for a coded one, else
// explainRefusal's hint from the text ("" for none).
func refusalHint(pe *tcpclient.PodError, overLAN bool) string {
	if pe.Code != "" {
		if e, ok := errcatalog.Lookup(pe.Code); ok {
			return errcatalog.Fill(e.Fix["cli"], pe.Args)
		}
	}
	return explainRefusal(pe.Reason, overLAN)
}

// refusedError is the one line for a refusal from a command: "<what>: the pod refused: <why>",
// plus refusalHint's hint (a coded refusal: the catalog's message and fix.cli).
func refusedError(what string, pe *tcpclient.PodError, overLAN bool) error {
	return refusedWithHint(what, pe, refusalHint(pe, overLAN))
}

// isUnknownCommand says the pod does not know the command it was sent: its firmware is older
// than the command. BP0002, else (firmware without codes) the text "unknown cmd".
func isUnknownCommand(err error) bool { return tcpclient.IsUnknownCommand(err) }

// refusedWithHint is refusedError with the hint given ("" for none).
func refusedWithHint(what string, pe *tcpclient.PodError, hint string) error {
	if coded, ok := codedRefusal(pe); ok {
		return &shownRefusal{msg: what + ": the pod refused: " + coded, pe: pe}
	}
	shown, _ := translateRefusal(pe.Reason)
	msg := what + ": the pod refused: " + shown
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
	// embeddedci.com's own failures (a held lease, a missing role) say what to do already.
	var ce *cloudpod.Error
	if errors.As(err, &ce) {
		return err
	}
	pe, ok := tcpclient.AsPodError(err)
	if !ok {
		return err
	}
	if coded, ok := codedRefusal(pe); ok {
		if strings.Contains(err.Error(), coded) {
			return err
		}
		msg := strings.Replace(err.Error(), strings.TrimSpace(pe.Reason), coded, 1)
		if msg == err.Error() {
			msg = err.Error() + ": " + coded
		}
		return &shownRefusal{msg: msg, pe: pe}
	}
	hint := explainRefusal(pe.Reason, pe.Cmd == "")
	if hint == "" || strings.Contains(err.Error(), hint) {
		return err
	}
	if shown, _ := translateRefusal(pe.Reason); shown != strings.TrimSpace(pe.Reason) {
		// Drop the raw JSON the firmware suggests; the hint says it in CLI terms.
		msg := strings.Replace(err.Error(), strings.TrimSpace(pe.Reason), shown, 1)
		return &shownRefusal{msg: msg + " (" + hint + ")", pe: pe}
	}
	return fmt.Errorf("%w (%s)", err, hint)
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
	"  6  update the CLI: the pod's firmware needs a newer benchpod CLI"

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
	return fmt.Sprintf("%s: %s (%s)", pe.Code, strings.TrimSpace(pe.Reason), errcatalog.DocsURL(pe.Code)), true
}
