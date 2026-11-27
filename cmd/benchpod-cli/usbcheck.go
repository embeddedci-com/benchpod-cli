package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// `benchpod usb-check <test>`: checks of the hosted board's own USB firmware (firmware 4.0.3+,
// feature usb_check): enumeration time after power-on, current against bMaxPower, suspend
// current and resume, bus resets and short VBUS dropouts. The pod runs the check in the
// background; this starts host mode when it is off, waits for the board, starts the check and
// polls it. The tests that switch power need the board's USB VBUS on target-power eFuse 1.

const usbCheckDefaultTimeout = 5 * time.Minute

var usbCheckTests = []string{"enumerate", "power", "suspend", "reset", "replug"}

type usbCheckOpts struct {
	action     string // a test name, "status" or "abort"
	count      int    // 0 = the pod's default
	offMS      []int
	cycleTO    int // timeout_ms
	durationMS int
	settleMS   int
	port       string
	asJSON     bool
	hostSecs   int
	pollPeriod time.Duration
}

type usbCheckStat struct {
	Min int `json:"min"`
	Avg int `json:"avg"`
	Max int `json:"max"`
}

type usbCheckLen struct {
	OffMS       int `json:"off_ms"`
	Stayed      int `json:"stayed"`
	Reenumerate int `json:"reenumerated"`
	Failed      int `json:"failed"`
}

type usbCheckResult struct {
	VID              int           `json:"vid"`
	PID              int           `json:"pid"`
	Cycles           int           `json:"cycles"`
	OK               int           `json:"ok"`
	NoConnect        int           `json:"no_connect"`
	Stuck            int           `json:"stuck"`
	Timeout          int           `json:"timeout"`
	IdentityChanged  int           `json:"identity_changed"`
	OffMS            int           `json:"off_ms"`
	ConnectMS        *usbCheckStat `json:"connect_ms"`
	DescriptorMS     *usbCheckStat `json:"descriptor_ms"`
	MountMS          *usbCheckStat `json:"mount_ms"`
	ReenumerateMS    *usbCheckStat `json:"reenumerate_ms"`
	Samples          int           `json:"samples"`
	AvgUA            int           `json:"avg_ua"`
	MinUA            int           `json:"min_ua"`
	PeakUA           int           `json:"peak_ua"`
	LimitMA          int           `json:"limit_ma"`
	Verdict          string        `json:"verdict"`
	SelfPowered      bool          `json:"self_powered"`
	LimitUA          int           `json:"limit_ua"`
	Current          string        `json:"current"`
	SuspendOK        bool          `json:"suspend_ok"`
	SOFStopped       bool          `json:"sof_stopped"`
	Resume           string        `json:"resume"`
	Answers          bool          `json:"answers"`
	RecoveredByReset bool          `json:"recovered_by_reset"`
	PortReinit       bool          `json:"port_reinit"`
	ReinitOK         bool          `json:"reinit_ok"`
	Count            int           `json:"count"`
	Lengths          []usbCheckLen `json:"lengths"`
}

// usbCheckStatus is the usb_check reply.
type usbCheckStatus struct {
	Running    bool            `json:"running"`
	HaveResult bool            `json:"have_result"`
	Test       string          `json:"test"`
	Phase      string          `json:"phase"`
	Done       int             `json:"done"`
	Total      int             `json:"total"`
	MS         int             `json:"ms"`
	Pass       bool            `json:"pass"`
	Error      string          `json:"error"`
	RawResult  json.RawMessage `json:"result"`
	Result     usbCheckResult  `json:"-"`
}

func (s *usbCheckStatus) decode() {
	if len(s.RawResult) == 0 {
		return
	}
	_ = json.Unmarshal(s.RawResult, &s.Result)
}

func newUSBCheckCmd(g *globalFlags) *cobra.Command {
	var o usbCheckOpts
	var offMS []int
	var duration, settle time.Duration
	cmd := &cobra.Command{
		Use:   "usb-check <enumerate|power|suspend|reset|replug|status|abort>",
		Short: "Check the USB firmware of the board on the pod's USB host port",
		Long: "The pod hosts the board on its USB port and measures what a computer cannot (firmware 4.0.3 and\n" +
			"later). If USB host mode is off, usb-check starts it (and says so). The check runs on the pod;\n" +
			"this shows its progress and prints the result, and exits non-zero when the check fails.\n\n" +
			"  enumerate  power-cycle the board (eFuse 1 off for --off-ms) --count times and time power-on to its\n" +
			"             pull-up, to its device descriptor and to mounted\n" +
			"  power      sample the board's current for --duration and compare it with the bMaxPower it declares\n" +
			"  suspend    suspend the bus, measure the current against the 2.5 mA a bus-powered device may draw,\n" +
			"             resume it and check the board still answers\n" +
			"  reset      reset the port --count times and time the board's re-enumeration\n" +
			"  replug     drop VBUS for each length in --off-ms, --count times each: does the board re-enumerate,\n" +
			"             stay on the bus (its capacitance carried it) or fail\n" +
			"  status     show the running check or the last result\n" +
			"  abort      stop a running check (the bus and eFuse 1 are put back)\n\n" +
			"enumerate, power, suspend and replug switch eFuse 1 or measure its current: wire the board's USB VBUS\n" +
			"to eFuse 1 (the internal 5 V rail) and switch it on (web app, SDK or a `power_on` CI step) first. Currents are the\n" +
			"board's own draw. The global --timeout is the whole deadline (default 5m); a long check needs more.\n\n" +
			"--port is usb_a or usb_c when host mode has to be started (default: usb_a on a pod with the USB-A host\n" +
			"port, board v4 and later; else usb_c). While the pod hosts it is off the computer's USB, so use the\n" +
			"pod's network address (not --connection usb). Host mode stays on until its timeout;\n" +
			"`benchpod usb-host off` stops it.",
		Example: "  benchpod usb-check enumerate --count 20\n" +
			"  benchpod usb-check power --duration 5s\n" +
			"  benchpod usb-check suspend\n" +
			"  benchpod usb-check replug --off-ms 5,20,100 --count 5 --json\n" +
			"  benchpod usb-check status",
		Args:      cobra.ExactArgs(1),
		ValidArgs: append(append([]string{}, usbCheckTests...), "status", "abort"),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.action = strings.ToLower(args[0])
			f := cmd.Flags()
			o.offMS = offMS
			if err := validateUSBCheck(&o, duration, settle, f.Changed("count"), f.Changed("off-ms"), f.Changed("cycle-timeout"), f.Changed("duration"), f.Changed("settle")); err != nil {
				return err
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			total := g.effectiveTimeout(usbCheckDefaultTimeout)
			o.hostSecs = int((total + usbHostExtra) / time.Second)
			if o.hostSecs > 3600 {
				o.hostSecs = 3600
			}
			ctx, cancel, t, err := g.podClient("usb-check", total)
			if err != nil {
				return err
			}
			defer cancel()
			return runUSBCheck(ctx, t.client, o, out, cmd.ErrOrStderr())
		},
	}
	f := cmd.Flags()
	f.IntVar(&o.count, "count", 0, "enumerate (default 10) and reset (default 20): cycles, 1 to 200; replug (default 10): cycles per length, 1 to 100")
	f.IntSliceVar(&offMS, "off-ms", nil, "enumerate: how long eFuse 1 stays off, 50 to 10000 (default 500); replug: up to 6 dropout lengths, 1 to 5000 each (default 5,20,100,500)")
	f.IntVar(&o.cycleTO, "cycle-timeout", 0, "enumerate, reset, replug: milliseconds to wait for the board after each cycle, 500 to 30000 (default 5000)")
	f.DurationVar(&duration, "duration", 0, "power (default 3s) and suspend (default 1s): the measuring time, 200ms to 60s")
	f.DurationVar(&settle, "settle", 0, "suspend: the wait after the bus is suspended before measuring, 20ms to 5s (default 200ms)")
	f.StringVar(&o.port, "port", "", "usb_a or usb_c when host mode has to be started (default: usb_a on a pod with the USB-A host port, board v4 or later; else usb_c)")
	f.BoolVar(&o.asJSON, "json", false, "print the pod's final reply as JSON")
	return cmd
}

func validateUSBCheck(o *usbCheckOpts, duration, settle time.Duration, hasCount, hasOff, hasCycleTO, hasDuration, hasSettle bool) error {
	const what = "usb-check"
	o.durationMS = int(duration / time.Millisecond)
	o.settleMS = int(settle / time.Millisecond)
	switch o.action {
	case "status", "abort":
		if hasCount || hasOff || hasCycleTO || hasDuration || hasSettle {
			return usageErrorf("%s %s: no test flags apply", what, o.action)
		}
		return validUSBPort(o.port)
	case "enumerate", "power", "suspend", "reset", "replug":
	default:
		return usageErrorf("%s: use enumerate, power, suspend, reset, replug, status or abort (got %q)", what, o.action)
	}
	type rule struct {
		set   bool
		flag  string
		tests string // the tests it applies to, space separated
	}
	for _, r := range []rule{
		{hasCount, "--count", " enumerate reset replug "},
		{hasOff, "--off-ms", " enumerate replug "},
		{hasCycleTO, "--cycle-timeout", " enumerate reset replug "},
		{hasDuration, "--duration", " power suspend "},
		{hasSettle, "--settle", " suspend "},
	} {
		if r.set && !strings.Contains(r.tests, " "+o.action+" ") {
			return usageErrorf("%s %s: %s does not apply (it is for %s)", what, o.action, r.flag, strings.Join(strings.Fields(r.tests), ", "))
		}
	}
	if hasCount {
		max := 200
		if o.action == "replug" {
			max = 100
		}
		if o.count < 1 || o.count > max {
			return usageErrorf("%s: --count must be 1 to %d for %s (got %d)", what, max, o.action, o.count)
		}
	}
	if hasOff {
		if o.action == "enumerate" && len(o.offMS) != 1 {
			return usageErrorf("%s: enumerate takes one --off-ms value", what)
		}
		if len(o.offMS) == 0 || len(o.offMS) > 6 {
			return usageErrorf("%s: --off-ms takes 1 to 6 values", what)
		}
		lo, hi := 1, 5000
		if o.action == "enumerate" {
			lo, hi = 50, 10000
		}
		for _, v := range o.offMS {
			if v < lo || v > hi {
				return usageErrorf("%s: --off-ms for %s must be %d to %d (got %d)", what, o.action, lo, hi, v)
			}
		}
	}
	if hasCycleTO && (o.cycleTO < 500 || o.cycleTO > 30000) {
		return usageErrorf("%s: --cycle-timeout must be 500 to 30000 ms (got %d)", what, o.cycleTO)
	}
	if hasDuration && (o.durationMS < 200 || o.durationMS > 60000) {
		return usageErrorf("%s: --duration must be 200ms to 60s (got %s)", what, duration)
	}
	if hasSettle && (o.settleMS < 20 || o.settleMS > 5000) {
		return usageErrorf("%s: --settle must be 20ms to 5s (got %s)", what, settle)
	}
	return validUSBPort(o.port)
}

func validUSBPort(port string) error {
	if p := strings.ToLower(strings.TrimSpace(port)); p != "" && p != "usb_a" && p != "usb_c" {
		return usageErrorf("--port must be usb_a or usb_c (got %q)", port)
	}
	return nil
}

func usbCheckRequest(o usbCheckOpts) map[string]any {
	req := map[string]any{"cmd": "usb_check", "test": o.action}
	if o.count > 0 {
		req["count"] = o.count
	}
	switch {
	case len(o.offMS) == 1 && o.action == "enumerate":
		req["off_ms"] = o.offMS[0]
	case len(o.offMS) > 0:
		req["off_ms"] = o.offMS
	}
	if o.cycleTO > 0 {
		req["timeout_ms"] = o.cycleTO
	}
	if o.durationMS > 0 {
		req["duration_ms"] = o.durationMS
	}
	if o.settleMS > 0 {
		req["settle_ms"] = o.settleMS
	}
	return req
}

func usbCheckRead(ctx context.Context, c podCommander, req map[string]any) (*usbCheckStatus, json.RawMessage, error) {
	raw, err := c.Command(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	var st usbCheckStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, nil, fmt.Errorf("unexpected usb_check reply %s", strings.TrimSpace(string(raw)))
	}
	st.decode()
	return &st, raw, nil
}

func runUSBCheck(ctx context.Context, pod podCommander, o usbCheckOpts, out, progress io.Writer) error {
	const what = "usb-check"
	poll := o.pollPeriod
	if poll <= 0 {
		poll = usbHostPoll
	}
	say := func(format string, a ...any) { fmt.Fprintf(progress, "usb-check: "+format+"\n", a...) }
	info, err := requireUSBFeatures(ctx, pod, what, "usb_host_mode", "usb_check")
	if err != nil {
		return err
	}
	state := map[string]any{"cmd": "usb_check"}
	switch o.action {
	case "status", "abort":
		if o.action == "abort" {
			state["abort"] = true
		}
		st, raw, err := usbCheckRead(ctx, pod, state)
		if err != nil {
			return usbCheckErr(what, err)
		}
		if o.asJSON {
			fmt.Fprintln(out, strings.TrimSpace(string(raw)))
			return nil
		}
		printUSBCheck(out, st)
		return nil
	}

	// Host mode, and for every test but enumerate a board on the bus.
	host, err := usbEnsureHost(ctx, pod, what, info, o.port, o.hostSecs, int(usbCheckDefaultTimeout/time.Second)+60, poll, say)
	if err != nil {
		return err
	}
	if o.action != "enumerate" {
		if _, err := usbWaitDevice(ctx, pod, host, poll, say); err != nil {
			return usbCheckErr(what, err)
		}
	}

	if _, err := pod.Command(ctx, usbCheckRequest(o)); err != nil {
		return usbCheckErr(what, err)
	}
	st, raw, err := usbCheckWait(ctx, pod, poll, say)
	if err != nil {
		return usbCheckErr(what, err)
	}
	if o.asJSON {
		fmt.Fprintln(out, strings.TrimSpace(string(raw)))
	} else {
		printUSBCheck(out, st)
	}
	if !st.Pass {
		if st.Error != "" {
			return fmt.Errorf("%s: the %s check stopped: %s", what, st.Test, usbCheckErrorText(st.Error))
		}
		return fmt.Errorf("%s: the %s check failed", what, st.Test)
	}
	return nil
}

// usbCheckWait polls usb_check until the check has ended and returns its final reply. Progress is
// printed when the phase changes and at each 10 percent. On cancel it aborts the check.
func usbCheckWait(ctx context.Context, c podCommander, poll time.Duration, say func(string, ...any)) (*usbCheckStatus, json.RawMessage, error) {
	lastPhase, lastPct := "", -1
	abort := func() (*usbCheckStatus, json.RawMessage, error) {
		actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = c.Command(actx, map[string]any{"cmd": "usb_check", "abort": true})
		cancel()
		return nil, nil, fmt.Errorf("check: %w (the check was aborted)", ctx.Err())
	}
	for {
		select {
		case <-ctx.Done():
			return abort()
		case <-time.After(poll):
		}
		st, raw, err := usbCheckRead(ctx, c, map[string]any{"cmd": "usb_check"})
		if err != nil {
			if ctx.Err() != nil { // cancelled during the poll: still abort the check on the pod
				return abort()
			}
			return nil, nil, err
		}
		if !st.Running && st.HaveResult {
			return st, raw, nil
		}
		pct := -1
		if st.Total > 0 {
			pct = st.Done * 100 / st.Total / 10 * 10
		}
		if st.Phase != lastPhase || pct != lastPct {
			lastPhase, lastPct = st.Phase, pct
			if st.Total > 0 {
				say("%s: %d of %d", orDash(st.Phase), st.Done, st.Total)
			} else {
				say("%s", orDash(st.Phase))
			}
		}
	}
}

// usbCheckErr renders a pod refusal (the catalog's fix) or passes other errors through.
func usbCheckErr(what string, err error) error { return usbHostErr(what, err) }

func usbCheckErrorText(code string) string {
	switch code {
	case "aborted":
		return "aborted"
	case "host_stopped":
		return "USB host mode ended during the check (its timeout ran out, or `usb-host off`)"
	case "not_powered_by_efuse1":
		return "the board stayed on the bus with eFuse 1 off: its USB power (VBUS) is not wired to eFuse 1"
	case "no_device":
		return "no board on the bus"
	case "monitor_error", "efuse_error":
		return "the pod could not read or switch eFuse 1 (" + code + ")"
	case "bus_busy", "bus_timeout":
		return "the USB bus did not respond (" + code + ")"
	}
	return code
}

func printUSBCheck(out io.Writer, st *usbCheckStatus) {
	if st.Test == "" {
		fmt.Fprintln(out, "No USB check has run yet.")
		return
	}
	if st.Running {
		fmt.Fprintf(out, "USB check %s: running, phase %s, %d of %d, %.1f s\n", st.Test, orDash(st.Phase), st.Done, st.Total, float64(st.MS)/1000)
		return
	}
	r := &st.Result
	verdict := yesNo(st.Pass, "PASS", "FAIL")
	fmt.Fprintf(out, "USB check %s: %s (%.1f s, board %04X:%04X)\n", st.Test, verdict, float64(st.MS)/1000, r.VID, r.PID)
	if st.Error != "" {
		fmt.Fprintf(out, "  Stopped:     %s\n", usbCheckErrorText(st.Error))
	}
	stat := func(label string, s *usbCheckStat) {
		if s == nil {
			fmt.Fprintf(out, "  %-12s no sample\n", label+":")
			return
		}
		fmt.Fprintf(out, "  %-12s min %d, avg %d, max %d ms\n", label+":", s.Min, s.Avg, s.Max)
	}
	switch st.Test {
	case "enumerate":
		fmt.Fprintf(out, "  Cycles:      %d, %d ok (power off %d ms), %d without pull-up, %d stuck, %d timed out\n", r.Cycles, r.OK, r.OffMS, r.NoConnect, r.Stuck, r.Timeout)
		stat("Pull-up", r.ConnectMS)
		stat("Descriptor", r.DescriptorMS)
		stat("Mounted", r.MountMS)
	case "power":
		fmt.Fprintf(out, "  Current:     avg %s, min %s, peak %s (%d samples)\n", mA(r.AvgUA), mA(r.MinUA), mA(r.PeakUA), r.Samples)
		fmt.Fprintf(out, "  Declared:    %d mA (bMaxPower), verdict %s\n", r.LimitMA, r.Verdict)
	case "suspend":
		cur := r.Current
		if r.SelfPowered {
			cur += " (self-powered board)"
		}
		fmt.Fprintf(out, "  Suspended:   %s, current avg %s against %s: %s\n", yesNo(r.SuspendOK, "bus suspended", "the pod could not suspend the bus"), mA(r.AvgUA), mA(r.LimitUA), cur)
		fmt.Fprintf(out, "  Resume:      %s, board answers: %s\n", r.Resume, yesNo(r.Answers, "yes", "no"))
		if r.Resume == "failed" {
			fmt.Fprintf(out, "  Recovery:    board back after a bus reset: %s\n", yesNo(r.RecoveredByReset, "yes", "no"))
		}
		if r.PortReinit {
			fmt.Fprintf(out, "  Port:        the pod re-initialized the port; the board enumerated and answered afterwards: %s\n", yesNo(r.ReinitOK, "yes", "no"))
		}
	case "reset":
		fmt.Fprintf(out, "  Cycles:      %d, %d ok, %d stuck, %d timed out, %d came back with another identity\n", r.Cycles, r.OK, r.Stuck, r.Timeout, r.IdentityChanged)
		stat("Re-enumerate", r.ReenumerateMS)
	case "replug":
		fmt.Fprintf(out, "  Dropouts:    %d cycle(s) per length\n", r.Count)
		for _, l := range r.Lengths {
			fmt.Fprintf(out, "  %6d ms:    %d re-enumerated, %d stayed on the bus, %d failed\n", l.OffMS, l.Reenumerate, l.Stayed, l.Failed)
		}
	}
}

// mA formats a current given in uA.
func mA(ua int) string { return fmt.Sprintf("%.2f mA", float64(ua)/1000) }
