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

	"github.com/embeddedci-com/benchpod-cli/internal/progdev"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// Chips without SWD or a bootloader: `flash --target avr-updi|pic|efm8|attiny-tpi|xmega` has the
// pod run the chip's own programming protocol on its LA pins (the firmware's prog_flash command,
// feature prog_flash). Each region of the image is uploaded into the pod's PSRAM (load_bin), the
// pod writes it in the background and this polls its status. --info only reads the chip ID.

const progPollInterval = 200 * time.Millisecond

// Wiring-profile signal names tried for each pin when the flag is not given (case-insensitive).
var progSignals = map[string]map[string][]string{
	"updi": {"data": {"UPDI", "DATA"}},
	"pic":  {"clk": {"ICSPCLK", "PGC", "CLK"}, "data": {"ICSPDAT", "PGD", "DATA"}, "reset": {"MCLR", "RESET", "NRST"}},
	"c2":   {"clk": {"C2CK", "CLK"}, "data": {"C2D", "DATA"}},
	"tpi":  {"clk": {"TPICLK", "CLK"}, "data": {"TPIDATA", "DATA"}, "reset": {"RESET", "NRST"}},
	"pdi":  {"clk": {"PDI_CLK", "PDICLK", "CLK"}, "data": {"PDI_DATA", "PDIDATA", "DATA"}},
}

// progFlashFlags are the flags of the prog_flash targets (the rest are shared with flash).
type progFlashFlags struct {
	device, mem, erase  string
	clk, data, reset    string
	info                bool
	baud                int
	expectID            string
	noExpectID, noReset bool
}

func isProgTarget(t string) bool { return progdev.Proto(t) != "" }

type progRegion struct {
	addr int
	data []byte
}

// progImages reads --file: Intel HEX (.hex, .ihex; one region from its lowest address, gaps filled with 0xFF)
// or a raw .bin written at --load-address (default 0).
func progImages(file, loadAddr string) ([]progRegion, error) {
	f := strings.TrimSpace(file)
	if f == "" {
		return nil, fmt.Errorf("this target needs --file: an Intel HEX file (.hex) or a raw .bin written at --load-address (default 0)")
	}
	raw, err := os.ReadFile(f)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s is empty", f)
	}
	low := strings.ToLower(f)
	if strings.HasSuffix(low, ".hex") || strings.HasSuffix(low, ".ihex") {
		bin, base, err := parseIntelHex(raw) // gaps are filled with 0xFF, the erased state
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		return []progRegion{{addr: int(base), data: bin}}, nil
	}
	if strings.HasSuffix(low, ".elf") {
		return nil, fmt.Errorf("%s: ELF files are not read here; convert with `objcopy -O ihex` (or -O binary)", f)
	}
	addr := 0
	if s := strings.TrimSpace(loadAddr); s != "" {
		v, err := strconv.ParseInt(s, 0, 32)
		if err != nil || v < 0 {
			return nil, usageErrorf("bad --load-address %q", s)
		}
		addr = int(v)
	}
	return []progRegion{{addr: addr, data: raw}}, nil
}

// progPins resolves --clk/--data/--reset, falling back to wiring-profile signals.
func (p *progFlashFlags) progPins(proto string, w *podWiring) (map[string]any, error) {
	pin := func(flag, val string) (int, error) {
		if strings.TrimSpace(val) != "" {
			n, err := parseLAPin(val)
			if err != nil {
				return 0, usageErrorf("--%s: %w", flag, err)
			}
			return n, nil
		}
		for _, s := range progSignals[proto][flag] {
			if n, ok := w.channel(s); ok {
				return n, nil
			}
		}
		return 0, nil
	}
	pins := map[string]any{}
	data, err := pin("data", p.data)
	if err != nil {
		return nil, err
	}
	if data == 0 {
		return nil, usageErrorf("--data is required: the LA pin on the chip's data line (%s), or a wiring-profile signal named %s",
			progLines[proto], strings.Join(progSignals[proto]["data"], ", "))
	}
	pins["data"] = data
	if proto != "updi" {
		clk, err := pin("clk", p.clk)
		if err != nil {
			return nil, err
		}
		if clk == 0 {
			return nil, usageErrorf("--clk is required: the LA pin on the chip's clock line (%s), or a wiring-profile signal named %s",
				progLines[proto], strings.Join(progSignals[proto]["clk"], ", "))
		}
		pins["clk"] = clk
		if clk == data {
			return nil, usageErrorf("--clk and --data are the same pin")
		}
	}
	if proto == "pic" || proto == "tpi" {
		if r := strings.ToLower(strings.TrimSpace(p.reset)); r == "" || r == "nrst" {
			// A profile signal named MCLR or RESET puts reset on an LA pin; else the pod's reset pin.
			if r == "" {
				if n, _ := pin("reset", ""); n != 0 && n != data && n != pins["clk"] {
					pins["reset"] = n
				}
			}
			if _, ok := pins["reset"]; !ok {
				pins["reset"] = "nrst"
			}
		} else {
			n, err := parseLAPin(r)
			if err != nil {
				return nil, usageErrorf("--reset: use nrst or an LA pin: %w", err)
			}
			pins["reset"] = n
		}
	} else if strings.TrimSpace(p.reset) != "" {
		return nil, usageErrorf("--reset applies to --target pic and attiny-tpi only")
	}
	return pins, nil
}

var progLines = map[string]string{
	"updi": "UPDI", "pic": "ICSPCLK / ICSPDAT", "c2": "C2CK / C2D", "tpi": "TPICLK / TPIDATA", "pdi": "PDI_CLK / PDI_DATA",
}

type progStatus struct {
	Running bool     `json:"running"`
	Phase   string   `json:"phase"`
	Done    int      `json:"done"`
	Total   int      `json:"total"`
	ID      string   `json:"id"`
	AtAddr  *int     `json:"at_addr"`
	Protos  []string `json:"protos"`
	Result  *string  `json:"result"`
}

// progFix is the advice shown after a failed job.
func progFix(res string, proto string) string {
	switch {
	case strings.HasPrefix(res, "no answer"):
		return "check the wiring (" + progLines[proto] + "), that the chip is powered, and that the pod's I/O voltage (the wiring profile's la_mv) is the chip's supply"
	case strings.HasPrefix(res, "wrong chip"):
		return "the chip is not the one --device names; run with --info to read its ID, or fix --device / --expect-id"
	case strings.HasPrefix(res, "locked"):
		return "pass --erase chip to erase the chip and unlock it"
	case strings.HasPrefix(res, "verify failed"):
		return "the read-back differs: try --erase chip, and check the supply voltage and wiring"
	case strings.HasPrefix(res, "timeout"):
		return "the chip stayed busy: check its supply voltage (a PIC erase needs VDD above its brown-out level)"
	case strings.HasPrefix(res, "not supported"):
		return "this chip, memory or protocol is not supported by the pod's firmware: update it with `benchpod flash-self`"
	case strings.HasPrefix(res, "memory, address"):
		return "check --mem, --load-address and the device's page size"
	}
	return ""
}

func progCommand(ctx context.Context, c *tcpclient.Client, req map[string]any) (json.RawMessage, error) {
	raw, err := c.Command(ctx, req)
	if err != nil {
		if isUnknownCommand(err) {
			return nil, fmt.Errorf("flash: this pod's firmware has no prog_flash command: update it with `benchpod flash-self`")
		}
		msg := err.Error()
		if strings.Contains(msg, "xip_prog") || strings.Contains(msg, "install the blobs") || strings.Contains(msg, "install-blobs") {
			return nil, fmt.Errorf("flash: %w (the chip protocols live in the xip_prog blob: run `benchpod install-blobs`)", err)
		}
		return nil, fmt.Errorf("flash: %w", err)
	}
	return raw, nil
}

// progWait polls prog_flash status until the job ends, logging progress every 10%.
func progWait(ctx context.Context, c *tcpclient.Client, what string) (*progStatus, error) {
	lastPct := -1
	for {
		select {
		case <-ctx.Done():
			_, _ = c.Command(context.Background(), map[string]any{"cmd": "prog_flash", "op": "abort"})
			return nil, fmt.Errorf("flash: %s: %w (the job was aborted)", what, ctx.Err())
		case <-time.After(progPollInterval):
		}
		raw, err := c.Command(ctx, map[string]any{"cmd": "prog_flash", "op": "status"})
		if err != nil {
			return nil, fmt.Errorf("flash: status: %w", err)
		}
		var st progStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return nil, fmt.Errorf("flash: status: %w", err)
		}
		if !st.Running {
			return &st, nil
		}
		if st.Total > 0 {
			if pct := st.Done * 100 / st.Total / 10 * 10; pct != lastPct {
				lastPct = pct
				log.Printf("flash: %s %d%% (%s)", what, pct, st.Phase)
			}
		}
	}
}

func runFlashProg(g *globalFlags, f *flashFlags, spec ConnSpec) error {
	p := &f.prog
	proto := progdev.Proto(f.target)
	if spec.IsCloud() {
		return fmt.Errorf("flashing %s from the CLI needs a direct link to the pod (its network address or `usb`); over embeddedci.com use the Python SDK or the MCP server", f.target)
	}
	if !spec.IsNetwork() {
		return fmt.Errorf("flashing %s needs the pod's network: pass its address, or `usb` on firmware 3.9.0+ with USB networking", f.target)
	}
	var dev *progdev.Device
	if s := strings.TrimSpace(p.device); s != "" {
		d, err := progdev.Find(proto, s)
		if err != nil {
			return usageErrorf("--device: %w", err)
		}
		dev = &d
	}
	pins, err := p.progPins(proto, loadPodWiring(g, os.Stderr))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashTimeout))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()
	c := &tcpclient.Client{Addr: spec.Addr}

	base := func(op string) map[string]any {
		req := map[string]any{"cmd": "prog_flash", "op": op, "proto": proto}
		for k, v := range pins {
			req[k] = v
		}
		if p.baud > 0 {
			req["baud"] = p.baud
		}
		return req
	}

	// info: enter programming mode, read the ID, write nothing.
	runInfo := func() (*progStatus, error) {
		if _, err := progCommand(ctx, c, base("info")); err != nil {
			return nil, err
		}
		st, err := progWait(ctx, c, "reading the chip ID")
		if err != nil {
			return nil, err
		}
		if st.Result == nil || *st.Result != "ok" {
			res := ""
			if st.Result != nil {
				res = *st.Result
			}
			return st, progFailure("reading the chip ID", st, res, proto)
		}
		return st, nil
	}
	if p.info {
		st, err := runInfo()
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s chip ID: %s", proto, st.ID)
		if names := progdev.ByID(proto, st.ID); len(names) > 0 {
			var n []string
			for _, d := range names {
				n = append(n, d.Name)
			}
			line += " (" + strings.Join(n, ", ") + ")"
		} else {
			line += " (not in the device table)"
		}
		fmt.Println(line)
		return nil
	}

	regions, err := progImages(f.file, f.loadAddr)
	if err != nil {
		return err
	}
	mem := strings.ToLower(strings.TrimSpace(p.mem))
	if mem == "" {
		mem = "flash"
	}
	switch mem {
	case "flash", "eeprom", "config", "user":
	default:
		return usageErrorf("--mem must be flash, eeprom, config or user, got %q", p.mem)
	}
	erase := strings.ToLower(strings.TrimSpace(p.erase))
	if erase == "" {
		erase = "page"
		if proto == "tpi" {
			erase = "chip" // the chips erase flash only as a whole code section
		}
	}
	if erase != "chip" && erase != "page" && erase != "none" {
		return usageErrorf("--erase must be chip, page or none, got %q", p.erase)
	}
	expect := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(p.expectID), "0x"))
	page := 0
	flags := 0
	if dev != nil {
		page, flags = dev.Page, dev.Flags
		if expect == "" && !p.noExpectID {
			expect = dev.ID
		}
		if mem == "flash" && dev.FlashSize > 0 {
			for _, r := range regions {
				if r.addr+len(r.data) > dev.FlashSize {
					return fmt.Errorf("flash: the image ends at 0x%x, past the %d bytes of flash of %s", r.addr+len(r.data), dev.FlashSize, dev.Name)
				}
			}
		}
	} else if mem != "flash" || proto != "c2" {
		// Without --device the write page is unknown; the pod needs it.
		st, err := runInfo()
		if err != nil {
			return err
		}
		m := progdev.ByID(proto, st.ID)
		if len(m) == 0 {
			return usageErrorf("the chip answered ID %s, which is not in the device table: pass --device (known: %s)", st.ID, strings.Join(progdev.Names(proto), ", "))
		}
		if len(m) > 1 {
			var n []string
			for _, d := range m {
				n = append(n, d.Name)
			}
			return usageErrorf("the chip answered ID %s, which fits several devices: pass --device (%s)", st.ID, strings.Join(n, ", "))
		}
		dev = &m[0]
		page, flags = dev.Page, dev.Flags
		log.Printf("flash: chip ID %s is a %s", st.ID, dev.Name)
		if expect == "" && !p.noExpectID {
			expect = dev.ID
		}
	} else {
		return usageErrorf("--device is required for this target (the chip's write page comes from the device table)")
	}

	t0 := time.Now()
	total := 0
	for i, r := range regions {
		if _, err := c.StagePSRAM(ctx, r.data); err != nil {
			return fmt.Errorf("flash: uploading the image: %w", err)
		}
		req := base("start")
		req["mem"], req["addr"], req["len"] = mem, r.addr, len(r.data)
		req["erase"] = erase
		if i > 0 && erase == "chip" {
			req["erase"] = "none" // the first region already erased the chip
		}
		if page > 0 {
			req["page"] = page
		}
		if flags != 0 {
			req["flags"] = flags
		}
		if expect != "" {
			req["expect_id"] = expect
		}
		req["run"] = !p.noReset && i == len(regions)-1
		if _, err := progCommand(ctx, c, req); err != nil {
			return err
		}
		what := fmt.Sprintf("%s 0x%x", mem, r.addr)
		st, err := progWait(ctx, c, what)
		if err != nil {
			return err
		}
		res := ""
		if st.Result != nil {
			res = *st.Result
		}
		if res != "ok" {
			return progFailure(what, st, res, proto)
		}
		total += len(r.data)
		fmt.Printf("%-8s 0x%06x %8d bytes  verified  chip ID %s\n", mem, r.addr, len(r.data), st.ID)
	}
	fmt.Printf("%s: %d bytes in %d region(s) written in %.1f s\n", proto, total, len(regions), time.Since(t0).Seconds())
	return nil
}

func progFailure(what string, st *progStatus, res, proto string) error {
	msg := fmt.Sprintf("flash: %s failed in %s: %s", what, st.Phase, res)
	if st.AtAddr != nil {
		msg += fmt.Sprintf(" (at 0x%x)", *st.AtAddr)
	}
	if fix := progFix(res, proto); fix != "" {
		msg += ": " + fix
	}
	return fmt.Errorf("%s", msg)
}
