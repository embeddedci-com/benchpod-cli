package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/embeddedci-com/benchpod-cli/internal/detect"
	"github.com/embeddedci-com/benchpod-cli/internal/features"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// `benchpod detect`: which chip is wired to the pod. It asks the pod to read the target's IDs the
// ways the wiring profile allows and names the chip from the ID table (internal/detect). The pod
// only reads: SWD and JTAG go through dap_start op "id" (feature dap_id), the pin programmers
// through prog_flash op "info" (the code `flash --target pic|avr-updi|...` uses), an ESP32 through
// esp_flash op "sync", and a USB device through the usb_host report. The probes that program pins
// or reset the chip (the "invasive" ones) are the last four of the list; --no-invasive leaves them out.

const (
	detectTimeout   = 90 * time.Second
	detectNeedsDAP  = "needs pod firmware with dap_id"
	detectNeedsHost = "needs pod firmware 4.0.3 or later (usb_host_mode)"
)

// detectMethods are the probes in the order they run.
var detectMethods = []string{"swd", "jtag", "updi", "pic", "c2", "tpi", "pdi", "sbw", "esp32", "usb"}

func detectInvasive(method string) bool {
	switch method {
	case "swd", "jtag", "usb":
		return false
	}
	return true
}

type detectOpts struct {
	methods    []string // empty = all
	noInvasive bool
	asJSON     bool
}

// detectOutcome is the error of a detect that found nothing (exit code 8) or found the target
// unpowered (exit code 9). The result was printed already.
type detectOutcome struct {
	code int
	msg  string
}

func (e *detectOutcome) Error() string { return e.msg }

func newDetectCmd(g *globalFlags) *cobra.Command {
	var o detectOpts
	cmd := &cobra.Command{
		Use:   "detect",
		Short: "Find out which chip is wired to the pod (SWD, JTAG, pin programmers, ESP32, USB)",
		Long: "Report which chip is wired to the pod. detect reads the target's IDs every way the wiring profile\n" +
			"allows (--wiring-profile) and names the chip from a built-in ID table:\n\n" +
			"  swd    SWD pins swd_swclk and swd_swdio: debug port, core and the vendor's ID register (firmware with dap_id)\n" +
			"  jtag   JTAG pins jtag_tck, jtag_tms, jtag_tdi and jtag_tdo: the IDCODEs of the chain (firmware with dap_id)\n" +
			"  usb    the device on the pod's USB host port, when host mode is already on (detect never turns it on)\n" +
			"  updi, pic, c2, tpi, pdi, sbw   a pin programmer: only when the profile has a signal named for the protocol\n" +
			"         (UPDI; ICSPCLK and ICSPDAT; C2CK and C2D; TPICLK and TPIDATA; PDI_CLK and PDI_DATA; SBWTCK or TEST and SBWTDIO or RST)\n" +
			"  esp32  an ESP32 over its UART ROM loader: needs uart_rx, uart_tx and a BOOT, IO0 or IO9 signal\n\n" +
			"swd, jtag and usb only read. The programmer and ESP32 probes put the chip into programming mode\n" +
			"and reset it; --no-invasive leaves them out. Nothing is probed while target power is off (the\n" +
			"profile's supply), and a pin another function owns (a UART, a GPIO, a sensor) is skipped.\n\n" +
			"Each finding is one line: method, chip (vendor, core), the ID and the confidence: high (an exact\n" +
			"table match), medium (vendor and core, not the part) or low (only a debug port answered).\n" +
			"A method that was not probed is listed with the reason.\n\n" +
			"Exit code 0 when something was found, 8 when nothing was, 9 when target power is off.",
		Example: "  benchpod detect\n" +
			"  benchpod detect --no-invasive\n" +
			"  benchpod detect --methods swd,jtag --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateDetectMethods(o.methods); err != nil {
				return err
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			ctx, cancel, t, err := g.podClient("detect", detectTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			return runDetect(ctx, t.client, loadPodWiring(g, cmd.ErrOrStderr()), o, out)
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&o.methods, "methods", nil, "only these methods, comma separated: "+strings.Join(detectMethods, ", ")+" (default: all the wiring allows)")
	f.BoolVar(&o.noInvasive, "no-invasive", false, "only swd, jtag and usb: do not program pins, talk to an ESP32 ROM loader or reset the chip")
	f.BoolVar(&o.asJSON, "json", false, "print the result as JSON: powered, findings, skipped, notes")
	return cmd
}

func validateDetectMethods(ms []string) error {
	for _, m := range ms {
		ok := false
		for _, k := range detectMethods {
			ok = ok || k == strings.ToLower(strings.TrimSpace(m))
		}
		if !ok {
			return usageErrorf("detect: unknown method %q: use %s", m, strings.Join(detectMethods, ", "))
		}
	}
	return nil
}

func runDetect(ctx context.Context, pod podCommander, w *podWiring, o detectOpts, out io.Writer) error {
	res, err := detectTarget(ctx, pod, w, o)
	if err != nil {
		return err
	}
	if o.asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(out, string(b))
	} else {
		printDetect(out, res)
	}
	switch {
	case !res.Powered:
		return &detectOutcome{exitDetectUnpowered, "detect: target power is off"}
	case len(res.Findings) == 0:
		return &detectOutcome{exitDetectNothing, "detect: nothing was detected"}
	}
	return nil
}

func printDetect(out io.Writer, r *detect.Result) {
	for _, f := range r.Findings {
		var who []string
		for _, s := range []string{f.Vendor, f.Core} {
			if s != "" {
				who = append(who, s)
			}
		}
		line := fmt.Sprintf("%s  %s", f.Method, f.Chip)
		if len(who) > 0 {
			line += " (" + strings.Join(who, ", ") + ")"
		}
		fmt.Fprintf(out, "%s  id %s  %s\n", line, f.ID, f.Confidence)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(out, "skipped %s: %s\n", s.Method, s.Reason)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(out, "note: %s\n", n)
	}
	if r.Powered && len(r.Findings) == 0 {
		fmt.Fprintln(out, "nothing detected")
	}
}

// detector is one run of detect.
type detector struct {
	ctx    context.Context
	pod    podCommander
	w      *podWiring
	o      detectOpts
	status json.RawMessage
	info   podInfo
	owners map[int]string // LA pin -> what owns it
	want   map[string]bool
	res    *detect.Result
}

func (d *detector) has(feature string) bool {
	ok, err := features.Has(d.status, feature)
	return err == nil && ok
}

// skip lists a method as not probed. A method that is simply not wired is only listed when the
// user asked for it (--methods), so the default run does not list the nine methods a bench never wires.
func (d *detector) skip(method, reason string, notWired bool) {
	if notWired && len(d.o.methods) == 0 {
		return
	}
	d.res.Skipped = append(d.res.Skipped, detect.Skipped{Method: method, Reason: reason})
}

func (d *detector) note(format string, a ...any) {
	d.res.Notes = append(d.res.Notes, fmt.Sprintf(format, a...))
}

// failed words a probe's error as a note.
func (d *detector) failed(method string, err error) {
	if pe, ok := tcpclient.AsPodError(err); ok {
		d.note("%s", refusedError(method, pe).Error())
		return
	}
	d.note("%s: %v", method, err)
}

// busyPin returns "LA11 is in use by uart" for the first of the pins that something owns.
func (d *detector) busyPin(names map[string]int) string {
	for _, k := range sortedKeys(names) {
		if who := d.owners[names[k]]; who != "" {
			return fmt.Sprintf("LA%d (%s) is in use by %s", names[k], k, who)
		}
	}
	return ""
}

func sortedKeys(m map[string]int) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := range ks {
		for j := i + 1; j < len(ks); j++ {
			if ks[j] < ks[i] {
				ks[i], ks[j] = ks[j], ks[i]
			}
		}
	}
	return ks
}

func pinInts(pins map[string]any) map[string]int {
	out := map[string]int{}
	for k, v := range pins {
		if n, ok := v.(int); ok && n > 0 {
			out[k] = n
		}
	}
	return out
}

func detectTarget(ctx context.Context, pod podCommander, w *podWiring, o detectOpts) (*detect.Result, error) {
	d := &detector{ctx: ctx, pod: pod, w: w, o: o, want: map[string]bool{},
		res: &detect.Result{Findings: []detect.Finding{}, Skipped: []detect.Skipped{}, Notes: []string{}}}
	if w == nil {
		d.w = &podWiring{}
		d.note("no wiring profile could be read: nothing says which LA pins the target is on")
	}
	for _, m := range o.methods {
		d.want[strings.ToLower(strings.TrimSpace(m))] = true
	}
	raw, err := pod.Command(ctx, features.StatusRequest())
	if err != nil {
		if pe, ok := tcpclient.AsPodError(err); ok {
			return nil, refusedError("detect", pe)
		}
		return nil, fmt.Errorf("detect: status: %w", err)
	}
	d.status = raw
	_ = json.Unmarshal(raw, &d.info)

	powered, err := d.targetPowered()
	if err != nil {
		return nil, err
	}
	d.res.Powered = powered
	if !powered {
		d.note("target power is off: switch it on (the web app, `power_on`, or a `benchpod ci` power step) and run detect again")
		return d.res, nil
	}
	d.loadOwners()

	for _, m := range detectMethods {
		if len(d.want) > 0 && !d.want[m] {
			continue
		}
		if d.o.noInvasive && detectInvasive(m) {
			d.skip(m, "invasive: left out by --no-invasive", true)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("detect: %w", err)
		}
		switch m {
		case "swd":
			d.swd()
		case "jtag":
			d.jtag()
		case "usb":
			d.usb()
		case "esp32":
			d.esp32()
		default:
			d.prog(m)
		}
	}
	return d.res, nil
}

// targetPowered: the profile's supply is on, or (no supply in the profile) the 5 V or the external one is.
func (d *detector) targetPowered() (bool, error) {
	raw, err := d.pod.Command(d.ctx, map[string]any{"cmd": "target_status"})
	if err != nil {
		if pe, ok := tcpclient.AsPodError(err); ok {
			return false, refusedError("detect", pe)
		}
		return false, fmt.Errorf("detect: target_status: %w", err)
	}
	var tp targetPower
	if err := json.Unmarshal(raw, &tp); err != nil {
		return false, fmt.Errorf("detect: unexpected target_status reply %s", strings.TrimSpace(string(raw)))
	}
	if name := d.w.supply(); name != "" {
		s, _ := tp.supplyState(name)
		return bool(s.Enabled), nil
	}
	a, _ := tp.supplyState("5v")
	b, _ := tp.supplyState("external")
	return bool(a.Enabled || b.Enabled), nil
}

func (d *detector) loadOwners() {
	d.owners = map[int]string{}
	raw, err := laPinsFetch(d.ctx, d.pod, nil)
	if err != nil {
		d.note("could not read which function owns each LA pin (la_pins): pins in use are not skipped")
		return
	}
	var rep laPinsReply
	if json.Unmarshal(raw, &rep) != nil {
		return
	}
	for _, p := range rep.Pins {
		if strings.TrimSpace(p.Owner) != "" {
			d.owners[p.LA] = p.Owner
		}
	}
}

func (d *detector) add(f detect.Finding) {
	d.res.Findings = append(d.res.Findings, f)
}

// ── swd and jtag ────────────────────────────────────────────────────────────

// detectAnswerWaitMS is how long the pod retries the first SWD / JTAG read for a target that is
// still booting: a NUCLEO answers SWD about 2 s after its power goes on (its ST-LINK boots first).
// The pod replies as soon as the target answers, so only an empty port costs the full wait.
const detectAnswerWaitMS = 3000

func (d *detector) dapID(req map[string]any, into any) error {
	req["cmd"], req["op"] = "dap_start", "id"
	raw, err := d.pod.Command(d.ctx, req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("unexpected dap_start reply %s", strings.TrimSpace(string(raw)))
	}
	return nil
}

func (d *detector) swd() {
	if d.w.SwdSwclk == nil || d.w.SwdSwdio == nil {
		d.skip("swd", "the wiring profile has no swd_swclk and swd_swdio", true)
		return
	}
	pins := map[string]any{"swclk": *d.w.SwdSwclk, "swdio": *d.w.SwdSwdio}
	if why := d.busyPin(pinInts(pins)); why != "" {
		d.skip("swd", why, false)
		return
	}
	if !d.has("dap_id") {
		d.skip("swd", detectNeedsDAP, false)
		return
	}
	req := func() map[string]any { return map[string]any{"swclk": pins["swclk"], "swdio": pins["swdio"]} }
	var r detect.SWDReply
	first := req()
	first["wait_ms"] = detectAnswerWaitMS
	if err := d.dapID(first, &r); err != nil {
		d.failed("swd", err)
		return
	}
	if !r.Found {
		d.note("swd: nothing answered on SWCLK LA%d and SWDIO LA%d", pins["swclk"], pins["swdio"])
		return
	}
	if reads := detect.SWDReads(detect.Core(deref(r.CPUID))); len(reads) > 0 {
		second := req()
		second["read"] = reads
		var r2 detect.SWDReply
		if err := d.dapID(second, &r2); err != nil {
			d.failed("swd (ID registers)", err)
		} else if r2.Found {
			r = r2
		}
	}
	if f := detect.MapSWD(r, pins); f != nil {
		d.add(*f)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (d *detector) jtag() {
	w := d.w
	if w.JtagTck == nil || w.JtagTms == nil || w.JtagTdi == nil || w.JtagTdo == nil {
		d.skip("jtag", "the wiring profile has no jtag_tck, jtag_tms, jtag_tdi and jtag_tdo", true)
		return
	}
	pins := map[string]any{"tck": *w.JtagTck, "tms": *w.JtagTms, "tdi": *w.JtagTdi, "tdo": *w.JtagTdo}
	trst := 0
	if w.JtagTrst != nil {
		trst = *w.JtagTrst
		pins["trst"] = trst
	}
	if why := d.busyPin(pinInts(pins)); why != "" {
		d.skip("jtag", why, false)
		return
	}
	if !d.has("dap_id") {
		d.skip("jtag", detectNeedsDAP, false)
		return
	}
	req := map[string]any{"tck": pins["tck"], "tms": pins["tms"], "tdi": pins["tdi"], "tdo": pins["tdo"], "trst": trst,
		"wait_ms": detectAnswerWaitMS}
	var r detect.JTAGReply
	if err := d.dapID(req, &r); err != nil {
		d.failed("jtag", err)
		return
	}
	if !r.Found {
		d.note("jtag: no device answered on TCK LA%d, TMS LA%d, TDI LA%d and TDO LA%d", pins["tck"], pins["tms"], pins["tdi"], pins["tdo"])
		return
	}
	for _, f := range detect.MapJTAG(r, pins) {
		d.add(f)
	}
}

// ── usb ─────────────────────────────────────────────────────────────────────

func (d *detector) usb() {
	if !d.has("usb_host_mode") {
		d.skip("usb", detectNeedsHost, false)
		return
	}
	r, _, err := usbHostRead(d.ctx, d.pod)
	if err != nil {
		d.failed("usb", err)
		return
	}
	if r.Mode != "host" {
		d.skip("usb", "USB host mode is off (detect never turns it on: `benchpod usb-host on`)", true)
		return
	}
	dev := r.Device
	if dev == nil || !r.Attached {
		d.skip("usb", "no USB device is mounted on the host port", false)
		return
	}
	f := detect.Finding{Method: "usb", ID: strings.ToLower(dev.ids()), Raw: dev, Pins: map[string]any{"port": r.Port}}
	f.Vendor = strings.TrimSpace(dev.Manufacturer)
	switch name := detect.USBName(dev.VID, dev.PID); {
	case name != "":
		f.Chip, f.Confidence = name, detect.High
	case dev.hasInterface(usbClassApp, 0x01):
		f.Chip, f.Confidence = "USB DFU bootloader", detect.Medium
	default:
		f.Chip, f.Confidence = firstNonEmptyStr(strings.TrimSpace(dev.Product), "USB device"), detect.Low
	}
	d.add(f)
}

// ── pin programmers and ESP32 ───────────────────────────────────────────────

var progVendor = map[string][2]string{
	"updi": {"Microchip", "AVR"}, "tpi": {"Microchip", "AVR"}, "pdi": {"Microchip", "AVR"},
	"pic": {"Microchip", "PIC"}, "c2": {"Silicon Labs", "8051"}, "sbw": {"Texas Instruments", "MSP430"},
}

func (d *detector) prog(proto string) {
	// Only a signal named for the protocol counts: the SWD pins are never a fallback here.
	ws := *d.w
	ws.SwdSwclk, ws.SwdSwdio = nil, nil
	pf := &progFlashFlags{}
	if pf.signal(&ws, progDataSignals[proto]) == 0 {
		d.skip(proto, "the wiring profile has no signal named "+strings.Join(progDataSignals[proto], ", "), true)
		return
	}
	if proto != "updi" && pf.signal(&ws, progClkSignals[proto]) == 0 {
		d.skip(proto, "the wiring profile has no signal named "+strings.Join(progClkSignals[proto], ", "), true)
		return
	}
	pins, pinText, err := pf.progPins(proto, &ws, false, false)
	if err != nil {
		d.skip(proto, err.Error(), false)
		return
	}
	if why := d.busyPin(pinInts(pins)); why != "" {
		d.skip(proto, why, false)
		return
	}
	ctx, pod := d.ctx, d.pod
	if proto == "sbw" && !progNamed(ctx, pod) {
		// Spy-Bi-Wire shipped with firmware 4.0.6, the release that added prog_named.
		d.skip(proto, "needs pod firmware with the sbw protocol (4.0.6 or newer; update the pod)", false)
		return
	}
	progEnsureDeepImage(ctx, pod)
	mv := progLAmV(ctx, pod)
	volts := "not set"
	if mv > 0 {
		volts = fmt.Sprintf("%g V", float64(mv)/1000)
	}
	st, dev, findErr, err := progIdentify(ctx, pod, proto, pins, "", pinText, progNamed(ctx, pod))
	if err != nil {
		if strings.Contains(err.Error(), "no prog_flash command") {
			d.skip(proto, "needs pod firmware with prog_flash", false)
			return
		}
		d.failed(proto, err)
		return
	}
	if st.result() != "ok" {
		if strings.HasPrefix(st.result(), "no answer") {
			d.note("%s: no chip answered (%s)", proto, pinText)
		} else {
			d.note("%s", strings.ReplaceAll(progResultError(proto, "", st, pinText, volts, st.ID).Error(), "\n", ": "))
		}
		return
	}
	vc := progVendor[proto]
	if proto == "sbw" && len(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(st.ID)), "0x")) == 2 {
		// Only the JTAG ID answered: the chip is JTAG-locked and its device ID cannot be read.
		d.add(detect.Finding{Method: proto, Chip: "MSP430 (JTAG-locked)", Vendor: vc[0], Core: vc[1],
			ID: "0x" + strings.TrimPrefix(strings.ToLower(strings.TrimSpace(st.ID)), "0x"), Confidence: detect.Low,
			Pins: pins, Invasive: true, Raw: map[string]any{"proto": proto, "id": st.ID, "locked": true}})
		d.note("sbw: only the JTAG ID answered, so the chip is JTAG-locked; flash with --device MSP430FR2433 (or the chip's name) and --erase chip to erase and unlock it")
		return
	}
	f := detect.Finding{Method: proto, Vendor: vc[0], Core: vc[1], ID: st.ID, Pins: pins, Invasive: true,
		Raw: map[string]any{"proto": proto, "id": st.ID}}
	if dev != nil && findErr == nil {
		f.Chip, f.Confidence = dev.Name, detect.High
	} else {
		f.Chip, f.Confidence = "unknown", detect.Low
		d.note("%s: a chip answered with ID %s, which is not in the programmer table", proto, st.ID)
	}
	d.add(f)
}

func (d *detector) esp32() {
	w := d.w
	if w.UartRx == nil || w.UartTx == nil {
		d.skip("esp32", "the wiring profile has no uart_rx and uart_tx", true)
		return
	}
	f := &flashFlags{}
	pins, err := f.espPins(w)
	if err != nil {
		d.skip("esp32", "the wiring profile has no signal named "+strings.Join(espBootSignals, ", "), true)
		return
	}
	if why := d.busyPin(pinInts(pins)); why != "" {
		d.skip("esp32", why, false)
		return
	}
	req := map[string]any{"cmd": "esp_flash", "op": "sync", "baud": espBaud}
	for k, v := range pins {
		req[k] = v
	}
	if _, err := d.pod.Command(d.ctx, req); err != nil {
		if isUnknownCommand(err) {
			d.skip("esp32", "needs pod firmware with esp_flash", false)
			return
		}
		d.failed("esp32", err)
		return
	}
	for {
		select {
		case <-d.ctx.Done():
			_, _ = d.pod.Command(context.Background(), map[string]any{"cmd": "esp_flash", "op": "abort"})
			d.note("esp32: %v", d.ctx.Err())
			return
		case <-time.After(espPollInterval):
		}
		raw, err := d.pod.Command(d.ctx, map[string]any{"cmd": "esp_flash", "op": "status"})
		if err != nil {
			d.failed("esp32", err)
			return
		}
		var st espStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			d.note("esp32: unexpected esp_flash reply %s", strings.TrimSpace(string(raw)))
			return
		}
		if st.Running {
			continue
		}
		res := ""
		if st.Result != nil {
			res = *st.Result
		}
		if (res == "ok" || res == "unverified") && st.Chip != "" {
			d.add(detect.Finding{Method: "esp32", Chip: st.Chip, Vendor: "Espressif", ID: st.Chip, Raw: json.RawMessage(raw),
				Confidence: detect.High, Pins: pins, Invasive: true})
		} else {
			d.note("esp32: %s", firstNonEmptyStr(res, "no answer from the ROM loader"))
		}
		return
	}
}
