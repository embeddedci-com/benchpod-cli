package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// The bus-master commands of gateware v55: the pod as an I2C master on any two LA pins
// (i2c_start, i2c_xfer, i2c_scan, i2c_status, i2c_stop) and which way each pin's DUT signal goes
// (la_dir, shown by la_pins). They are single-reply JSON commands, so they work over the network
// and through embeddedci.com. An I2C session outlives the connection: `benchpod i2c start` opens
// it, later runs use it, `benchpod i2c stop` ends it.

// busMasterMinGateware is the first gateware with the I2C master, SPI modes 1 and 2 and la_dir.
const busMasterMinGateware = 55

// tooOldForBusMaster is what a command says when the pod's firmware does not know it.
func tooOldForBusMaster(feature string) string {
	return fmt.Sprintf("this pod's firmware has no %s: it needs gateware v%d or later; update the pod's firmware with `benchpod flash-self`",
		feature, busMasterMinGateware)
}

// busCommand sends one command and words the reply for the user: a firmware without the command
// ("unknown cmd") says which update it needs, other refusals get the usual hints.
func busCommand(ctx context.Context, t *podTarget, what, feature string, req map[string]any, out any) error {
	raw, err := t.client.Command(ctx, req)
	if err != nil {
		if pe, ok := tcpclient.AsPodError(err); ok {
			if strings.TrimSpace(pe.Reason) == "unknown cmd" {
				return &shownRefusal{msg: what + ": " + tooOldForBusMaster(feature), pe: pe}
			}
			return refusedError(what, pe, !t.spec.IsCloud())
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: unexpected reply %s", what, strings.TrimSpace(string(raw)))
	}
	return nil
}

// pinResolver turns pin arguments into LA channels: a number (3, la3) or a name from the pod's
// wiring profile (a role such as i2c_sda, or a signal such as DRDY). The profile is read once,
// only when a name needs it.
type pinResolver struct {
	g      *globalFlags
	w      *podWiring
	loaded bool
}

func (r *pinResolver) wiring() *podWiring {
	if !r.loaded {
		r.loaded = true
		r.w = loadPodWiring(r.g, os.Stderr)
	}
	return r.w
}

func (r *pinResolver) pin(what, v string) (int, error) {
	v = strings.TrimSpace(v)
	if n, err := parseLAPin(v); err == nil {
		return n, nil
	}
	if strings.HasPrefix(strings.ToLower(v), "la") {
		if _, err := strconv.Atoi(v[2:]); err == nil {
			return parseLAPin(v) // la15: the range error
		}
	}
	if _, err := strconv.Atoi(v); err == nil {
		return parseLAPin(v)
	}
	if la, ok := r.wiring().channel(v); ok {
		if la < 1 || la > laPinCount {
			return 0, fmt.Errorf("%s: %q is LA%d in the wiring profile, not a pin 1-%d", what, v, la, laPinCount)
		}
		return la, nil
	}
	return 0, fmt.Errorf("%s: %q is not an LA pin (1-%d or la1) or a name in the pod's wiring profile", what, v, laPinCount)
}

// pins parses a pin list: "9,10", "9-12", "la9,DRDY".
func (r *pinResolver) pins(what, v string) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		var from, to int
		var err error
		if isRange {
			if from, err = parseLAPin(lo); err == nil {
				to, err = parseLAPin(hi)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", what, err)
			}
			if to < from {
				return nil, fmt.Errorf("%s: range %q runs backwards", what, part)
			}
		} else {
			if from, err = r.pin(what, part); err != nil {
				return nil, err
			}
			to = from
		}
		for la := from; la <= to; la++ {
			if !seen[la] {
				seen[la] = true
				out = append(out, la)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no pins given", what)
	}
	return out, nil
}

// parseHexBytes reads bytes written as hex: "0d", "0x0d1f", "0d 1f", "0d,1f", "0d:1f".
func parseHexBytes(s string) ([]byte, error) {
	var b strings.Builder
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == ':' }) {
		f = strings.TrimPrefix(strings.TrimPrefix(f, "0x"), "0X")
		if len(f)%2 == 1 {
			f = "0" + f // "d" or "0xd" is one byte
		}
		b.WriteString(f)
	}
	out, err := hex.DecodeString(b.String())
	if err != nil {
		return nil, fmt.Errorf("%q is not hex bytes (e.g. 0d or \"0d 1f\")", s)
	}
	return out, nil
}

// formatHexBytes writes bytes as "01 17".
func formatHexBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02x", c)
	}
	return strings.Join(parts, " ")
}

// parseI2CAddr reads a 7-bit address: 0x48, 48h or 72.
func parseI2CAddr(s string) (int, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	var n int64
	var err error
	switch {
	case strings.HasPrefix(v, "0x"):
		n, err = strconv.ParseInt(v[2:], 16, 16)
	case strings.HasSuffix(v, "h"):
		n, err = strconv.ParseInt(strings.TrimSuffix(v, "h"), 16, 16)
	default:
		n, err = strconv.ParseInt(v, 10, 16)
	}
	if err != nil || n < 0 || n > 0x7f {
		return 0, fmt.Errorf("invalid I2C address %q (a 7-bit address: 0x48 or 72)", s)
	}
	return int(n), nil
}

// ── i2c ─────────────────────────────────────────────────────────────────────

const i2cTimeout = 30 * time.Second

func newI2CCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "i2c",
		Short: "Talk to I2C chips on the DUT's board: the pod as an I2C master on two LA pins",
		Long: "The pod is an I2C master on any two LA pins (gateware v55 and later): read and\n" +
			"write registers of a chip on the DUT's board, or scan the bus. `i2c start` opens\n" +
			"the session and it stays open across runs until `i2c stop`. SWD, SPI and I2C\n" +
			"sessions exclude each other. Bytes are hex in and hex out. Works over the network\n" +
			"and through embeddedci.com.",
		Example: "  benchpod la pullup 1 on && benchpod la pullup 2 on\n" +
			"  benchpod i2c start --sda 1 --scl 2 --hz 400000\n" +
			"  benchpod i2c scan\n" +
			"  benchpod i2c xfer 0x48 --write 0d --read 2\n" +
			"  benchpod i2c stop",
	}
	cmd.AddCommand(newI2CStartCmd(g), newI2CXferCmd(g), newI2CScanCmd(g), newI2CStatusCmd(g), newI2CStopCmd(g))
	return cmd
}

type i2cSession struct {
	Active  *bool  `json:"active"`
	SDA     int    `json:"sda"`
	SCL     int    `json:"scl"`
	Hz      int    `json:"hz"`
	Pullups string `json:"pullups"`
}

func (s i2cSession) String() string {
	pu := "the pod's pull-ups"
	if s.Pullups == "external" {
		pu = "the board's pull-ups"
	}
	return fmt.Sprintf("SDA LA%d, SCL LA%d at %s, %s", s.SDA, s.SCL, formatHz(s.Hz), pu)
}

func newI2CStartCmd(g *globalFlags) *cobra.Command {
	var sda, scl, pullups string
	var hz int
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Claim two LA pins as an I2C master",
		Long: "Claim two LA pins as an I2C master. Pins are 1-14 (or la1) or names from the\n" +
			"pod's wiring profile; left out, they are its i2c_sda and i2c_scl. The bus needs\n" +
			"pull-ups: with --pullups pod (the default) both pins must be LA1-LA6 with their\n" +
			"pull-up on (`benchpod la pullup 1 on`), and the LA bank at 3.3 V. Pass\n" +
			"--pullups external when the board has its own. The pod reports the rate it uses.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if pullups != "pod" && pullups != "external" {
				return usageErrorf("--pullups must be pod or external")
			}
			if hz < 10000 || hz > 1000000 {
				return usageErrorf("--hz must be 10000 to 1000000")
			}
			r := &pinResolver{g: g}
			if sda == "" || scl == "" {
				w := r.wiring()
				var from []string
				if w != nil && pinFromWiring(&sda, w.I2cSda) {
					from = append(from, "SDA LA"+sda)
				}
				if w != nil && pinFromWiring(&scl, w.I2cScl) {
					from = append(from, "SCL LA"+scl)
				}
				if len(from) > 0 {
					fmt.Fprintf(os.Stderr, "i2c start: %s from %s\n", strings.Join(from, ", "), wiringSource)
				}
			}
			if sda == "" || scl == "" {
				return usageErrorf("--sda and --scl are required (or set i2c_sda and i2c_scl in the pod's wiring profile on embeddedci.com)")
			}
			sdaPin, err := r.pin("--sda", sda)
			if err != nil {
				return err
			}
			sclPin, err := r.pin("--scl", scl)
			if err != nil {
				return err
			}
			if sdaPin == sclPin {
				return usageErrorf("--sda and --scl are both LA%d; each needs its own pin", sdaPin)
			}
			ctx, cancel, t, err := g.podClient("i2c start", i2cTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			var s i2cSession
			req := map[string]any{"cmd": "i2c_start", "sda": sdaPin, "scl": sclPin, "hz": hz, "pullups": pullups}
			if err := busCommand(ctx, t, "i2c start", "I2C master", req, &s); err != nil {
				return err
			}
			return writeLine(g, "i2c: "+s.String())
		},
	}
	cmd.Flags().StringVar(&sda, "sda", "", "LA pin for SDA, 1-14 or a wiring name (default: i2c_sda from the pod's wiring profile)")
	cmd.Flags().StringVar(&scl, "scl", "", "LA pin for SCL, 1-14 or a wiring name (default: i2c_scl from the pod's wiring profile)")
	cmd.Flags().IntVar(&hz, "hz", 100000, "SCL rate in Hz, 10000 to 1000000; the pod never runs faster")
	cmd.Flags().StringVar(&pullups, "pullups", "pod", "pod (the pod's pull-ups on LA1-LA6, switched on first) or external (the board's)")
	return cmd
}

type i2cXferReply struct {
	Addr   int     `json:"addr"`
	Ack    bool    `json:"ack"`
	NackAt *int    `json:"nack_at"`
	Rx     *string `json:"rx"`
}

// nackText says which byte was not acknowledged (nack_at: 0 the address, 1..n a written byte,
// n+1 the address of the read).
func nackText(addr, nackAt, written int) string {
	switch {
	case nackAt == 0:
		return fmt.Sprintf("no device answered at 0x%02x (address not acknowledged)", addr)
	case nackAt <= written:
		return fmt.Sprintf("0x%02x did not acknowledge written byte %d of %d", addr, nackAt, written)
	default:
		return fmt.Sprintf("0x%02x did not acknowledge the read", addr)
	}
}

func newI2CXferCmd(g *globalFlags) *cobra.Command {
	var write string
	var read int
	cmd := &cobra.Command{
		Use:   "xfer <addr>",
		Short: "Write, read, or write then read (repeated START) one I2C device",
		Long: "Write --write bytes (hex, up to 128) to the 7-bit address, then read --read\n" +
			"bytes (up to 248) after a repeated START. With neither it probes the address.\n" +
			"Prints the bytes read as hex. A byte that is not acknowledged is an error that\n" +
			"says which one. Needs `benchpod i2c start` first.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			addr, err := parseI2CAddr(args[0])
			if err != nil {
				return usageErrorf("%v", err)
			}
			var w []byte
			if write != "" {
				if w, err = parseHexBytes(write); err != nil {
					return usageErrorf("--write: %v", err)
				}
				if len(w) > 128 {
					return usageErrorf("--write is %d bytes; the pod takes up to 128", len(w))
				}
			}
			if read < 0 || read > 248 {
				return usageErrorf("--read must be 0 to 248")
			}
			ctx, cancel, t, err := g.podClient("i2c xfer", i2cTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			req := map[string]any{"cmd": "i2c_xfer", "addr": addr}
			if len(w) > 0 {
				req["write"] = base64.RawURLEncoding.EncodeToString(w)
			}
			if read > 0 {
				req["read"] = read
			}
			var rep i2cXferReply
			if err := busCommand(ctx, t, "i2c xfer", "I2C master", req, &rep); err != nil {
				return err
			}
			if !rep.Ack {
				at := 0
				if rep.NackAt != nil {
					at = *rep.NackAt
				}
				return fmt.Errorf("i2c xfer: %s", nackText(addr, at, len(w)))
			}
			if read == 0 {
				return writeLine(g, fmt.Sprintf("0x%02x: ack", addr))
			}
			var rx []byte
			if rep.Rx != nil {
				if rx, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(*rep.Rx, "=")); err != nil {
					return fmt.Errorf("i2c xfer: the pod's rx %q is not base64url", *rep.Rx)
				}
			}
			return writeLine(g, formatHexBytes(rx))
		},
	}
	cmd.Flags().StringVar(&write, "write", "", `bytes to write, hex: 0d, "0d 1f" or 0x0d1f`)
	cmd.Flags().IntVar(&read, "read", 0, "bytes to read after the write, 0 to 248")
	return cmd
}

func newI2CScanCmd(g *globalFlags) *cobra.Command {
	var from, to string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "List the I2C addresses that answer",
		Long:  "Probe 0x08 to 0x77 (or --from to --to) and print the addresses that acknowledge, in hex.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := map[string]any{"cmd": "i2c_scan"}
			for _, f := range []struct{ name, val string }{{"from", from}, {"to", to}} {
				if !cmd.Flags().Changed(f.name) {
					continue
				}
				a, err := parseI2CAddr(f.val)
				if err != nil {
					return usageErrorf("--%s: %v", f.name, err)
				}
				req[f.name] = a
			}
			ctx, cancel, t, err := g.podClient("i2c scan", i2cTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			var rep struct {
				From  int   `json:"from"`
				To    int   `json:"to"`
				Found []int `json:"found"`
			}
			if err := busCommand(ctx, t, "i2c scan", "I2C master", req, &rep); err != nil {
				return err
			}
			if len(rep.Found) == 0 {
				return writeLine(g, fmt.Sprintf("no device answered (0x%02x to 0x%02x)", rep.From, rep.To))
			}
			parts := make([]string, len(rep.Found))
			for i, a := range rep.Found {
				parts[i] = fmt.Sprintf("0x%02x", a)
			}
			return writeLine(g, strings.Join(parts, " "))
		},
	}
	cmd.Flags().StringVar(&from, "from", "0x08", "first address to probe")
	cmd.Flags().StringVar(&to, "to", "0x77", "last address to probe")
	return cmd
}

func newI2CStatusCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the I2C session: its pins, rate and pull-ups",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel, t, err := g.podClient("i2c status", i2cTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			var s i2cSession
			if err := busCommand(ctx, t, "i2c status", "I2C master", map[string]any{"cmd": "i2c_status"}, &s); err != nil {
				return err
			}
			if s.Active != nil && !*s.Active {
				return writeLine(g, "i2c: no session (start one with `benchpod i2c start`)")
			}
			return writeLine(g, "i2c: "+s.String())
		},
	}
}

func newI2CStopCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "End the I2C session and release its two pins",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel, t, err := g.podClient("i2c stop", i2cTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			if err := busCommand(ctx, t, "i2c stop", "I2C master", map[string]any{"cmd": "i2c_stop"}, nil); err != nil {
				return err
			}
			return writeLine(g, "i2c: stopped")
		},
	}
}

// writeLine prints one line to the command output (--output-filename or stdout).
func writeLine(g *globalFlags, line string) error {
	out, closeOut, err := resolveOutput(g.outputFilename)
	if err != nil {
		return fmt.Errorf("open output: %w", err)
	}
	defer closeOut()
	fmt.Fprintln(out, line)
	return nil
}

// ── la pins / la dir ────────────────────────────────────────────────────────

// laPin is one la_pins entry.
type laPin struct {
	LA       int        `json:"la"`
	Function string     `json:"function"`
	Owner    string     `json:"owner"`
	GPIO     *string    `json:"gpio"`
	Level    *int       `json:"level"`
	Pull     *laPinPull `json:"pull"`
	Dir      string     `json:"dir"`
	Warn     int        `json:"warn"`
}

type laPinPull struct {
	Dir  string `json:"dir"`
	Ohms string `json:"ohms"`
	On   bool   `json:"on"`
}

type laPinsReply struct {
	Pins   []laPin `json:"pins"`
	Levels *int    `json:"levels"`
}

// laDirs are the directions la_dir takes.
var laDirs = []string{"dut_input", "dut_output", "bidir", "unknown"}

// warnTexts explains la_pins warn bits.
func warnTexts(w int) []string {
	var out []string
	if w&1 != 0 {
		out = append(out, "pull-up on under a push-pull output (costs current: use open_drain or switch the pull off)")
	}
	if w&2 != 0 {
		out = append(out, "driven high or pulled up while the pod's target power is off (this powers an unpowered DUT through its pins; fine when the DUT has its own supply)")
	}
	if rest := w &^ 3; rest != 0 {
		out = append(out, fmt.Sprintf("warning bits 0x%x", rest))
	}
	return out
}

// printLAPins writes the pin table, then one line per warning.
func printLAPins(w io.Writer, rep laPinsReply) {
	sort.Slice(rep.Pins, func(i, j int) bool { return rep.Pins[i].LA < rep.Pins[j].LA })
	fmt.Fprintf(w, "%-5s %-12s %-14s %-6s %-14s %-11s %s\n", "PIN", "FUNCTION", "GPIO", "LIVE", "PULL", "DIR", "WARN")
	for _, p := range rep.Pins {
		fn := p.Function
		if p.Owner != "" {
			fn += " (" + p.Owner + ")"
		}
		gpio := "-"
		if p.GPIO != nil {
			gpio = *p.GPIO
			if p.Level != nil {
				gpio += fmt.Sprintf(" %d", *p.Level)
			}
		}
		live := "-"
		if rep.Levels != nil {
			live = strconv.Itoa((*rep.Levels >> (p.LA - 1)) & 1)
		}
		pull := "-"
		if p.Pull != nil {
			state := "off"
			if p.Pull.On {
				state = "on"
			}
			pull = fmt.Sprintf("%s %s %s", p.Pull.Dir, p.Pull.Ohms, state)
		}
		dir := p.Dir
		if dir == "" {
			dir = "unknown"
		}
		warn := "-"
		if p.Warn != 0 {
			warn = strconv.Itoa(p.Warn)
		}
		fmt.Fprintf(w, "LA%-3d %-12s %-14s %-6s %-14s %-11s %s\n", p.LA, fn, gpio, live, pull, dir, warn)
	}
	for _, p := range rep.Pins {
		for _, t := range warnTexts(p.Warn) {
			fmt.Fprintf(w, "warning: LA%d: %s\n", p.LA, t)
		}
	}
}

func newLAPinsCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pins",
		Short: "List every LA pin: function, GPIO state, live level, pull-up, direction and warnings",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel, t, err := g.podClient("la pins", i2cTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			raw, err := t.client.Command(ctx, map[string]any{"cmd": "la_pins"})
			if err != nil {
				return fmt.Errorf("la pins: %w", err)
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("la pins: open output: %w", err)
			}
			defer closeOut()
			if asJSON {
				printJSON(out, raw)
				return nil
			}
			var rep laPinsReply
			if err := json.Unmarshal(raw, &rep); err != nil {
				return fmt.Errorf("la pins: unexpected reply %s", strings.TrimSpace(string(raw)))
			}
			printLAPins(out, rep)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the pod's reply as JSON")
	return cmd
}

func newLADirCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "dir [PINS DIR]",
		Short: "Tell the pod which LA pins are DUT outputs, inputs or bidirectional (or list them)",
		Long: "Tell the pod which way each pin's DUT signal goes: dut_input, dut_output, bidir,\n" +
			"or unknown (the default after a restart). PINS is a pin, a list or a range\n" +
			"(9, 9,10, 9-12, la9), a name from the pod's wiring profile, or all. On a pin\n" +
			"wired to a DUT output the pod refuses every push-pull driver (a GPIO output,\n" +
			"UART TX, SPI clock and data out, SWD, step pulses), so the two outputs never\n" +
			"drive against each other; open drain, I2C and inputs stay allowed. With no\n" +
			"arguments it lists every pin. Needs gateware v55 or later.",
		Example: "  benchpod la dir 9,10 dut_output\n  benchpod la dir uart_rx dut_output\n  benchpod la dir all unknown\n  benchpod la dir",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return usageErrorf("la dir takes no arguments (list) or PINS DIR")
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			req := map[string]any{"cmd": "la_dir"}
			if len(args) == 2 {
				dir := strings.ToLower(strings.TrimSpace(args[1]))
				if !containsString(laDirs, dir) {
					return usageErrorf("DIR must be one of %s, got %q", strings.Join(laDirs, ", "), args[1])
				}
				if strings.EqualFold(strings.TrimSpace(args[0]), "all") {
					req["la"] = "all"
				} else {
					r := &pinResolver{g: g}
					pins, err := r.pins("PINS", args[0])
					if err != nil {
						return usageErrorf("%v", err)
					}
					req["la"] = pins
				}
				req["dir"] = dir
			}
			ctx, cancel, t, err := g.podClient("la dir", i2cTimeout)
			if err != nil {
				return err
			}
			defer cancel()
			var rep laPinsReply
			if err := busCommand(ctx, t, "la dir", "pin directions (la_dir)", req, &rep); err != nil {
				return err
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return fmt.Errorf("la dir: open output: %w", err)
			}
			defer closeOut()
			printLAPins(out, rep)
			return nil
		},
	}
}
