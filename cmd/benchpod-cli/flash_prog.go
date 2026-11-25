package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/features"
	"github.com/embeddedci-com/benchpod-cli/internal/progdev"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// Chips without SWD, JTAG or a bootloader: `flash --target avr-updi|pic|efm8|attiny-tpi|xmega|msp430`
// has the pod run the chip's own programming protocol on its LA pins (the firmware's prog_flash
// command, feature prog_flash). The pod holds no device tables: it reads the chip's ID and the CLI
// passes the geometry from internal/progdev (the SDK's table), the same way the SDK's
// embeddedci/benchpod/prog.py does. Each region of the image is staged in the pod's PSRAM
// (load_bin), written and read back by the pod in the background, and this polls its status.

const progInfoTimeout = 10 * time.Second

// progPollInterval is how often progWait asks the pod (tests shorten it).
var progPollInterval = 200 * time.Millisecond

// Wiring-profile signal names looked up when a pin flag is not given (case-insensitive), as in
// the SDK; the profile's swd_swclk and swd_swdio are the last fallback.
var (
	progClkSignals = map[string][]string{
		"pic": {"ICSPCLK", "ICSP_CLK", "PGC"}, "c2": {"C2CK"}, "tpi": {"TPICLK", "TPI_CLK"}, "pdi": {"PDI_CLK"},
		"sbw": {"SBWTCK", "SBW_TCK", "TEST"},
	}
	progDataSignals = map[string][]string{
		"updi": {"UPDI"}, "pic": {"ICSPDAT", "ICSP_DAT", "PGD"}, "c2": {"C2D"},
		"tpi": {"TPIDATA", "TPI_DATA"}, "pdi": {"PDI_DATA"},
		"sbw": {"SBWTDIO", "SBW_TDIO", "RST"},
	}
	progResetSignals = []string{"MCLR", "RESET", "NRST", "PROG_RESET"}
)

// progPerByte is the time a write is given per byte beyond the base allowance (the SDK's
// _SECONDS_PER_BYTE): Spy-Bi-Wire moves a 16-bit word in about 4 ms, the others are faster.
func progPerByte(proto string) time.Duration {
	if proto == "sbw" {
		return 20 * time.Millisecond
	}
	return 5 * time.Millisecond
}

// progFlashFlags are the flags of the prog_flash targets (the rest are shared with flash).
type progFlashFlags struct {
	device, erase string
	clk, data     string
	reset         string // nrst or an LA pin
	info          bool
}

func isProgTarget(t string) bool { return progdev.Proto(t) != "" }

func (p *progFlashFlags) signal(w *podWiring, names []string) int {
	for _, s := range names {
		if n, ok := w.channel(s); ok {
			return n
		}
	}
	return 0
}

// progPins resolves the pins (a flag beats a profile signal named for the protocol, which beats
// the profile's SWD pins) and the same in words for messages. nreset/nresetSet are --nreset.
func (p *progFlashFlags) progPins(proto string, w *podWiring, nreset, nresetSet bool) (map[string]any, string, error) {
	pick := func(flag, val string, names []string, role string) (int, error) {
		if strings.TrimSpace(val) != "" {
			n, err := parseLAPin(val)
			if err != nil {
				return 0, usageErrorf("--%s: %w", flag, err)
			}
			return n, nil
		}
		if n := p.signal(w, names); n != 0 {
			return n, nil
		}
		if n, ok := w.channel(role); ok {
			return n, nil
		}
		return 0, usageErrorf("--%s is required: the LA pin on the chip's %s line, or a wiring-profile signal named %s",
			flag, flag, strings.Join(names, ", "))
	}
	pins := map[string]any{}
	data, err := pick("data", p.data, progDataSignals[proto], "swd_swdio")
	if err != nil {
		return nil, "", err
	}
	pins["data"] = data
	text := []string{fmt.Sprintf("data on LA%d", data)}
	if proto == "updi" {
		if strings.TrimSpace(p.clk) != "" {
			return nil, "", usageErrorf("avr-updi uses one pin: pass --data, not --clk")
		}
	} else {
		clk, err := pick("clk", p.clk, progClkSignals[proto], "swd_swclk")
		if err != nil {
			return nil, "", err
		}
		if clk == data {
			return nil, "", usageErrorf("--clk and --data must be different LA pins")
		}
		pins["clk"] = clk
		text = append([]string{fmt.Sprintf("clk on LA%d", clk)}, text...)
	}
	if proto == "pic" || proto == "tpi" {
		r := strings.ToLower(strings.TrimSpace(p.reset))
		sig := p.signal(w, progResetSignals)
		switch {
		case r != "" && r != "nrst":
			n, err := parseLAPin(r)
			if err != nil {
				return nil, "", usageErrorf("--reset: use nrst or an LA pin: %w", err)
			}
			pins["reset"] = n
		case r == "nrst" || (nresetSet && nreset):
			pins["reset"] = "nrst"
		case (nresetSet && !nreset) || sig != 0:
			if sig == 0 {
				return nil, "", usageErrorf("the chip's reset needs the pod's reset pin (--nreset) or an LA pin (--reset, or a wiring-profile signal named MCLR or RESET)")
			}
			pins["reset"] = sig
		default:
			pins["reset"] = "nrst"
		}
		if pins["reset"] == pins["data"] || pins["reset"] == pins["clk"] {
			return nil, "", usageErrorf("--reset must be another pin than --clk and --data")
		}
		if pins["reset"] == "nrst" {
			text = append(text, "reset on the pod's reset pin")
		} else {
			text = append(text, fmt.Sprintf("reset on LA%d", pins["reset"]))
		}
	} else if strings.TrimSpace(p.reset) != "" {
		return nil, "", usageErrorf("--reset applies to --target pic and attiny-tpi only")
	}
	return pins, strings.Join(text, ", "), nil
}

type progStatus struct {
	Running bool    `json:"running"`
	Phase   string  `json:"phase"`
	Done    int     `json:"done"`
	Total   int     `json:"total"`
	ID      string  `json:"id"`
	AtAddr  *int    `json:"at_addr"`
	Result  *string `json:"result"`
}

func (s *progStatus) result() string {
	if s.Result == nil {
		return ""
	}
	return *s.Result
}

// progFixes is what each result of the pod's status means for the user, by the start of the pod's
// text (the SDK's _FIXES).
var progFixes = []struct{ prefix, fix string }{
	{"no answer", "the chip did not answer. Check the wiring ({pins}), the chip's power with a common ground, that the pod's I/O voltage ({volts}) is the chip's, and that the chip's programming pin is not disabled by its fuses or program"},
	{"wrong chip", "a different chip than the one expected answered (ID {id}); check the board, or pass --device for the chip that is there"},
	{"locked", "the chip is locked: it refuses reads and writes until its memory is erased. Flash with --erase chip (that erases the chip, including the lock) or erase it with the vendor's tool"},
	{"verify failed", "a page read back different from what was written (at {at}). Check the supply voltage and the wiring, lower the load on the pins, and try again; a worn or write-protected region fails like this too"},
	{"timeout", "the chip stayed busy: its supply is too low for erase and write (see the chip's minimum VDD), or the chip is damaged"},
	{"protocol error", "the chip's answer broke the protocol: check the wiring, long wires or a pull-up that is too weak (keep the lines short), the I/O voltage and the chip's supply"},
	{"memory, address", "the pod refused this memory, address, length or page size for the chip: the device table and the image disagree with the chip's firmware options; pass --device for the exact chip"},
	{"could not read the staged", "the pod could not read the image it was given: try again, and report it if it repeats"},
	{"not supported", "this chip, memory or protocol is not supported by the pod's firmware; update the pod (`benchpod flash-self`), or check that the chip is one the table lists"},
	{"aborted", "the write was aborted"},
}

// progResultError is the error for a failed result of the pod's status; what is "" for an info.
func progResultError(proto, what string, st *progStatus, pins, volts, id string) error {
	res := st.result()
	fix := ""
	for _, f := range progFixes {
		if strings.HasPrefix(res, f.prefix) {
			fix = f.fix
			break
		}
	}
	at := "an unknown address"
	if st.AtAddr != nil {
		at = fmt.Sprintf("0x%x", *st.AtAddr)
	}
	if id == "" {
		id = "unknown"
	}
	fix = strings.NewReplacer("{pins}", pins, "{volts}", volts, "{id}", id, "{at}", at).Replace(fix)
	msg := fmt.Sprintf("%s flash failed%s: %s", proto, what, res)
	if st.Phase != "" {
		msg += " (phase " + st.Phase + ")"
	}
	if fix != "" {
		msg += "\n" + fix
	}
	return fmt.Errorf("%s", msg)
}

// progDirectionError turns BP0034 (the pod refuses to drive a pin the wiring marks as a DUT
// output) into words that say what to do.
func progDirectionError(err error, pins string) error {
	pe, ok := tcpclient.AsPodError(err)
	if !ok || pe.Code != "BP0034" {
		return err
	}
	la := "?"
	switch v := pe.Args["la"].(type) {
	case float64:
		la = strconv.Itoa(int(v))
	case string:
		la = v
	}
	return fmt.Errorf("flash: the pod refused to drive LA%s: the wiring profile marks it as an output of the chip (%s), and a programming pin is driven by the pod. "+
		"If the pin is wired to the chip's programming pin, mark it bidirectional (`benchpod la dir ... bidir`, or the profile's dut_dirs); otherwise fix the wiring (--clk, --data, --reset). "+
		"The CLI does not override pin directions", la, pins)
}

// progCommand runs one prog_flash command, with the refusals worth explaining in words.
func progCommand(ctx context.Context, c podCommander, req map[string]any, pins string) (json.RawMessage, error) {
	raw, err := c.Command(ctx, req)
	if err == nil {
		return raw, nil
	}
	if isUnknownCommand(err) {
		return nil, fmt.Errorf("flash: this pod's firmware has no prog_flash command: update it with `benchpod flash-self`, then `benchpod install-blobs`")
	}
	if e := progDirectionError(err, pins); e != err {
		return nil, e
	}
	msg := err.Error()
	if strings.Contains(msg, "xip_prog") || strings.Contains(msg, "install-blobs") || strings.Contains(msg, "install the blobs") {
		return nil, fmt.Errorf("flash: %w (the chip protocols live in the xip_prog blob: run `benchpod install-blobs`)", err)
	}
	return nil, fmt.Errorf("flash: %w", err)
}

// progWait polls prog_flash status until the job ends or timeout passes (then aborts it).
func progWait(ctx context.Context, c podCommander, what string, timeout time.Duration, pins string) (*progStatus, error) {
	lastPct := -1
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			_, _ = c.Command(context.Background(), map[string]any{"cmd": "prog_flash", "op": "abort"})
			return nil, fmt.Errorf("flash: %s: %w (the job was aborted)", what, ctx.Err())
		case <-time.After(progPollInterval):
		}
		raw, err := progCommand(ctx, c, map[string]any{"cmd": "prog_flash", "op": "status"}, pins)
		if err != nil {
			return nil, err
		}
		var st progStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("flash: status: %w", err)
		}
		if !st.Running {
			return &st, nil
		}
		if time.Now().After(deadline) {
			_, _ = c.Command(context.Background(), map[string]any{"cmd": "prog_flash", "op": "abort"})
			return nil, fmt.Errorf("flash: %s did not finish in time (stopped in phase %s): check the chip's supply and the wiring", what, st.Phase)
		}
		if st.Total > 0 {
			if pct := st.Done * 100 / st.Total / 10 * 10; pct != lastPct {
				lastPct = pct
				log.Printf("flash: %s %d%% (%s)", what, pct, st.Phase)
			}
		}
	}
}

// progNamed reports whether the pod takes the chip's options as named fields (feature prog_named,
// firmware 4.0.6 and later); an older pod, or a status that does not say, gets the packed flags.
func progNamed(ctx context.Context, c podCommander) bool {
	raw, err := c.Command(ctx, features.StatusRequest())
	if err != nil {
		return false
	}
	ok, err := features.Has(raw, "prog_named")
	return err == nil && ok
}

// progReadInfo enters programming mode and reads the chip's ID (nothing is written): prog_flash
// op info with the chip's fields (progdev.Device.Fields or an entry style's) when known, then
// status until the job ends.
func progReadInfo(ctx context.Context, c podCommander, proto string, pins, fields map[string]any, pinText string) (*progStatus, error) {
	req := map[string]any{"cmd": "prog_flash", "op": "info", "proto": proto}
	for k, v := range pins {
		req[k] = v
	}
	for k, v := range fields {
		req[k] = v
	}
	if _, err := progCommand(ctx, c, req, pinText); err != nil {
		return nil, err
	}
	return progWait(ctx, c, "reading the chip ID", progInfoTimeout, pinText)
}

// progIdentify reads the chip's ID and finds it in the table (the SDK's prog.identify). A PIC
// answers only in its own entry style (flags): a named chip sends its own; without a name every
// style of the table is tried until one answers with an ID in the table, and when the chip found
// uses other flags than the style that answered, the ID is read again with its own. Other
// protocols read the ID once.
//
// It returns the last status (its result is not "ok" when nothing answered), the chip (nil when
// the ID is not in the table, findErr says why) and err for a failed command.
func progIdentify(ctx context.Context, c podCommander, proto string, pins map[string]any, name, pinText string, named bool) (st *progStatus, dev *progdev.Device, findErr, err error) {
	if proto != "pic" || name != "" {
		var fields map[string]any
		if proto == "pic" {
			d, ferr := progdev.FindDevice(proto, "", name)
			if ferr != nil {
				return nil, nil, ferr, nil
			}
			fields = d.Fields(named)
		}
		if st, err = progReadInfo(ctx, c, proto, pins, fields, pinText); err != nil || st.result() != "ok" {
			return st, nil, nil, err
		}
		dev, findErr = progdev.FindDevice(proto, st.ID, name)
		return st, dev, findErr, nil
	}
	var first *progStatus
	var firstFind error
	for _, style := range progdev.PICEntryStyles() {
		s, err := progReadInfo(ctx, c, proto, pins, style.Fields(named), pinText)
		if err != nil {
			return nil, nil, nil, err
		}
		if s.result() != "ok" {
			if first == nil {
				first = s
			}
			continue
		}
		d, ferr := progdev.FindDevice(proto, s.ID, "")
		if ferr != nil {
			if firstFind == nil {
				first, firstFind = s, ferr
			}
			continue
		}
		if !style.SameEntry(*d) {
			if s, err = progReadInfo(ctx, c, proto, pins, d.Fields(named), pinText); err != nil || s.result() != "ok" {
				return s, nil, nil, err
			}
			if d, ferr = progdev.FindDevice(proto, s.ID, d.Name); ferr != nil {
				return s, nil, ferr, nil
			}
		}
		return s, d, nil, nil
	}
	return first, nil, firstFind, nil
}

// progLAmV is the pod's LA I/O voltage in millivolts, 0 when it is not set or unknown.
func progLAmV(ctx context.Context, c podCommander) int {
	raw, err := c.Command(ctx, map[string]any{"cmd": "la_voltage"})
	if err != nil {
		return 0
	}
	var r laVoltageReply
	if json.Unmarshal(raw, &r) != nil {
		return 0
	}
	return r.MV
}

// progEnsureDeepImage: the image is staged in PSRAM, which only the deep-replay gateware image
// writes (as for ESP32 flashing): a pod left on another image is switched first (~3 s).
func progEnsureDeepImage(ctx context.Context, c podCommander) {
	raw, err := c.Command(ctx, map[string]any{"cmd": "fpga_image"})
	if err != nil {
		return // an older pod without fpga_image: nothing to switch
	}
	var cur struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &cur) != nil || cur.Name == "" || cur.Name == "deep" {
		return
	}
	log.Printf("flash: switching the FPGA image from %s to deep (about 3 s)", cur.Name)
	_, _ = c.Command(ctx, map[string]any{"cmd": "fpga_image", "image": "deep"})
}

func runFlashProg(g *globalFlags, f *flashFlags, spec ConnSpec) error {
	p := &f.prog
	proto := progdev.Proto(f.target)
	fam := progdev.Fam(proto)
	if spec.IsCloud() {
		return fmt.Errorf("flashing %s from the CLI needs a direct link to the pod (its network address or `usb`); over embeddedci.com use the Python SDK or the MCP server", f.target)
	}
	if !spec.IsNetwork() {
		return fmt.Errorf("flashing %s needs the pod's network: pass its address, or `usb` on firmware 3.9.0+ with USB networking", f.target)
	}
	erase := strings.ToLower(strings.TrimSpace(p.erase))
	if erase == "" {
		erase = "chip"
	}
	if erase != "chip" && erase != "page" && erase != "none" {
		return usageErrorf("--erase must be chip, page or none, got %q", p.erase)
	}
	if !p.info && strings.TrimSpace(f.file) == "" {
		return usageErrorf("%s needs --file: a .hex, .elf or .bin (or pass --info to only read the chip's ID)", f.target)
	}
	if strings.TrimSpace(f.loadAddr) != "" {
		if _, err := strconv.ParseInt(strings.TrimSpace(f.loadAddr), 0, 64); err != nil {
			return usageErrorf("bad --load-address %q", f.loadAddr)
		}
	}
	var segs []progdev.Segment
	if !p.info {
		la := 0
		if s := strings.TrimSpace(f.loadAddr); s != "" {
			v, _ := strconv.ParseInt(s, 0, 64)
			la = int(v)
		}
		data, err := os.ReadFile(strings.TrimSpace(f.file))
		if err != nil {
			return err
		}
		if segs, err = progdev.LoadImage(f.file, data, la); err != nil {
			return err
		}
	}
	pins, pinText, err := p.progPins(proto, loadPodWiring(g, os.Stderr), f.nreset, f.nresetSet)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashTimeout))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()
	c := &tcpclient.Client{Addr: spec.Addr}
	req := func(op string) map[string]any {
		r := map[string]any{"cmd": "prog_flash", "op": op, "proto": proto}
		for k, v := range pins {
			r[k] = v
		}
		return r
	}

	progEnsureDeepImage(ctx, c)
	mv := progLAmV(ctx, c)
	volts := "not set"
	if mv > 0 {
		volts = fmt.Sprintf("%g V", float64(mv)/1000)
	}

	// Enter programming mode and read the chip's ID (nothing is written).
	named := progNamed(ctx, c)
	st, dev, findErr, err := progIdentify(ctx, c, proto, pins, strings.TrimSpace(p.device), pinText, named)
	if err != nil {
		return err
	}
	if st == nil && findErr != nil {
		return usageErrorf("%w", findErr)
	}
	if st.result() != "ok" {
		return progResultError(proto, "", st, pinText, volts, st.ID)
	}
	if findErr != nil {
		return usageErrorf("%w", findErr)
	}
	chipID := st.ID
	fmt.Printf("%s (%s), ID %s\n", dev.Name, fam.Title, chipID)
	checkErase := erase
	if p.info {
		checkErase = "none"
	}
	if err := progdev.CheckVoltage(dev, mv, checkErase); err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	if !p.info {
		if err := progdev.CheckErase(dev, erase); err != nil {
			return fmt.Errorf("flash: %w", err)
		}
	}
	if p.info {
		fmt.Println("info only: nothing was written")
		return nil
	}

	regions, notes, err := progdev.SplitRegions(segs, dev, fam.Ignore)
	if err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	hasFlash := false
	for _, r := range regions {
		hasFlash = hasFlash || r.Mem == "flash"
	}
	if erase == "chip" && !hasFlash {
		erase = "page" // an image without flash must not wipe the chip's program
		notes = append(notes, "the image has no flash: no chip erase, only the pages written are erased")
	}

	t0 := time.Now()
	chipErased := false
	for i, r := range regions {
		rt := time.Now()
		if _, err := c.StagePSRAM(ctx, r.Data); err != nil {
			return fmt.Errorf("flash: uploading the image: %w", err)
		}
		mode := erase
		if erase == "chip" && chipErased {
			mode = "none"
		}
		start := req("start")
		start["mem"], start["addr"], start["len"] = r.Mem, r.Addr, len(r.Data)
		start["page"], start["erase"] = r.Page, mode
		start["run"] = !f.noReset && i == len(regions)-1
		if e := dev.ExpectID(); e != "" {
			start["expect_id"] = e
		}
		for k, v := range dev.Fields(named) {
			start[k] = v
		}
		if _, err := progCommand(ctx, c, start, pinText); err != nil {
			return err
		}
		chipErased = chipErased || mode == "chip"
		what := fmt.Sprintf("%s at 0x%x", r.Mem, r.Addr)
		st, err := progWait(ctx, c, what, 60*time.Second+time.Duration(len(r.Data))*progPerByte(proto), pinText)
		if err != nil {
			return err
		}
		if st.result() != "ok" {
			return progResultError(proto, fmt.Sprintf(" writing %s (%d bytes)", what, len(r.Data)), st, pinText, volts, st.ID)
		}
		fmt.Printf("%s: %d bytes written and verified in %.1f s\n", what, len(r.Data), time.Since(rt).Seconds())
	}
	for _, n := range notes {
		fmt.Println(n)
	}
	tail := "; the chip runs its program"
	if f.noReset {
		tail = "; the chip is held in reset"
	}
	fmt.Printf("%d region(s) in %.1f s%s\n", len(regions), time.Since(t0).Seconds(), tail)
	return nil
}
