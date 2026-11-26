package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── benchpod ci: the run ────────────────────────────────────────────────────
//
// One run: build and hash the firmware, connect, take the lease, check `require`, record the
// inputs, run the steps (native ones here, pytest and run as child processes), clean up, check
// the run's health, write run.json and junit.xml, print the summary. The pod comes in through
// connect, so tests drive the whole run with a fake pod.

// ciOptions are the flags of `benchpod ci run`.
type ciOptions struct {
	file      string
	pod       string
	firmware  string
	steps     []string
	out       string
	noUpload  bool
	keepGoing bool
	repeat    int
	json      bool
	wait      time.Duration
}

// ciConnected is a connected pod with how to hand it to child processes.
type ciConnected struct {
	pod    ciPod
	target *ciPodTarget
	conn   string // BENCHPOD_CONNECTION for children
	hwAddr string // BENCHPOD_HW_ADDR, "" off the network
}

type ciRunner struct {
	cfg   *ci.Config
	opts  ciOptions
	steps []*ci.Step

	// connect reaches the pod; takeLease takes its lease. Variables of the runner so tests fake them.
	connect   func(ctx context.Context) (*ciConnected, error)
	takeLease func(ctx context.Context, c *ciConnected, join *tcpclient.LeaseJoin) (ciLease, error)
	// signedIn says whether a credential for embeddedci.com exists; upload sends the run folder.
	signedIn func(ctx context.Context) (bool, error)
	upload   func(ctx context.Context, dir string) (*ci.Uploaded, error)
	// compare asks embeddedci.com to diff an uploaded run with another (with: baseline:<branch>).
	compare func(ctx context.Context, id, with string) (json.RawMessage, error)

	stdout, stderr io.Writer

	dir     string // the run folder (absolute)
	t0      time.Time
	run     *ci.Run
	rec     *ci.Recorder
	conn    *ciConnected
	pod     ciPod // traced
	lease   ciLease
	wiring  *ciWiring
	fw      ciFlash
	started ciPodStatus

	requiredFailed string // the id of the required step that failed, "" when none
	stopReason     string // why the remaining steps were not run
	podDriven      bool   // a step touched the pod: cleanup runs
}

// errLeaseLost is the cause of a step stopped because the run's lease was lost.
var errLeaseLost = errors.New("the run's pod lease was lost")

// stoppedWhy says why a step's context ended early, for its case message.
func stoppedWhy(ctx context.Context, lease ciLease) string {
	if errors.Is(context.Cause(ctx), errLeaseLost) && lease != nil {
		return "stopped: " + lease.Lost()
	}
	return "stopped (the run was interrupted)"
}

// ciExit is the exit code of `benchpod ci`, with the message to print ("" when the summary said
// it already).
type ciExit struct {
	code int
	msg  string
	err  error // the error a setup failure came from, when there was one (its CLI exit code may apply)
}

func (e *ciExit) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("benchpod ci: exit %d", e.code)
	}
	return e.msg
}

func setupErr(format string, args ...any) *ciExit {
	e := &ciExit{code: ci.ExitSetup, msg: fmt.Sprintf(format, args...)}
	for _, a := range args {
		if err, ok := a.(error); ok {
			e.err = err
		}
	}
	return e
}

// execute runs the whole run and returns its exit code. ctx ends on Ctrl-C.
func (r *ciRunner) execute(ctx context.Context) *ciExit {
	r.t0 = time.Now()
	r.rec = nil
	dir := r.opts.out
	if dir == "" {
		dir = ci.DefaultRunDir(r.t0, r.cfg.Suite)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return setupErr("run folder %s: %v", dir, err)
	}
	r.dir = abs
	for _, sub := range []string{"logs", "parts"} {
		if err := os.MkdirAll(filepath.Join(r.dir, sub), 0o755); err != nil {
			return setupErr("create the run folder: %v", err)
		}
	}
	r.rec = ci.NewRecorder(r.dir, r.t0, r.cfg.Keep)
	gh, trigger := githubInputs()
	r.run = &ci.Run{
		RunID:   ci.NewRunID(r.t0),
		Key:     ci.Key{Suite: r.cfg.Suite, DUT: r.cfg.DUT, DefinitionSHA256: r.cfg.SHA256},
		Trigger: trigger,
		Started: ci.Timestamp(r.t0),
		Health:  ci.Health{Valid: true},
		Keep:    ci.Keep{Traces: r.cfg.Keep},
		Inputs: ci.Inputs{Tests: gitInfo(r.cfg.Dir), GitHub: gh,
			Tools:  ci.Tools{CLI: version, OS: osName()},
			Wiring: ci.Wiring{Name: r.cfg.Wiring}},
	}
	r.logf("benchpod ci: %s (dut %s), run %s", r.cfg.Suite, r.cfg.DUT, r.run.RunID)
	r.logf("run folder: %s", r.dir)

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	if r.uploadMode() == "true" && r.signedIn != nil {
		if ok, err := r.signedIn(runCtx); !ok {
			why := "it needs a sign-in to embeddedci.com: run `benchpod login` or set BENCHPOD_API_KEY"
			if err != nil {
				why = err.Error()
			}
			return r.finish(setupErr("upload: true, but %s (or pass --no-upload)", why))
		}
	}

	if exit := r.prepareFirmware(runCtx); exit != nil {
		return r.finish(exit)
	}
	c, err := r.connect(runCtx)
	if err != nil {
		var be *ciBusyError
		if errors.As(err, &be) {
			r.run.Health.Invalidate(be.msg)
			return r.finish(&ciExit{code: ci.ExitInvalid, msg: be.msg})
		}
		return r.finish(setupErr("connect to the pod: %v", err))
	}
	r.conn = c
	r.pod = &tracedPod{inner: c.pod, rec: r.rec}
	r.logf("pod: %s (%s, %s)", firstNonEmptyStr(c.target.Name, c.conn), c.pod.Transport(), c.target.Why)

	join := &tcpclient.LeaseJoin{ID: newLeaseID(), Holder: leaseHolder(), Purpose: truncate(r.cfg.Suite, 64), TTL: ciLeaseTTL}
	lease, err := r.takeLease(runCtx, c, join)
	if err != nil {
		var be *ciBusyError
		if errors.As(err, &be) {
			r.run.Health.Invalidate(be.msg)
			return r.finish(&ciExit{code: ci.ExitInvalid, msg: be.msg})
		}
		return r.finish(setupErr("%v", err))
	}
	r.lease = lease
	defer func() {
		if r.lease != nil {
			r.lease.Release(context.Background())
		}
	}()

	st, _, err := readPodStatus(runCtx, c.pod)
	if err != nil {
		return r.finish(setupErr("read the pod's status: %v", err))
	}
	r.started = st
	r.recordPod(runCtx, st)
	if rng := r.cfg.Require.Range; len(rng) > 0 && !rng.Allows(st.Version) {
		msg := fmt.Sprintf("the pod runs firmware %s, which does not meet require.pod_firmware %q: update it with `benchpod flash-self`",
			firstNonEmptyStr(st.Version, "(unknown)"), r.cfg.Require.PodFirmware)
		r.run.Health.Invalidate(msg)
		return r.finish(&ciExit{code: ci.ExitInvalid, msg: msg})
	}
	if exit := r.loadWiring(runCtx); exit != nil {
		return r.finish(exit)
	}

	r.runSteps(runCtx, ctx)
	r.cleanup()
	r.checkHealthAtEnd()
	return r.finish(nil)
}

// prepareFirmware runs firmware.build and hashes the image the flash step will flash.
func (r *ciRunner) prepareFirmware(ctx context.Context) *ciExit {
	fw := r.cfg.Firmware
	override := strings.TrimSpace(r.opts.firmware)
	if fw == nil && override == "" {
		return nil
	}
	flashes := false
	for _, s := range r.steps {
		flashes = flashes || s.Kind == ci.KindFlash
	}
	if fw == nil {
		fw = &ci.Firmware{}
	}
	r.fw = ciFlash{Overrides: fw.Flash}
	switch {
	case strings.HasPrefix(override, "build:"):
		r.fw.BuildID = strings.TrimSpace(strings.TrimPrefix(override, "build:"))
	case override != "":
		abs, err := filepath.Abs(override)
		if err != nil {
			return setupErr("--firmware %s: %v", override, err)
		}
		r.fw.File = abs
	default:
		r.fw.File = r.cfg.Resolve(fw.File)
		r.fw.BuildID, r.fw.Repo, r.fw.Ref, r.fw.Artifact = fw.BuildID, fw.Repo, fw.Ref, fw.Artifact
		if fw.Build != "" && flashes {
			r.logf("firmware: building (%s)", fw.Build)
			log, err := openCappedLog(filepath.Join(r.dir, "logs", "firmware-build.log"))
			if err != nil {
				return setupErr("%v", err)
			}
			res := runChild(ctx, shellCommand(fw.Build), r.cfg.Dir, mergeEnv(os.Environ(), r.cfg.Env), io.MultiWriter(log, r.stderr), io.MultiWriter(log, r.stderr))
			_ = log.Close()
			if res.ExitCode != 0 {
				why := fmt.Sprintf("exit status %d", res.ExitCode)
				if res.Err != nil {
					why = res.Err.Error()
				}
				r.run.Health.Reasons = append(r.run.Health.Reasons, "firmware.build failed")
				return setupErr("firmware.build (%s) failed: %s; its output is in logs/firmware-build.log", fw.Build, why)
			}
		}
	}
	r.run.Inputs.DUTFirmware.BuildID = r.fw.BuildID
	if r.fw.File != "" {
		in, err := firmwareInputs(r.fw.File)
		if err != nil {
			if !flashes {
				return nil
			}
			return setupErr("firmware file: %v", err)
		}
		r.run.Inputs.DUTFirmware = in
	}
	return nil
}

// recordPod fills inputs.pod from the pod's status and calibration.
func (r *ciRunner) recordPod(ctx context.Context, st ciPodStatus) {
	p := &r.run.Inputs.Pod
	p.Name = r.conn.target.Name
	p.Board = st.boardName()
	p.Firmware = st.Version
	p.Gateware = st.Gateware
	p.FeaturesRev = st.FeaturesRev
	p.Connection = r.conn.pod.Transport()
	if raw, err := r.conn.pod.Command(ctx, map[string]any{"cmd": "calibrate"}); err == nil {
		var cal any
		if json.Unmarshal(raw, &cal) == nil {
			p.CalibrationSHA256 = ci.CanonicalSHA256(cal)
		}
	}
}

// ciWiring is the wiring profile the run uses.
type ciWiring struct {
	Name string
	Raw  map[string]any
	Pins podWiring
}

func (w *ciWiring) intKey(key string, def int) int {
	if w == nil || w.Raw == nil {
		return def
	}
	switch v := w.Raw[key].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (w *ciWiring) strKey(key string) string {
	if w == nil || w.Raw == nil {
		return ""
	}
	s, _ := w.Raw[key].(string)
	return s
}

// rail is the profile's target-power eFuse (1 internal, 2 external).
func (w *ciWiring) rail() int {
	if r := w.intKey("efuse", 1); r == 1 || r == 2 {
		return r
	}
	return 1
}

// j1Rail is the profile's target-power rail when it is a J1 supply of the digital board v4
// ("j1_3v3" or "j1_1v8", profile key efuse), else "".
func (w *ciWiring) j1Rail() string {
	if s := w.strKey("efuse"); s == "j1_3v3" || s == "j1_1v8" {
		return s
	}
	return ""
}

// loadWiring reads the YAML's wiring profile (else the pod's default) from the pod.
func (r *ciRunner) loadWiring(ctx context.Context) *ciExit {
	r.wiring = &ciWiring{Name: r.cfg.Wiring}
	l, err := wiringStoreList(ctx, r.conn.pod)
	if err != nil {
		if r.cfg.Wiring != "" {
			r.warnf("wiring: the pod's stored profiles cannot be read (%v); the run goes on without wiring %s", err, r.cfg.Wiring)
		}
		return nil
	}
	name := r.cfg.Wiring
	if name == "" {
		if l.Default == nil {
			return nil
		}
		name = *l.Default
	} else {
		found := false
		var names []string
		for _, p := range l.Profiles {
			names = append(names, p.Name)
			found = found || p.Name == name
		}
		if !found {
			return setupErr("the pod has no wiring profile %q (it has: %s); save it from the web or desktop app, or with `benchpod wiring save`",
				name, firstNonEmptyStr(strings.Join(names, ", "), "none"))
		}
	}
	raw, _, err := wiringGet(ctx, r.conn.pod, name)
	if err != nil {
		return setupErr("read wiring profile %s: %v", name, err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		return setupErr("wiring profile %s does not parse: %v", name, err)
	}
	r.wiring = &ciWiring{Name: name, Raw: snap}
	_ = json.Unmarshal(raw, &r.wiring.Pins)
	r.run.Inputs.Wiring = ci.Wiring{Name: name, Snapshot: snap, SHA256: ci.CanonicalSHA256(snap)}
	return nil
}

// childEnv is the environment of pytest and run steps (spec section 4).
func (r *ciRunner) childEnv(step string) []string {
	set := map[string]string{
		"EMBEDDEDCI_RUN_DIR":     r.dir,
		"EMBEDDEDCI_RUN_ID":      r.run.RunID,
		"EMBEDDEDCI_RUN_STEP":    step,
		"EMBEDDEDCI_RUN_T0":      strconv.FormatFloat(float64(r.t0.UnixNano())/1e9, 'f', 6, 64),
		"EMBEDDEDCI_KEEP_TRACES": r.cfg.Keep,
		"BENCHPOD_CONNECTION":    r.conn.conn,
		"BENCHPOD_HW_ADDR":       r.conn.hwAddr,
		"BENCHPOD_LEASE":         "\x00",
	}
	if r.conn.hwAddr == "" {
		set["BENCHPOD_HW_ADDR"] = "\x00"
	}
	if r.wiring != nil && r.wiring.Name != "" {
		set["BENCHPOD_WIRING_PROFILE"] = r.wiring.Name
	}
	if r.lease != nil && r.lease.Supported() {
		set["BENCHPOD_LEASE"] = r.lease.ID()
	}
	return mergeEnv(os.Environ(), r.cfg.Env, set)
}

// runSteps runs the selected steps, --repeat times.
func (r *ciRunner) runSteps(runCtx, sigCtx context.Context) {
	repeat := max(r.opts.repeat, 1)
	for attempt := 1; attempt <= repeat; attempt++ {
		for _, s := range r.steps {
			if s.Implicit && attempt > 1 {
				continue // the firmware is flashed once
			}
			if r.stopReason != "" {
				r.addCase(ci.Case{ID: s.ID, Step: s.ID, Kind: caseKind(s), Outcome: ci.Skipped, SkipReason: r.stopReason, Attempt: attempt})
				continue
			}
			first := len(r.run.Results.Cases)
			r.runStep(runCtx, s, attempt)
			failing := false
			for _, c := range r.run.Results.Cases[first:] {
				failing = failing || c.Failing()
			}
			if lost := r.leaseLost(); lost != "" && r.stopReason == "" {
				r.stopReason = "not run: " + lost
			}
			switch {
			case sigCtx.Err() != nil:
				r.stopReason = "not run: the run was interrupted"
				r.run.Health.Invalidate("interrupted (Ctrl-C)")
			case runCtx.Err() != nil:
				r.stopReason = fmt.Sprintf("not run: the run's timeout (%s) ended it", r.cfg.Timeout)
			case failing && s.Required:
				if r.requiredFailed == "" {
					r.requiredFailed = s.ID
				}
				if !r.opts.keepGoing {
					r.stopReason = "not run: the required step " + s.ID + " failed"
				}
			}
		}
	}
}

func caseKind(s *ci.Step) string {
	if s.Kind == ci.KindPytest {
		return "pytest"
	}
	return "native"
}

// addCase appends a case and prints its line.
func (r *ciRunner) addCase(c ci.Case) {
	r.run.Results.Cases = append(r.run.Results.Cases, c)
}

// stepTimeout is the step's own timeout, else its kind's default, within the run's deadline.
func stepTimeout(s *ci.Step) time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	switch s.Kind {
	case ci.KindFlash:
		return 10 * time.Minute
	case ci.KindPowerCycle:
		return s.PowerCycle.Timeout + time.Duration(s.PowerCycle.OffMS)*time.Millisecond + 30*time.Second
	case ci.KindUARTExpect:
		return s.UARTExpect.Timeout + 30*time.Second
	case ci.KindMeasurePower:
		return s.MeasurePower.Duration + 30*time.Second
	case ci.KindWait:
		return s.Wait + 5*time.Second
	case ci.KindPytest, ci.KindRun:
		return 0 // the run's timeout
	}
	return time.Minute
}

func (r *ciRunner) runStep(runCtx context.Context, s *ci.Step, attempt int) {
	ctx := runCtx
	if d := stepTimeout(s); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(runCtx, d)
		defer cancel()
	}
	// A lost lease (the run stopped from the web app) stops the step at once: a child step's
	// process group is stopped like at a timeout, its results so far are still read.
	if r.lease != nil {
		var cancel context.CancelCauseFunc
		ctx, cancel = context.WithCancelCause(ctx)
		defer cancel(nil)
		lost := r.lease.LostC()
		stepDone := make(chan struct{})
		defer close(stepDone)
		go func() {
			select {
			case <-lost:
				cancel(errLeaseLost)
			case <-stepDone:
			}
		}()
	}
	label := s.ID
	if attempt > 1 {
		label += fmt.Sprintf(" (attempt %d)", attempt)
	}
	r.logf("step %s: %s", label, describeStep(s))
	first := len(r.run.Results.Cases)
	switch s.Kind {
	case ci.KindPytest:
		r.podDriven = true
		r.pytestStep(ctx, s, attempt)
	case ci.KindRun:
		r.podDriven = true
		r.runCommandStep(ctx, s, attempt)
	default:
		if s.Kind != ci.KindWait {
			r.podDriven = true
		}
		r.addCase(r.nativeStep(ctx, s, attempt))
	}
	for _, c := range r.run.Results.Cases[first:] {
		line := fmt.Sprintf("  %s %s (%s)", c.Outcome, c.ID, fmtMS(c.DurationMS))
		if c.Failing() && c.Message != "" {
			line += ": " + firstLineOf(c.Message)
		}
		if len(r.run.Results.Cases)-first <= 20 || c.Failing() {
			r.logf("%s", line)
		}
	}
	if n := len(r.run.Results.Cases) - first; n > 20 {
		r.logf("  step %s: %d cases", s.ID, n)
	}
}

func describeStep(s *ci.Step) string {
	switch s.Kind {
	case ci.KindPytest:
		return "pytest " + strings.TrimSpace(s.Pytest.Path+" "+strings.Join(s.Pytest.Args, " "))
	case ci.KindRun:
		return "run " + s.Run
	case ci.KindWait:
		return "wait " + s.Wait.String()
	}
	if s.Implicit {
		return "flash the firmware"
	}
	return strings.ReplaceAll(s.Kind, "_", " ")
}

// cleanup puts the pod back in a safe state after the steps, best effort, logged in
// logs/cleanup.log. It runs also after a failure or Ctrl-C.
func (r *ciRunner) cleanup() {
	if !r.podDriven || r.conn == nil || len(r.cfg.Cleanup) == 0 {
		return
	}
	log, err := openCappedLog(filepath.Join(r.dir, "logs", "cleanup.log"))
	if err != nil {
		return
	}
	defer func() { _ = log.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if r.lease != nil {
		r.lease.Verify(ctx)
		if lost := r.lease.Lost(); lost != "" {
			// Best effort without the lease: if someone else took the pod, the pod refuses these.
			r.lease.Drop()
			fmt.Fprintf(log, "the lease was lost (%s): cleaning up without it\n", lost)
		}
	}
	pod := r.conn.pod
	note := func(action string, err error) {
		r.noteLeaseError(err)
		if err != nil {
			fmt.Fprintf(log, "%s: %v\n", action, err)
		} else {
			fmt.Fprintf(log, "%s: ok\n", action)
		}
	}
	for _, a := range r.cfg.Cleanup {
		switch a {
		case ci.CleanupDACStop:
			_, err := pod.Command(ctx, map[string]any{"cmd": "dac_stop"})
			note(a, err)
		case ci.CleanupPowerOff:
			for _, efuse := range []int{1, 2} {
				_, err := pod.Command(ctx, map[string]any{"cmd": "target_power", "efuse": efuse, "enabled": false})
				note(fmt.Sprintf("%s efuse %d", a, efuse), err)
			}
		case ci.CleanupSensorsOff:
			_, err := pod.Command(ctx, map[string]any{"cmd": "sensor_stop", "all": true})
			if tcpclient.IsUnknownCommand(err) {
				err = nil
			}
			note(a, err)
		case ci.CleanupPullsOff:
			for la := 1; la <= 8; la++ {
				_, err := setLAPull(ctx, pod, la, "off")
				note(fmt.Sprintf("%s LA%d", a, la), err)
			}
		}
	}
	r.logf("cleanup: %s (logs/cleanup.log)", strings.Join(r.cfg.Cleanup, ", "))
}

// checkHealthAtEnd marks the run invalid when the pod rebooted, was lost, or the lease was lost.
func (r *ciRunner) checkHealthAtEnd() {
	if r.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, _, err := readPodStatus(ctx, r.conn.pod)
	switch {
	case err != nil:
		r.run.Health.Invalidate("the pod did not answer at the end of the run (" + err.Error() + ")")
	case r.started.uptimeMS() >= 0 && st.uptimeMS() >= 0 && st.uptimeMS() < r.started.uptimeMS():
		r.run.Health.Invalidate("the pod rebooted during the run")
	case r.started.BootID != "" && st.BootID != "" && st.BootID != r.started.BootID:
		r.run.Health.Invalidate("the pod rebooted during the run")
	}
	if r.lease != nil {
		r.lease.Verify(ctx)
	}
	if lost := r.leaseLost(); lost != "" {
		r.run.Health.LeaseLost = true
		r.run.Health.Invalidate(lost)
	}
}

func (r *ciRunner) leaseLost() string {
	if r.lease == nil {
		return ""
	}
	return r.lease.Lost()
}

// noteLeaseError marks the lease lost when a command was refused with "lease lost".
func (r *ciRunner) noteLeaseError(err error) {
	if err == nil || r.lease == nil {
		return
	}
	if pe, ok := tcpclient.AsPodError(err); ok && isLeaseLost(pe) {
		r.lease.MarkLost(leaseLostText(pe))
	}
	var ce *cloudpod.Error
	if errors.As(err, &ce) && ce.Code == "lease_held" {
		r.lease.MarkLost("the lease was lost on embeddedci.com: " + ce.Error())
	}
}

// finish writes run.json and junit.xml, prints the summary and returns the exit code. setup is
// the setup error that ended the run early (nil after the steps ran).
func (r *ciRunner) finish(setup *ciExit) *ciExit {
	if r.run == nil {
		return setup
	}
	end := time.Now()
	r.run.Finished = ci.Timestamp(end)
	r.run.Results.DurationMS = end.Sub(r.t0).Milliseconds()
	if r.rec != nil && r.rec.Truncated() {
		r.run.Health.TraceTruncated = true
	}
	r.run.Finish(r.requiredFailed != "")
	if setup != nil && setup.code == ci.ExitSetup && r.run.Results.Outcome == ci.Passed {
		r.run.Results.Outcome = ci.Errored
		r.run.Health.Reasons = append(r.run.Health.Reasons, setup.msg)
	}
	if err := ci.WriteJSON(filepath.Join(r.dir, "run.json"), r.run); err != nil {
		r.warnf("write run.json: %v", err)
	}
	var junit strings.Builder
	if err := ci.WriteJUnit(&junit, r.cfg.Suite, r.run.Results.Cases, r.run.Results.DurationMS); err == nil {
		if err := ci.WriteFileAtomic(filepath.Join(r.dir, "junit.xml"), []byte(junit.String())); err != nil {
			r.warnf("write junit.xml: %v", err)
		}
	}
	uploadNote := r.uploadRun(setup)
	var baseline *ci.Baseline
	if setup == nil {
		baseline = r.compareBaseline()
	}
	code := ci.ExitCode(r.run.Results.Outcome)
	if code == ci.ExitPassed && baseline != nil && len(baseline.Gated) > 0 {
		code = ci.ExitRegression
	}
	if r.opts.json {
		data, _ := json.MarshalIndent(r.run, "", "  ")
		fmt.Fprintln(r.stdout, string(data))
	} else {
		r.printSummary(setup)
		if uploadNote != "" {
			fmt.Fprintln(r.stdout, uploadNote)
		}
		if h := baseline.Headline(); h != "" {
			fmt.Fprintln(r.stdout, h)
		}
		for _, m := range baseline.GatedList() {
			fmt.Fprintf(r.stdout, "  %s %s drifted %s (fail_on_drift)\n", m.Case, m.Name, changeText(m))
		}
		if code == ci.ExitRegression {
			fmt.Fprintln(r.stdout, "Exit 3: the run passed, but a gated measurement drifted beyond its allowance against the baseline.")
		}
	}
	r.writeStepSummary(baseline)
	if setup != nil {
		return setup
	}
	return &ciExit{code: code}
}

func changeText(m ci.MeasureChange) string {
	if m.Change == nil {
		return fmt.Sprintf("%g -> %g", m.A, m.B)
	}
	return fmt.Sprintf("%+.1f%% (%g -> %g %s)", *m.Change*100, m.A, m.B, m.Unit)
}

// compareBaseline compares the uploaded run with the last valid run of its suite and DUT on the
// file's baseline branch, and picks the gated drifts. nil when the file names no baseline.
func (r *ciRunner) compareBaseline() *ci.Baseline {
	branch := strings.TrimSpace(r.cfg.Baseline)
	if branch == "" {
		return nil
	}
	b := &ci.Baseline{Branch: branch}
	if r.run.Uploaded == nil || r.compare == nil {
		b.Message = "not compared: the run was not uploaded (the compare runs on embeddedci.com)"
		return b
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, err := r.compare(ctx, r.run.Uploaded.ID, "baseline:"+branch)
	if err != nil {
		b.Message = "not compared: " + err.Error()
		r.warnf("baseline %s: %v", branch, err)
		return b
	}
	var d ci.Diff
	if err := json.Unmarshal(raw, &d); err != nil {
		b.Message = "not compared: " + err.Error()
		return b
	}
	if !d.Found {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &m)
		b.Message = firstNonEmptyStr(m.Message, "no earlier valid run on "+branch)
		return b
	}
	b.Found, b.Diff = true, &d
	b.Gated = ci.GateBaseline(r.cfg, &d)
	return b
}

// writeStepSummary appends the run's Markdown summary to $GITHUB_STEP_SUMMARY (GitHub Actions).
func (r *ciRunner) writeStepSummary(b *ci.Baseline) {
	path := strings.TrimSpace(os.Getenv("GITHUB_STEP_SUMMARY"))
	if path == "" {
		return
	}
	url := ""
	if r.run.Uploaded != nil {
		url = r.run.Uploaded.URL
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		r.warnf("step summary: %v", err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(ci.StepSummary(r.run, url, b)); err != nil {
		r.warnf("step summary: %v", err)
	}
}

// uploadMode is auto, true or false: --no-upload, else the file's upload.
func (r *ciRunner) uploadMode() string {
	if r.opts.noUpload {
		return "false"
	}
	return firstNonEmptyStr(r.cfg.Upload, "auto")
}

// uploadRun uploads the finished run folder (upload: auto when signed in, true always) and
// returns the line for the summary. A failed upload is a warning: it never changes the exit code.
func (r *ciRunner) uploadRun(setup *ciExit) string {
	mode := r.uploadMode()
	if mode == "false" || r.upload == nil {
		return ""
	}
	if setup != nil && strings.HasPrefix(setup.msg, "upload: true") {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if mode == "auto" && r.signedIn != nil {
		ok, err := r.signedIn(ctx)
		if err != nil {
			r.warnf("upload: %v; the run stays local (upload it later with `benchpod ci upload %s`)", err, r.dir)
			return ""
		}
		if !ok {
			return "Not uploaded (not signed in to embeddedci.com; `benchpod login`, then `benchpod ci upload <run folder>`)."
		}
	}
	u, err := r.upload(ctx, r.dir)
	if err != nil {
		r.warnf("upload failed: %v; the run stays local (try again with `benchpod ci upload %s`)", err, r.dir)
		return ""
	}
	r.run.Uploaded = u
	return "Run URL: " + u.URL
}

func (r *ciRunner) printSummary(setup *ciExit) {
	w := r.stdout
	run := r.run
	c := run.Results.Counts
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s (dut %s)", run.Key.Suite, run.Key.DUT)
	if p := run.Inputs.Pod; p.Name != "" || p.Connection != "" {
		fmt.Fprintf(w, " on %s", firstNonEmptyStr(p.Name, p.Connection))
	}
	fmt.Fprintln(w)
	parts := []string{fmt.Sprintf("%d passed", c.Passed), fmt.Sprintf("%d failed", c.Failed)}
	if c.Error > 0 {
		parts = append(parts, fmt.Sprintf("%d error", c.Error))
	}
	if c.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", c.Skipped))
	}
	if c.XFailed > 0 {
		parts = append(parts, fmt.Sprintf("%d xfailed", c.XFailed))
	}
	if c.XPassed > 0 {
		parts = append(parts, fmt.Sprintf("%d xpassed", c.XPassed))
	}
	fmt.Fprintf(w, "%s in %s: %s\n", strings.Join(parts, ", "), (time.Duration(run.Results.DurationMS) * time.Millisecond).Round(100*time.Millisecond),
		strings.ToUpper(run.Results.Outcome))
	if failed := run.FailedCases(); len(failed) > 0 {
		fmt.Fprintln(w, "Failed:")
		for i, fc := range failed {
			if i == 30 {
				fmt.Fprintf(w, "  ... and %d more (run.json)\n", len(failed)-30)
				break
			}
			msg := firstLineOf(fc.Message)
			if fc.Attempt > 1 {
				fmt.Fprintf(w, "  %s [attempt %d] (%s): %s\n", fc.ID, fc.Attempt, fc.Outcome, msg)
			} else {
				fmt.Fprintf(w, "  %s (%s): %s\n", fc.ID, fc.Outcome, msg)
			}
		}
	}
	if r.requiredFailed != "" && !r.opts.keepGoing {
		fmt.Fprintf(w, "The required step %s failed; the steps after it were not run.\n", r.requiredFailed)
	}
	if !run.Health.Valid {
		fmt.Fprintf(w, "The run is not valid: %s\n", strings.Join(run.Health.Reasons, "; "))
	}
	if setup != nil && setup.msg != "" {
		fmt.Fprintf(w, "Stopped: %s\n", setup.msg)
	}
	fmt.Fprintf(w, "Run folder: %s\n", r.dir)
}

func (r *ciRunner) logf(format string, args ...any) {
	fmt.Fprintf(r.stderr, "[benchpod ci] "+format+"\n", args...)
}

func (r *ciRunner) warnf(format string, args ...any) {
	fmt.Fprintf(r.stderr, "[benchpod ci] warning: "+format+"\n", args...)
}

func fmtMS(ms float64) string {
	if ms >= 1000 {
		return strconv.FormatFloat(ms/1000, 'f', 1, 64) + " s"
	}
	return strconv.FormatFloat(ms, 'f', 0, 64) + " ms"
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 240 {
		s = s[:240] + "..."
	}
	return s
}
