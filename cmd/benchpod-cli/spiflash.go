package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/spiflash"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// spiFlags are shared by every spi-flash subcommand.
type spiFlags struct {
	sck, mosi, miso, cs string
	hz                  int
	mode                int
	nreset              bool
}

func newSPIFlashCmd(g *globalFlags) *cobra.Command {
	f := &spiFlags{}
	cmd := &cobra.Command{
		Use:   "spi-flash",
		Short: "Read, erase and program an SPI NOR flash on four LA pins (network)",
		Long: "Read, erase and program a 25-series SPI NOR flash (W25Q, MX25, GD25, IS25, ...)\n" +
			"wired to four LA pins, through the pod's SPI master. Needs gateware with the\n" +
			"spi_master capability and the LA voltage set. 3-byte addresses: the first 16 MB.\n" +
			"Network connection only: it does not work over USB or embeddedci.com.\n\n" +
			"Pins left out come from the pod's wiring profile on embeddedci.com (spi_sclk,\n" +
			"spi_mosi, spi_miso, spi_cs) when this machine is signed in (`benchpod login` or\n" +
			"BENCHPOD_API_KEY) and the pod is on that account. Flags always win.\n\n" +
			"Pass --nreset when the DUT's reset is wired to the pod's reset pin (" + nrstPinLocation + "):\n" +
			"the DUT is held in reset for the whole run so its own controller stays off the bus,\n" +
			"and released at the end. The SPI pins are always released at the end, also on\n" +
			"an error or Ctrl-C.",
		Example: "  benchpod spi-flash id --sck 3 --mosi 4 --miso 5 --cs 6\n" +
			"  benchpod spi-flash write fw.bin --sck 3 --mosi 4 --miso 5 --cs 6 --nreset\n" +
			"  benchpod spi-flash read dump.bin --addr 0 --len 1M --sck 3 --mosi 4 --miso 5 --cs 6\n" +
			"  benchpod spi-flash erase --chip --sck 3 --mosi 4 --miso 5 --cs 6",
	}
	pf := cmd.PersistentFlags()
	pf.StringVar(&f.sck, "sck", "", "LA pin for SCK, 1-14 (default: spi_sclk from the pod's wiring profile)")
	pf.StringVar(&f.mosi, "mosi", "", "LA pin for MOSI, 1-14 (default: spi_mosi from the pod's wiring profile)")
	pf.StringVar(&f.miso, "miso", "", "LA pin for MISO, 1-14 (default: spi_miso from the pod's wiring profile)")
	pf.StringVar(&f.cs, "cs", "", "LA pin for CS, 1-14 (default: spi_cs from the pod's wiring profile)")
	pf.IntVar(&f.hz, "hz", 1000000, "SCK rate in Hz; the pod uses the nearest rate at or below it (190 kHz to 6 MHz)")
	pf.IntVar(&f.mode, "mode", 0, "SPI mode, 0 or 3")
	pf.BoolVar(&f.nreset, "nreset", false, "hold the DUT in reset through the pod's reset pin while flashing")

	cmd.AddCommand(
		newSPIFlashIDCmd(g, f),
		newSPIFlashWriteCmd(g, f),
		newSPIFlashReadCmd(g, f),
		newSPIFlashEraseCmd(g, f),
	)
	return cmd
}

// fillFromWiring takes the SPI pins left out of the flags from the pod's wiring profile on
// embeddedci.com (spi_sclk, spi_mosi, spi_miso, spi_cs), when there is one to read. Flags win.
func (f *spiFlags) fillFromWiring(g *globalFlags) {
	if f.sck != "" && f.mosi != "" && f.miso != "" && f.cs != "" {
		log.Printf("spi-flash: pins from the flags")
		return
	}
	if spec, err := g.resolveTarget(); err != nil || !spec.IsNetwork() {
		return // the connection error comes from the command itself
	}
	w := loadPodWiring(g, os.Stderr)
	if w == nil {
		return
	}
	var from []string
	for _, p := range []struct {
		name string
		flag *string
		pin  *int
	}{{"SCK", &f.sck, w.SpiSclk}, {"MOSI", &f.mosi, w.SpiMosi}, {"MISO", &f.miso, w.SpiMiso}, {"CS", &f.cs, w.SpiCs}} {
		if pinFromWiring(p.flag, p.pin) {
			from = append(from, p.name+" LA"+*p.flag)
		}
	}
	if len(from) > 0 {
		log.Printf("spi-flash: %s from %s", strings.Join(from, ", "), wiringSource)
	}
}

// config validates the pin flags into a spi_start request.
func (f *spiFlags) config() (spiflash.Config, error) {
	var pins [4]int
	names := [4]string{"sck", "mosi", "miso", "cs"}
	vals := [4]string{f.sck, f.mosi, f.miso, f.cs}
	for i, v := range vals {
		if strings.TrimSpace(v) == "" {
			return spiflash.Config{}, fmt.Errorf("--sck, --mosi, --miso and --cs are required (or set spi_sclk, spi_mosi, spi_miso and spi_cs in the pod's wiring profile on embeddedci.com)")
		}
		n, err := parseLAPin(v)
		if err != nil {
			return spiflash.Config{}, fmt.Errorf("--%s: %w", names[i], err)
		}
		for j := 0; j < i; j++ {
			if pins[j] == n {
				return spiflash.Config{}, fmt.Errorf("--%s and --%s are both LA%d; each needs its own pin", names[j], names[i], n)
			}
		}
		pins[i] = n
	}
	if f.mode != 0 && f.mode != 3 {
		return spiflash.Config{}, fmt.Errorf("--mode must be 0 or 3")
	}
	if f.hz <= 0 {
		return spiflash.Config{}, fmt.Errorf("--hz must be positive")
	}
	return spiflash.Config{SCK: pins[0], MOSI: pins[1], MISO: pins[2], CS: pins[3], Hz: f.hz, Mode: f.mode}, nil
}

// runSPI resolves the network connection, checks the pod has the SPI master,
// and runs fn in an SPI session. Commands go over one persistent connection;
// cleanup (spi_stop, reset release) uses fresh ones so it works after Ctrl-C.
func runSPI(g *globalFlags, f *spiFlags, name string, def time.Duration,
	fn func(ctx context.Context, p *spiflash.Pod, s spiflash.Started) error) error {
	f.fillFromWiring(g)
	cfg, err := f.config()
	if err != nil {
		return err
	}
	ctx, cancel, client, err := g.networkClient("spi-flash", def)
	if err != nil {
		return err
	}
	defer cancel()
	return runSPIWith(ctx, client, cfg, f.nreset, name, fn)
}

func runSPIWith(ctx context.Context, client *tcpclient.Client, cfg spiflash.Config, nreset bool, name string,
	fn func(ctx context.Context, p *spiflash.Pod, s spiflash.Started) error) error {
	sess, err := client.Open(ctx)
	if err != nil {
		return fmt.Errorf("spi-flash %s: %w", name, err)
	}
	defer sess.Close()

	status, err := sess.Command(ctx, map[string]any{"cmd": "status"})
	if err != nil {
		return fmt.Errorf("spi-flash %s: status: %w", name, err)
	}
	ok, err := spiflash.HasCap(status, spiflash.Cap)
	if err != nil {
		return fmt.Errorf("spi-flash %s: %w", name, err)
	}
	if !ok {
		return fmt.Errorf("spi-flash: this pod has no SPI master (no %q in its caps); it needs gateware v45 or newer", spiflash.Cap)
	}

	err = spiflash.Session(ctx, sess, cleanupCommander{sess, client}, cfg, nreset, func(ctx context.Context, p *spiflash.Pod, s spiflash.Started) error {
		msg := fmt.Sprintf("spi-flash: SCK LA%d, MOSI LA%d, MISO LA%d, CS LA%d at %s, mode %d", s.SCK, s.MOSI, s.MISO, s.CS, formatHz(s.Hz), s.Mode)
		if nreset {
			msg += ", DUT held in reset"
		}
		fmt.Fprintln(os.Stderr, msg)
		return fn(ctx, p, s)
	})
	if err != nil {
		return fmt.Errorf("spi-flash %s: %w", name, err)
	}
	return nil
}

// cleanupCommander sends cleanup on the session while it is healthy, and over a
// fresh connection once a cancel or timeout has left it owing a reply.
type cleanupCommander struct {
	sess   *tcpclient.Session
	client *tcpclient.Client
}

func (c cleanupCommander) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	if c.sess.Broken() {
		return c.client.Command(ctx, req)
	}
	return c.sess.Command(ctx, req)
}

// identify reads the ID and refuses when nothing answers.
func identify(ctx context.Context, p *spiflash.Pod) (spiflash.ID, error) {
	id, err := p.ReadID(ctx)
	if err != nil {
		return id, fmt.Errorf("read id: %w", err)
	}
	if !id.Present {
		return id, fmt.Errorf("no flash answering (id %s): check the wiring, the LA voltage, and that the DUT is not driving the bus (--nreset)", id.ID)
	}
	size := "unknown size"
	if id.Size > 0 {
		size = formatBytes(id.Size)
	}
	fmt.Fprintf(os.Stderr, "spi-flash: id %s, %s\n", id.ID, size)
	return id, nil
}

// fits checks [addr, addr+n) against the part's size when it is known.
func fits(id spiflash.ID, addr, n int) error {
	if id.Size > 0 && addr+n > id.Size {
		return fmt.Errorf("0x%06x..0x%06x is past the end of this %s part", addr, addr+n, formatBytes(id.Size))
	}
	return nil
}

// ── id ──────────────────────────────────────────────────────────────────────

func newSPIFlashIDCmd(g *globalFlags, f *spiFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "id",
		Short: "Read the flash's JEDEC ID, size and status register",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSPI(g, f, "id", time.Minute, func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
				id, err := p.ReadID(ctx)
				if err != nil {
					return err
				}
				out, closeOut, err := resolveOutput(g.outputFilename)
				if err != nil {
					return err
				}
				defer closeOut()
				fmt.Fprintf(out, "id:      %s\npresent: %v\nsize:    %d\nstatus:  0x%02x\n", id.ID, id.Present, id.Size, id.Status)
				if !id.Present {
					return fmt.Errorf("no flash answering on those pins")
				}
				return nil
			})
		},
	}
}

// ── write ───────────────────────────────────────────────────────────────────

func newSPIFlashWriteCmd(g *globalFlags, f *spiFlags) *cobra.Command {
	var addrS string
	var noErase, noVerify bool
	cmd := &cobra.Command{
		Use:   "write FILE",
		Short: "Erase, program and verify a raw image",
		Long: "Erase the range FILE covers (whole 4 KB sectors, in 1 MB steps), program it in\n" +
			"768-byte chunks, and have the pod read every chunk back. Bytes that share a\n" +
			"sector with the image but lie outside it are erased too.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			addr, err := parseSize(addrS)
			if err != nil {
				return fmt.Errorf("--addr: %w", err)
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return fmt.Errorf("%s is empty", args[0])
			}
			return runSPI(g, f, "write", time.Hour, func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
				return spiWrite(ctx, p, addr, data, !noErase, !noVerify, os.Stderr)
			})
		},
	}
	cmd.Flags().StringVar(&addrS, "addr", "0", "flash address to write at, e.g. 0x10000")
	cmd.Flags().BoolVar(&noErase, "no-erase", false, "do not erase first (the range must already be erased)")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "do not read each chunk back")
	return cmd
}

func spiWrite(ctx context.Context, p *spiflash.Pod, addr int, data []byte, erase, verify bool, w io.Writer) error {
	id, err := identify(ctx, p)
	if err != nil {
		return err
	}
	if err := fits(id, addr, len(data)); err != nil {
		return err
	}
	start := time.Now()
	if erase {
		pr := newProgress(w, "erase")
		if err := p.Erase(ctx, addr, len(data), pr.update); err != nil {
			pr.end()
			return err
		}
		pr.end()
	}
	label := "write"
	if verify {
		label = "write+verify"
	}
	pr := newProgress(w, label)
	err = p.Write(ctx, addr, data, verify, pr.update)
	pr.end()
	if err != nil {
		return err
	}
	el := time.Since(start)
	done := "wrote"
	if verify {
		done = "wrote and verified"
	}
	fmt.Fprintf(w, "spi-flash: %s %s at 0x%06x in %s (%s)\n", done, formatBytes(len(data)), addr, el.Round(100*time.Millisecond), formatRate(len(data), el))
	return nil
}

// ── read ────────────────────────────────────────────────────────────────────

func newSPIFlashReadCmd(g *globalFlags, f *spiFlags) *cobra.Command {
	var addrS, lenS string
	cmd := &cobra.Command{
		Use:   "read OUT",
		Short: "Read flash contents into a file (- for stdout)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			addr, err := parseSize(addrS)
			if err != nil {
				return fmt.Errorf("--addr: %w", err)
			}
			n := 0
			if strings.TrimSpace(lenS) != "" {
				if n, err = parseSize(lenS); err != nil || n <= 0 {
					return fmt.Errorf("--len must be a positive size, e.g. 4096, 0x1000 or 1M")
				}
			}
			return runSPI(g, f, "read", time.Hour, func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
				id, err := identify(ctx, p)
				if err != nil {
					return err
				}
				if n == 0 {
					if id.Size == 0 {
						return fmt.Errorf("the part's size is unknown; pass --len")
					}
					n = min(id.Size, spiflash.AddrSpace) - addr
					if n <= 0 {
						return fmt.Errorf("--addr is past the end of the part")
					}
				}
				if err := fits(id, addr, n); err != nil {
					return err
				}
				start := time.Now()
				pr := newProgress(os.Stderr, "read")
				data, err := p.Read(ctx, addr, n, pr.update)
				pr.end()
				if err != nil {
					return err
				}
				if err := writeOut(args[0], data); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "spi-flash: read %s from 0x%06x in %s (%s)\n", formatBytes(n), addr, time.Since(start).Round(100*time.Millisecond), formatRate(n, time.Since(start)))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&addrS, "addr", "0", "flash address to read from")
	cmd.Flags().StringVar(&lenS, "len", "", "bytes to read, e.g. 4096 or 1M (default: to the end of the part)")
	return cmd
}

func writeOut(path string, data []byte) error {
	if path == "-" {
		_, err := os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// ── erase ───────────────────────────────────────────────────────────────────

func newSPIFlashEraseCmd(g *globalFlags, f *spiFlags) *cobra.Command {
	var addrS, lenS string
	var chip bool
	cmd := &cobra.Command{
		Use:   "erase",
		Short: "Erase a range (--addr, --len) or the whole part (--chip)",
		Long: "Erase every 4 KB sector the range touches, in 1 MB steps, or the whole part\n" +
			"with --chip. A chip erase can take minutes on a large part.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var addr, n int
			if chip {
				if strings.TrimSpace(lenS) != "" {
					return fmt.Errorf("use either --chip or --addr/--len")
				}
			} else {
				var err error
				if addr, err = parseSize(addrS); err != nil {
					return fmt.Errorf("--addr: %w", err)
				}
				if n, err = parseSize(lenS); err != nil || n <= 0 {
					return fmt.Errorf("--len is required (or --chip), e.g. 4096 or 1M")
				}
			}
			return runSPI(g, f, "erase", time.Hour, func(ctx context.Context, p *spiflash.Pod, _ spiflash.Started) error {
				id, err := identify(ctx, p)
				if err != nil {
					return err
				}
				if chip {
					fmt.Fprintln(os.Stderr, "spi-flash: chip erase, this can take minutes")
					d, err := p.ChipErase(ctx)
					if err != nil {
						return err
					}
					fmt.Fprintf(os.Stderr, "spi-flash: chip erased in %s\n", d.Round(100*time.Millisecond))
					return nil
				}
				if err := fits(id, addr, n); err != nil {
					return err
				}
				start := time.Now()
				pr := newProgress(os.Stderr, "erase")
				err = p.Erase(ctx, addr, n, pr.update)
				pr.end()
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "spi-flash: erased %s at 0x%06x in %s\n", formatBytes(n), addr, time.Since(start).Round(100*time.Millisecond))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&addrS, "addr", "0", "first address to erase")
	cmd.Flags().StringVar(&lenS, "len", "", "bytes to erase, e.g. 65536 or 1M")
	cmd.Flags().BoolVar(&chip, "chip", false, "erase the whole part")
	return cmd
}

// ── formatting and parsing ──────────────────────────────────────────────────

// parseSize parses a decimal or 0x-hex number with an optional K or M suffix
// (1024-based): "4096", "0x1000", "64K", "1M".
func parseSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 1
	switch {
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	}
	v, err := strconv.ParseInt(s, 0, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid number %q", s)
	}
	if v*int64(mult) > 1<<31 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return int(v) * mult, nil
}

func formatBytes(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func formatRate(n int, d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return formatBytes(int(float64(n)/d.Seconds())) + "/s"
}

func formatHz(hz int) string {
	if hz >= 1000000 {
		return strconv.FormatFloat(float64(hz)/1e6, 'f', -1, 64) + " MHz"
	}
	return strconv.FormatFloat(float64(hz)/1e3, 'f', -1, 64) + " kHz"
}

// progress prints "label  42%  1.2/3.0 MB  45.1 KB/s  ETA 40s": in place on a
// terminal, else one line every few seconds.
type progress struct {
	w          io.Writer
	label      string
	tty        bool
	start      time.Time
	last       time.Time
	printed    bool
	done, tot  int
	lastPrint  int
	lineLength int
}

func newProgress(w io.Writer, label string) *progress {
	tty := false
	if f, ok := w.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	return &progress{w: w, label: label, tty: tty, start: time.Now()}
}

func (p *progress) update(done, total int) {
	p.done, p.tot = done, total
	every := 5 * time.Second
	if p.tty {
		every = 200 * time.Millisecond
	}
	if done < total && time.Since(p.last) < every {
		return
	}
	p.print()
}

func (p *progress) print() {
	if p.tot == 0 || p.done == p.lastPrint && p.printed {
		return
	}
	p.last, p.printed, p.lastPrint = time.Now(), true, p.done
	line := progressLine(p.label, p.done, p.tot, time.Since(p.start))
	if p.tty {
		pad := ""
		if n := p.lineLength - len(line); n > 0 {
			pad = strings.Repeat(" ", n)
		}
		p.lineLength = len(line)
		fmt.Fprintf(p.w, "\r%s%s", line, pad)
		return
	}
	fmt.Fprintln(p.w, line)
}

// end prints the final state and ends the in-place line.
func (p *progress) end() {
	p.print()
	if p.tty && p.printed {
		fmt.Fprintln(p.w)
	}
}

func progressLine(label string, done, total int, el time.Duration) string {
	pct := 100 * done / total
	s := fmt.Sprintf("%s %3d%%  %s/%s", label, pct, formatBytes(done), formatBytes(total))
	if el > 0 && done > 0 {
		s += "  " + formatRate(done, el)
		if done < total {
			eta := time.Duration(float64(el) * float64(total-done) / float64(done))
			s += "  ETA " + eta.Round(time.Second).String()
		}
	}
	return s
}
