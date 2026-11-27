package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/features"
)

// ── native steps ────────────────────────────────────────────────────────────

// checkFailed is a native step whose check did not hold (no UART match, a fault): the case fails.
// Any other error (a refusal, a lost link) is an error of the case.
type checkFailed struct{ msg string }

func (e *checkFailed) Error() string { return e.msg }

func failCheck(format string, args ...any) error {
	return &checkFailed{msg: fmt.Sprintf(format, args...)}
}

// nativeStep runs a step the CLI does itself and returns its case.
func (r *ciRunner) nativeStep(ctx context.Context, s *ci.Step, attempt int) ci.Case {
	c := ci.Case{ID: s.ID, Step: s.ID, Kind: "native", Attempt: attempt}
	logName := s.ID
	if attempt > 1 {
		logName += fmt.Sprintf(".attempt%d", attempt)
	}
	stepLog, err := openCappedLog(filepath.Join(r.dir, "logs", logName+".log"))
	if err != nil {
		stepLog = &cappedLog{}
	}
	defer func() { _ = stepLog.Close() }()

	r.rec.Begin()
	start := time.Now()
	metrics, err := r.doNative(ctx, s, stepLog)
	c.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	var cf *checkFailed
	switch {
	case err == nil:
		c.Outcome = ci.Passed
	case errors.Is(context.Cause(ctx), errLeaseLost):
		c.Outcome = ci.Errored
		c.Message = stoppedWhy(ctx, r.lease)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		c.Outcome = ci.Errored
		c.Message = fmt.Sprintf("timed out after %s: %v", stepTimeout(s), err)
	case errors.As(err, &cf):
		c.Outcome = ci.Failed
		c.Message = cf.msg
	case s.Kind == ci.KindFlash:
		c.Outcome = ci.Failed
		c.Message = withRefusalHint(err).Error()
	default:
		c.Outcome = ci.Errored
		c.Message = withRefusalHint(err).Error()
	}
	r.noteLeaseError(err)
	if err != nil {
		fmt.Fprintf(stepLog, "%s: %s\n", c.Outcome, c.Message)
	}
	c.Measurements = nativeMeasurements(metrics, s.Expect)
	ci.FailOnLimits(&c)
	if c.Failing() {
		c.Log = stepLog.Tail()
	}
	if err := r.rec.End(&c); err != nil {
		r.warnf("write the trace of %s: %v", c.ID, err)
	}
	return c
}

// nativeMeasurements turns a step's metrics into measurements: every metric is recorded, with
// the step's expect limits applied to the ones it names. A step that failed before it measured
// has no metrics, so it records none.
func nativeMeasurements(metrics map[string]float64, expect []ci.Expect) []ci.Measurement {
	var out []ci.Measurement
	byName := map[string]ci.Expect{}
	for _, e := range expect {
		byName[e.Name] = e
	}
	for _, name := range orderedMetricNames(metrics) {
		m := ci.Measurement{Name: name, Value: metrics[name], Unit: ci.MetricUnit(name), Source: "native"}
		if e, ok := byName[name]; ok {
			ci.ApplyExpect(&m, e)
		} else {
			m.Evaluate()
		}
		out = append(out, m)
	}
	return out
}

func orderedMetricNames(m map[string]float64) []string {
	order := []string{"boot_ms", "match_ms", "mean_ma", "peak_ma", "min_ma", "mean_v"}
	var out []string
	for _, n := range order {
		if _, ok := m[n]; ok {
			out = append(out, n)
		}
	}
	for _, n := range sortedStrKeys(toStrMap(m)) {
		found := false
		for _, o := range out {
			found = found || o == n
		}
		if !found {
			out = append(out, n)
		}
	}
	return out
}

func toStrMap(m map[string]float64) map[string]string {
	out := make(map[string]string, len(m))
	for k := range m {
		out[k] = ""
	}
	return out
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *ciRunner) doNative(ctx context.Context, s *ci.Step, w io.Writer) (map[string]float64, error) {
	switch s.Kind {
	case ci.KindWait:
		fmt.Fprintf(w, "wait %s\n", s.Wait)
		return nil, sleepCtx(ctx, s.Wait)
	case ci.KindFlash:
		return nil, r.flashStep(ctx, s, w)
	case ci.KindPowerOn:
		return nil, r.powerOn(ctx, w)
	case ci.KindPowerOff:
		return nil, r.powerOff(ctx, w)
	case ci.KindPowerCycle:
		return r.powerCycle(ctx, s.PowerCycle, w)
	case ci.KindUARTExpect:
		return r.uartExpect(ctx, s.UARTExpect, w)
	case ci.KindMeasurePower:
		return r.measurePower(ctx, s.MeasurePower, w)
	}
	return nil, fmt.Errorf("step kind %s is not supported by this CLI", s.Kind)
}

// flashMu keeps the log redirection of one flash at a time.
var flashMu sync.Mutex

func (r *ciRunner) flashStep(ctx context.Context, s *ci.Step, w io.Writer) error {
	if r.fw.File == "" && r.fw.BuildID == "" && r.fw.Repo == "" && r.fw.Ref == "" {
		return errors.New("no firmware to flash: set firmware.file (or build_id, repo/ref), or pass --firmware")
	}
	fw := r.fw
	if fw.Overrides.TargetPower == 0 {
		// The target must be powered to answer SWD: the profile's supply, as the SDK's flash does.
		if sw := r.wiring.switched(); sw != "" {
			// 3v3 and 1v8 are not flash power options: switch it on here, the flash then finds the target up.
			req := map[string]any{"cmd": "target_power", "enabled": true}
			if err := supplyRequest(req, sw, r.takesSupplyKey(ctx)); err != nil {
				return fmt.Errorf("power on (%s) before the flash: %w", supplyLabel(sw), err)
			}
			if _, err := r.pod.Command(ctx, req); err != nil {
				return fmt.Errorf("power on (%s supply) before the flash: %w", supplyLabel(sw), err)
			}
		} else {
			fw.Overrides.TargetPower = r.wiring.rail()
		}
	}
	fw.Timeout = stepTimeout(s)
	if dl, ok := ctx.Deadline(); ok {
		fw.Timeout = time.Until(dl)
	}
	flashMu.Lock()
	defer flashMu.Unlock()
	// The flash logs through the log package: into the step log as well as the terminal.
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, w))
	defer log.SetOutput(prev)
	what := fw.File
	if what == "" {
		what = "build " + firstNonEmptyStr(fw.BuildID, fw.Repo+"@"+fw.Ref)
	}
	fmt.Fprintf(w, "flash %s\n", what)
	return r.pod.Flash(ctx, fw)
}

// takesSupplyKey says whether the pod's firmware reads the `supply` key (else power commands
// send efuse 1/2); asked once per run.
func (r *ciRunner) takesSupplyKey(ctx context.Context) bool {
	if r.supplyKey == nil {
		raw, err := r.pod.Command(ctx, features.StatusRequest())
		v := err == nil && podTakesSupplyKey(raw)
		r.supplyKey = &v
	}
	return *r.supplyKey
}

// powerRequest is target_power for the profile's supply.
func (r *ciRunner) powerRequest(ctx context.Context, on bool) (map[string]any, error) {
	req := map[string]any{"cmd": "target_power", "enabled": on}
	if err := supplyRequest(req, r.wiring.supply(), r.takesSupplyKey(ctx)); err != nil {
		return nil, err
	}
	return req, nil
}

// powerOn applies the profile's I/O voltage (only while the supply is off), then switches the
// profile's supply on, naming the profile so the pod records it.
func (r *ciRunner) powerOn(ctx context.Context, w io.Writer) error {
	supply := r.wiring.supply()
	if mv := r.wiring.intKey("la_mv", 0); mv > 0 {
		st, _, err := readPodStatus(ctx, r.pod)
		if err != nil {
			return err
		}
		if st.LAVccioMV != mv {
			if supplyIsOn(ctx, r.pod, supply) {
				return fmt.Errorf("the board is powered with I/O at %d mV and the wiring profile wants %d mV: power it off first", st.LAVccioMV, mv)
			}
			if _, err := r.pod.Command(ctx, map[string]any{"cmd": "la_voltage", "mv": mv}); err != nil {
				return fmt.Errorf("set the I/O voltage to %d mV: %w", mv, err)
			}
			fmt.Fprintf(w, "I/O voltage %d mV from the wiring profile\n", mv)
		}
	}
	req, err := r.powerRequest(ctx, true)
	if err != nil {
		return fmt.Errorf("power on (%s): %w", supplyLabel(supply), err)
	}
	if r.wiring.Name != "" && r.wiring.switched() == "" {
		req["profile"] = r.wiring.Name
	}
	if _, err := r.pod.Command(ctx, req); err != nil {
		return fmt.Errorf("power on (%s supply): %w", supplyLabel(supply), err)
	}
	fmt.Fprintf(w, "power on, %s supply\n", supplyLabel(supply))
	return nil
}

func (r *ciRunner) powerOff(ctx context.Context, w io.Writer) error {
	req, err := r.powerRequest(ctx, false)
	if err != nil {
		return fmt.Errorf("power off (%s): %w", supplyLabel(r.wiring.supply()), err)
	}
	if _, err := r.pod.Command(ctx, req); err != nil {
		return fmt.Errorf("power off (%s supply): %w", supplyLabel(r.wiring.supply()), err)
	}
	fmt.Fprintf(w, "power off, %s supply\n", supplyLabel(r.wiring.supply()))
	return nil
}

// uartRequest is uart_proxy_start on the profile's UART.
func (r *ciRunner) uartRequest(ctx context.Context, port ci.UARTPort) (map[string]any, error) {
	if port.IsUSB() {
		return r.usbUARTRequest(ctx, port)
	}
	p := r.wiring.Pins
	if p.UartRx == nil {
		name := firstNonEmptyStr(r.wiring.Name, "(none)")
		return nil, fmt.Errorf("the wiring profile %s sets no uart_rx: the UART steps read the DUT on the profile's uart_rx (and uart_tx)", name)
	}
	req := map[string]any{"cmd": "uart_proxy_start", "rx": *p.UartRx, "baud": r.wiring.intKey("uart_baud", 115200)}
	if p.UartTx != nil {
		req["tx"] = *p.UartTx
	} else {
		req["tx"] = *p.UartRx
	}
	if parity := r.wiring.strKey("uart_parity"); parity != "" && parity != "none" {
		req["parity"] = parity
	}
	if sb := r.wiring.intKey("uart_stop_bits", 1); sb == 2 {
		req["stop_bits"] = 2
	}
	return req, nil
}

// usbUARTRequest is uart_proxy_start on the board's USB serial port ("port":"usb"): the pod hosts
// the board (usb-host on), so no LA pin is involved. The baud rate is the step's, else the profile's.
func (r *ciRunner) usbUARTRequest(ctx context.Context, port ci.UARTPort) (map[string]any, error) {
	raw, err := r.pod.Command(ctx, features.StatusRequest())
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if ok, err := features.Has(raw, "uart_usb"); err != nil || !ok {
		return nil, fmt.Errorf("port: usb needs pod firmware 4.0.3 or later (update the pod with `benchpod flash-self`)")
	}
	baud := port.Baud
	if baud == 0 {
		baud = r.wiring.intKey("uart_baud", 115200)
	}
	req := map[string]any{"cmd": "uart_proxy_start", "port": "usb", "baud": baud}
	if port.DTR != nil {
		req["dtr"] = *port.DTR
	}
	if port.RTS != nil {
		req["rts"] = *port.RTS
	}
	if parity := r.wiring.strKey("uart_parity"); parity != "" && parity != "none" {
		req["parity"] = parity
	}
	if sb := r.wiring.intKey("uart_stop_bits", 1); sb == 2 {
		req["stop_bits"] = 2
	}
	return req, nil
}

func (r *ciRunner) powerCycle(ctx context.Context, pc *ci.PowerCycle, w io.Writer) (map[string]float64, error) {
	if err := r.powerOff(ctx, w); err != nil {
		return nil, err
	}
	if err := sleepCtx(ctx, time.Duration(pc.OffMS)*time.Millisecond); err != nil {
		return nil, err
	}
	if pc.Re == nil {
		return nil, r.powerOn(ctx, w)
	}
	req, err := r.uartRequest(ctx, pc.UART)
	if err != nil {
		return nil, err
	}
	uart, err := r.pod.OpenUART(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open the UART: %w", err)
	}
	defer func() { _ = uart.Close() }()
	reader := startUARTReader(uart, w)
	if err := r.powerOn(ctx, w); err != nil {
		return nil, err
	}
	on := time.Now()
	at, text, ok := reader.waitFor(ctx, pc.Re, pc.Timeout)
	if !ok {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failCheck("no match for /%s/ within %s after power on; the DUT printed: %s", pc.Until, pc.Timeout, tailText(text, 400))
	}
	return map[string]float64{"boot_ms": msSince(on, at)}, nil
}

func (r *ciRunner) uartExpect(ctx context.Context, u *ci.UARTExpect, w io.Writer) (map[string]float64, error) {
	req, err := r.uartRequest(ctx, u.UART)
	if err != nil {
		return nil, err
	}
	uart, err := r.pod.OpenUART(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open the UART: %w", err)
	}
	defer func() { _ = uart.Close() }()
	reader := startUARTReader(uart, w)
	start := time.Now()
	if u.Send != "" {
		if _, err := io.WriteString(uart, u.Send); err != nil {
			return nil, fmt.Errorf("send to the DUT: %w", err)
		}
		fmt.Fprintf(w, "sent %q\n", u.Send)
	}
	at, text, ok := reader.waitFor(ctx, u.Re, u.Timeout)
	if !ok {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failCheck("no match for /%s/ within %s; the DUT printed: %s", u.Until, u.Timeout, tailText(text, 400))
	}
	return map[string]float64{"match_ms": msSince(start, at)}, nil
}

func msSince(from, to time.Time) float64 {
	return float64(to.Sub(from).Microseconds()) / 1000
}

func tailText(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(nothing)"
	}
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return fmt.Sprintf("%q", s)
}

// uartReader collects what the DUT sends, with the time each chunk arrived.
type uartReader struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	marks  []uartMark
	notify chan struct{}
	done   chan struct{}
}

type uartMark struct {
	end int // buf length after the chunk
	at  time.Time
}

func startUARTReader(src io.Reader, log io.Writer) *uartReader {
	u := &uartReader{notify: make(chan struct{}, 1), done: make(chan struct{})}
	go func() {
		defer close(u.done)
		b := make([]byte, 4096)
		for {
			n, err := src.Read(b)
			if n > 0 {
				u.mu.Lock()
				u.buf.Write(b[:n])
				u.marks = append(u.marks, uartMark{end: u.buf.Len(), at: time.Now()})
				u.mu.Unlock()
				_, _ = log.Write(b[:n])
				select {
				case u.notify <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return u
}

// waitFor waits up to timeout for re to match what arrived; at is when the chunk that completed
// the match arrived.
func (u *uartReader) waitFor(ctx context.Context, re *regexp.Regexp, timeout time.Duration) (at time.Time, text string, ok bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		u.mu.Lock()
		text = u.buf.String()
		if loc := re.FindStringIndex(text); loc != nil {
			at = time.Now()
			for _, m := range u.marks {
				if m.end >= loc[1] {
					at = m.at
					break
				}
			}
			u.mu.Unlock()
			return at, text, true
		}
		u.mu.Unlock()
		select {
		case <-ctx.Done():
			return time.Time{}, text, false
		case <-deadline.C:
			u.mu.Lock()
			text = u.buf.String()
			u.mu.Unlock()
			return time.Time{}, text, false
		case <-u.notify:
		case <-u.done:
			u.mu.Lock()
			text = u.buf.String()
			u.mu.Unlock()
			if loc := re.FindStringIndex(text); loc != nil {
				return time.Now(), text, true
			}
			return time.Time{}, text, false
		}
	}
}

// powerStats is the stats object of a power_profile result.
type powerStats struct {
	AvgUA  *float64 `json:"avg_ua"`
	MinUA  *float64 `json:"min_ua"`
	PeakUA *float64 `json:"peak_ua"`
	AvgMV  *float64 `json:"avg_mv"`
	N      int      `json:"n"`
	Fault  bool     `json:"fault"`
}

// findPowerStats finds the stats in a power_profile reply: a "stats" object at the top or in
// "data", or the object itself.
func findPowerStats(raw json.RawMessage) (*powerStats, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil, false
	}
	for _, cand := range []json.RawMessage{top["stats"], nestedStats(top["data"]), raw} {
		if len(cand) == 0 {
			continue
		}
		var st powerStats
		if json.Unmarshal(cand, &st) == nil && st.AvgUA != nil {
			return &st, true
		}
	}
	return nil, false
}

func nestedStats(data json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	if s, ok := m["stats"]; ok {
		return s
	}
	return data
}

func powerProfileOneShot(keyed map[string]any, ms int) map[string]any {
	req := map[string]any{"cmd": "power_profile", "duration_ms": ms, "sample_rate_hz": 500, "keep_samples": 0}
	maps.Copy(req, keyed)
	return req
}

func (r *ciRunner) measurePower(ctx context.Context, m *ci.MeasurePower, w io.Writer) (map[string]float64, error) {
	supply := r.wiring.supply()
	if sw := r.wiring.switched(); sw != "" {
		return nil, fmt.Errorf("measure_power: the profile's supply is %s, which has no current monitor; measure it with an external current monitor, or power the board from the 5 V or external supply", supplyLabel(sw))
	}
	keyed := map[string]any{}
	if err := supplyRequest(keyed, supply, r.takesSupplyKey(ctx)); err != nil {
		return nil, err
	}
	ms := int(m.Duration / time.Millisecond)
	var stats *powerStats
	if r.pod.Transport() == "cloud" {
		// The cloud command channel takes start/stop, not the one-shot (it waits on a stream).
		start := map[string]any{"cmd": "power_profile", "action": "start", "sample_rate_hz": 500, "keep_samples": 0, "max_duration_ms": ms + 10000}
		maps.Copy(start, keyed)
		if _, err := r.pod.Command(ctx, start); err != nil {
			return nil, fmt.Errorf("power_profile start: %w", err)
		}
		if err := sleepCtx(ctx, m.Duration); err != nil {
			return nil, err
		}
		raw, err := r.pod.Command(ctx, map[string]any{"cmd": "power_profile", "action": "stop"})
		if err != nil {
			return nil, fmt.Errorf("power_profile stop: %w", err)
		}
		st, ok := findPowerStats(raw)
		if !ok {
			return nil, fmt.Errorf("power_profile stop: no stats in the reply")
		}
		stats = st
	} else {
		lines, err := r.pod.Chunks(ctx, powerProfileOneShot(keyed, ms))
		if err != nil {
			return nil, fmt.Errorf("power_profile: %w", err)
		}
		for i := len(lines) - 1; i >= 0 && stats == nil; i-- {
			stats, _ = findPowerStats(lines[i])
		}
		if stats == nil {
			return nil, fmt.Errorf("power_profile: no stats in the reply")
		}
	}
	out := map[string]float64{"mean_ma": *stats.AvgUA / 1000}
	if stats.PeakUA != nil {
		out["peak_ma"] = *stats.PeakUA / 1000
	}
	if stats.MinUA != nil {
		out["min_ma"] = *stats.MinUA / 1000
	}
	if stats.AvgMV != nil {
		out["mean_v"] = *stats.AvgMV / 1000
	}
	fmt.Fprintf(w, "%s supply over %s: mean %.3f mA, %d samples\n", supplyLabel(supply), m.Duration, out["mean_ma"], stats.N)
	if stats.Fault {
		return out, failCheck("the %s supply reported a fault during the measurement (an overcurrent or a short)", supplyLabel(supply))
	}
	return out, nil
}

// ── pytest and run steps ────────────────────────────────────────────────────

// pythonCommand is the interpreter of a pytest step as argv ("uv run python" is three words).
func pythonCommand(p *ci.Pytest) []string {
	if p != nil && strings.TrimSpace(p.Python) != "" {
		return strings.Fields(p.Python)
	}
	return []string{"python3"}
}

// checkEmbeddedci checks that `import embeddedci` works in the interpreter, and returns its
// version, or the error with the fix.
func checkEmbeddedci(ctx context.Context, py []string, dir string, env []string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var out, errOut bytes.Buffer
	argv := append(append([]string(nil), py...), "-c",
		"import embeddedci, sys; print(getattr(embeddedci, '__version__', '')); print('%d.%d.%d' % sys.version_info[:3])")
	res := runChild(cctx, argv, dir, env, &out, &errOut)
	if res.ExitCode == 0 {
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		return strings.TrimSpace(lines[0]), nil
	}
	why := strings.TrimSpace(errOut.String())
	if i := strings.LastIndex(why, "\n"); i >= 0 {
		why = why[i+1:]
	}
	if res.Err != nil {
		why = res.Err.Error()
	}
	pyText := strings.Join(py, " ")
	return "", fmt.Errorf("`import embeddedci` failed in %s (%s). Install the BenchPod SDK in that environment: "+
		"`%s -m pip install embeddedci` (with uv: `uv add embeddedci`), or set the step's python: to the interpreter that has it",
		pyText, firstNonEmptyStr(why, fmt.Sprintf("exit status %d", res.ExitCode)), pyText)
}

// pytestStep runs pytest with the run handed over in env, and takes its cases from the plugin's
// parts file, else from its JUnit XML, else from its exit code.
func (r *ciRunner) pytestStep(ctx context.Context, s *ci.Step, attempt int) {
	start := time.Now()
	env := r.childEnv(s.ID)
	py := pythonCommand(s.Pytest)
	stepCase := ci.Case{ID: s.ID, Step: s.ID, Kind: "pytest", Attempt: attempt}
	sdk, err := checkEmbeddedci(ctx, py, r.cfg.Dir, env)
	if err != nil {
		stepCase.Outcome, stepCase.Message = ci.Errored, err.Error()
		stepCase.DurationMS = msSince(start, time.Now())
		r.addCase(stepCase)
		return
	}
	r.run.Inputs.Tools.Merge(ci.Tools{SDK: sdk})

	suffix := ""
	if attempt > 1 {
		suffix = fmt.Sprintf(".attempt%d", attempt)
	}
	junit := filepath.Join(r.dir, "logs", s.ID+suffix+".junit.xml")
	partPath := ci.PartPath(r.dir, s.ID)
	_ = os.Remove(partPath)
	argv := append(append([]string(nil), py...), "-m", "pytest")
	if s.Pytest.Path != "" {
		argv = append(argv, s.Pytest.Path)
	}
	argv = append(argv, s.Pytest.Args...)
	argv = append(argv, "--junitxml", junit)

	stepLog, err := openCappedLog(filepath.Join(r.dir, "logs", s.ID+suffix+".log"))
	if err != nil {
		stepLog = &cappedLog{}
	}
	defer func() { _ = stepLog.Close() }()
	fmt.Fprintf(stepLog, "$ %s\n", strings.Join(argv, " "))
	res := runChild(ctx, argv, r.cfg.Dir, env, io.MultiWriter(stepLog, r.stderr), io.MultiWriter(stepLog, r.stderr))

	first := len(r.run.Results.Cases)
	part, ok, perr := ci.ReadPart(r.dir, s.ID)
	switch {
	case perr != nil:
		r.warnf("step %s: %v", s.ID, perr)
	case ok:
		r.run.MergePart(part, s.ID, attempt)
		if attempt > 1 || r.opts.repeat > 1 {
			_ = os.Rename(partPath, filepath.Join(r.dir, "parts", fmt.Sprintf("%s.attempt%d.json", s.ID, attempt)))
		}
	}
	if !ok {
		if data, err := os.ReadFile(junit); err == nil {
			cases, err := ci.ParseJUnit(data, s.ID, "pytest")
			if err != nil {
				r.warnf("step %s: %v", s.ID, err)
			}
			for i := range cases {
				cases[i].Attempt = attempt
			}
			if len(cases) > 0 {
				r.warnf("step %s: the embeddedci pytest plugin wrote no results (parts/%s.json): the cases come from pytest's JUnit XML, without traces or measure() values; update the SDK (embeddedci) for full results", s.ID, s.ID)
			}
			r.run.Results.Cases = append(r.run.Results.Cases, cases...)
		}
	}
	added := r.run.Results.Cases[first:]
	stepCase.DurationMS = msSince(start, time.Now())
	stepCase.Log = stepLog.Tail()
	anyFailing := false
	for _, c := range added {
		anyFailing = anyFailing || c.Failing()
	}
	switch {
	case res.TimedOut:
		stepCase.Outcome, stepCase.Message = ci.Errored, fmt.Sprintf("pytest timed out after %s and was stopped", res.Duration.Round(time.Second))
	case res.Canceled:
		stepCase.Outcome, stepCase.Message = ci.Errored, "pytest was "+stoppedWhy(ctx, r.lease)
	case res.Err != nil:
		stepCase.Outcome, stepCase.Message = ci.Errored, "pytest did not start: "+res.Err.Error()
	case len(added) == 0 && res.ExitCode == 0:
		stepCase.Outcome, stepCase.Log = ci.Passed, ""
	case len(added) == 0 && res.ExitCode == 5:
		stepCase.Outcome, stepCase.SkipReason, stepCase.Log = ci.Skipped, "pytest collected no tests", ""
	case res.ExitCode == 1 && !anyFailing:
		stepCase.Outcome, stepCase.Message = ci.Failed, "pytest reported failures (exit status 1)"
	case res.ExitCode == 0 || res.ExitCode == 1 || res.ExitCode == 5:
		stepCase.Outcome = "" // the cases say it
	default: // 2 interrupted, 3 internal error, 4 usage error
		stepCase.Outcome, stepCase.Message = ci.Errored, fmt.Sprintf("pytest exited with status %d (%s)", res.ExitCode, pytestExitText(res.ExitCode))
	}
	if stepCase.Outcome != "" {
		r.addCase(stepCase)
	}
	r.applyCaseExpect(s, first)
}

func pytestExitText(code int) string {
	switch code {
	case 2:
		return "interrupted"
	case 3:
		return "internal error"
	case 4:
		return "usage error: check the step's args"
	}
	return "see the step log"
}

// runCommandStep runs a shell command; its cases come from results (JUnit XML, go test -json),
// else from its exit status.
func (r *ciRunner) runCommandStep(ctx context.Context, s *ci.Step, attempt int) {
	start := time.Now()
	suffix := ""
	if attempt > 1 {
		suffix = fmt.Sprintf(".attempt%d", attempt)
	}
	stepLog, err := openCappedLog(filepath.Join(r.dir, "logs", s.ID+suffix+".log"))
	if err != nil {
		stepLog = &cappedLog{}
	}
	defer func() { _ = stepLog.Close() }()
	fmt.Fprintf(stepLog, "$ %s\n", s.Run)
	var stdout bytes.Buffer
	outW := io.MultiWriter(stepLog, r.stderr)
	gotest := s.Results == "gotest-json"
	if gotest {
		outW = io.MultiWriter(stepLog, &limitedBuffer{b: &stdout, max: 256 << 20})
	}
	res := runChild(ctx, shellCommand(s.Run), r.cfg.Dir, r.childEnv(s.ID), outW, io.MultiWriter(stepLog, r.stderr))

	first := len(r.run.Results.Cases)
	stepCase := ci.Case{ID: s.ID, Step: s.ID, Kind: "native", Attempt: attempt, DurationMS: msSince(start, time.Now()), Log: stepLog.Tail()}
	var cases []ci.Case
	resultsMissing := ""
	switch {
	case gotest:
		cases = ci.ParseGoTestJSON(stdout.Bytes(), s.ID)
	case strings.HasPrefix(s.Results, "junit:"):
		path := r.cfg.Resolve(strings.TrimPrefix(s.Results, "junit:"))
		st, err := os.Stat(path)
		switch {
		case err != nil:
			resultsMissing = fmt.Sprintf("the step wrote no %s", path)
		case st.ModTime().Before(start.Add(-2 * time.Second)):
			resultsMissing = fmt.Sprintf("%s is older than this run: the step did not write it", path)
		default:
			data, _ := os.ReadFile(path)
			cases, err = ci.ParseJUnit(data, s.ID, "junit")
			if err != nil {
				resultsMissing = fmt.Sprintf("%s: %v", path, err)
			}
		}
	}
	for i := range cases {
		cases[i].Attempt = attempt
	}
	r.run.Results.Cases = append(r.run.Results.Cases, cases...)
	anyFailing := false
	for _, c := range cases {
		anyFailing = anyFailing || c.Failing()
	}
	switch {
	case res.TimedOut:
		stepCase.Outcome, stepCase.Message = ci.Errored, fmt.Sprintf("timed out after %s and was stopped", res.Duration.Round(time.Second))
	case res.Canceled:
		stepCase.Outcome, stepCase.Message = ci.Errored, stoppedWhy(ctx, r.lease)
	case res.Err != nil:
		stepCase.Outcome, stepCase.Message = ci.Errored, "did not start: "+res.Err.Error()
	case resultsMissing != "":
		stepCase.Outcome, stepCase.Message = ci.Errored, resultsMissing
		if res.ExitCode != 0 {
			stepCase.Message += fmt.Sprintf(" (exit status %d)", res.ExitCode)
		}
	case len(cases) == 0 && res.ExitCode == 0:
		stepCase.Outcome, stepCase.Log = ci.Passed, ""
	case res.ExitCode != 0 && !anyFailing:
		stepCase.Outcome, stepCase.Message = ci.Failed, fmt.Sprintf("exit status %d: %s", res.ExitCode, tailText(stepLog.Tail(), 300))
	}
	if stepCase.Outcome != "" {
		r.addCase(stepCase)
	}
	r.applyCaseExpect(s, first)
}

// applyCaseExpect applies a pytest/run step's expect ("<case id>/<name>") to the cases the step
// added from index first: the limits go on the measurement, and a case that did not record an
// expected measurement fails.
func (r *ciRunner) applyCaseExpect(s *ci.Step, first int) {
	for _, e := range s.Expect {
		i := strings.LastIndex(e.Name, "/")
		caseID, name := e.Name[:i], e.Name[i+1:]
		found := false
		for ci2 := first; ci2 < len(r.run.Results.Cases); ci2++ {
			c := &r.run.Results.Cases[ci2]
			if c.ID != caseID {
				continue
			}
			found = true
			hit := false
			for mi := range c.Measurements {
				if c.Measurements[mi].Name == name {
					ci.ApplyExpect(&c.Measurements[mi], e)
					hit = true
				}
			}
			if !hit && c.Outcome != ci.Skipped {
				if c.Outcome == ci.Passed {
					c.Outcome = ci.Failed
				}
				msg := fmt.Sprintf("expect: the measurement %s was not recorded", name)
				c.Message = strings.TrimSpace(c.Message + "\n" + msg)
			}
			ci.FailOnLimits(c)
		}
		if !found {
			r.warnf("step %s: expect %s: no case %s in this step's results", s.ID, e.Name, caseID)
		}
	}
}

// limitedBuffer keeps at most max bytes.
type limitedBuffer struct {
	b   *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
