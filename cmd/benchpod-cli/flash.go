package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/openocd"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// flashTimeout bounds a whole flash session; flashing over the network
// (or the USB console) is slow, so this is generous compared to the 30s
// default command deadline. Overridable with --timeout.
const flashTimeout = 5 * time.Minute

// dapHandshakeTimeout bounds just the dap-start handshake (arming the probe and
// reading the firmware's "dap ready"). The firmware answers near-instantly, so a
// short deadline here means a pod that never enters SWD mode fails fast — and we
// never launch OpenOCD against a dead link — instead of hanging on flashTimeout.
const dapHandshakeTimeout = 15 * time.Second

// targetPowerSettle gives a target a moment to boot after the pod enables its
// power eFuse, so the very first SWD connect attempt has a live DAP to talk to
// rather than relying on OpenOCD's connect retries.
const targetPowerSettle = 250 * time.Millisecond

// dapOptions are the dap_start extras the CLI asks for: 1 KB CMSIS-DAP packets, 4 in flight,
// and (when the CLI just powered the target) up to targetAnswerWaitMS for it to answer SWD, so a
// board that boots slowly (an STM32 NUCLEO: ~2.1 s) is not caught mid-boot by OpenOCD.
const targetAnswerWaitMS = 5000

func dapOptions(powered bool) tcpclient.DAPOptions {
	o := tcpclient.DAPOptions{PacketSize: 1024, PacketCount: 4}
	if powered {
		o.WaitMS = targetAnswerWaitMS
	}
	return o
}

// Where the pod's target-reset line comes out, and where that is documented.
// Since rev3 the pod drives NRST from its own pin, so --nreset no longer names an
// LA channel — it just says whether the target's reset reaches that pin.
const (
	nrstPinLocation = "DUT header J1 pin 22"
	nrstPinDocsURL  = "https://github.com/embeddedci-com/benchpod-firmware/blob/main/docs/API.md#dut-header"
)

// flashFlags holds the flash-specific (non-global) flags.
type flashFlags struct {
	swclk, swdio        string
	tck, tms, tdi, tdo  string
	trst                string
	nreset              bool
	target, file        string
	loadAddr            string
	noVerify, noReset   bool
	noConnectUnderReset bool
	keepResetInit       bool
	openocdBin          string
	targetPower         int
	extraConfigs        []string
	extraArgs           []string

	// An uploaded build instead of --file (see flashFromBuild).
	build, repo, ref, artifact string
	// serverSide: --server-side was given (serverSideSet) and its value.
	serverSide, serverSideSet bool

	// nresetSet says --nreset was given (either way), so the wiring profile does not override it.
	nresetSet bool

	// ESP32-family targets (--target esp32...): flash_esp.go.
	espRx, espTx, espBoot string

	// hasNRST is filled in from the pod during runFlash (not a flag): whether
	// this pod has the dedicated target-reset pin. Used by the failure hint.
	hasNRST bool
}

// newFlashCmd builds the flash subcommand. It flashes an SWD target wired to the
// pod's LA pins (1-14, mapped to the ice40/FPGA) by arming the pod's CMSIS-DAP
// probe, then running host-side OpenOCD bridged over the chosen transport. The
// pod holds no flash intelligence; OpenOCD's exit code is the verdict.
//
// The pod runs a CMSIS-DAP processor locally and OpenOCD's cmsis-dap TCP backend
// ships whole DAP transfers (DAP_Transfer / DAP_TransferBlock), collapsing
// thousands of per-bit round-trips into one per DAP command. A network address
// uses the TCP dap_start handshake (tcpclient.DAPStart); --connection usb / a
// device path uses the console dap-start handshake (serialconsole.DAPStart). Both
// need a recent OpenOCD with the cmsis_dap_tcp backend.
func newFlashCmd(g *globalFlags) *cobra.Command {
	f := &flashFlags{}
	cmd := &cobra.Command{
		Use:   "flash",
		Short: "Flash an SWD target via OpenOCD's CMSIS-DAP backend (network, USB, or an uploaded build)",
		Long: "Flash an SWD target wired to the pod's LA pins: the pod becomes a CMSIS-DAP\n" +
			"probe and a local OpenOCD programs the target through it. Works over the network\n" +
			"and over USB.\n\n" +
			"--build, --repo, --ref and --artifact flash a build uploaded to embeddedci.com\n" +
			"(the upload-artifact action or the pytest build_report fixture) instead of --file.\n" +
			"Over embeddedci.com (--connection embeddedci:<pod>) embeddedci.com runs OpenOCD, so\n" +
			"none is needed here; on the network or USB the file is downloaded and flashed by the\n" +
			"local OpenOCD. Both need `benchpod login` or BENCHPOD_API_KEY.\n\n" +
			"--swclk, --swdio, --nreset and --target default to swd_swclk, swd_swdio,\n" +
			"swd_nreset and swd_target in the pod's wiring profile when this\n" +
			"machine is signed in (`benchpod login` or BENCHPOD_API_KEY), the pod is on that\n" +
			"account and the connection is a network address. Flags always win; the log says\n" +
			"where each value came from. Without a profile, --swclk and --swdio are required.\n\n" +
			"JTAG is picked automatically, there is no transport flag: when --tck, --tms, --tdi and\n" +
			"--tdo (default: jtag_tck, jtag_tms, jtag_tdi, jtag_tdo in the wiring profile; --trst\n" +
			"is optional) are all known and either no SWD pins are or the target is JTAG-only\n" +
			"(esp32, esp32s2, gd32vf103, fe310, k210, riscv*), flash uses JTAG; otherwise SWD.\n" +
			"JTAG needs a pod with the jtag feature (gateware 61 or newer).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f.nresetSet = cmd.Flags().Changed("nreset")
			f.serverSideSet = cmd.Flags().Changed("server-side")
			return runFlash(g, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.swclk, "swclk", "", "LA pin for SWCLK, 1-14, e.g. 1 or la1 (default: swd_swclk from the pod's wiring profile)")
	fl.StringVar(&f.swdio, "swdio", "", "LA pin for SWDIO, 1-14, e.g. 2 or la2 (default: swd_swdio from the pod's wiring profile)")
	fl.StringVar(&f.tck, "tck", "", "LA pin for JTAG TCK (default: jtag_tck from the pod's wiring profile)")
	fl.StringVar(&f.tms, "tms", "", "LA pin for JTAG TMS (default: jtag_tms from the pod's wiring profile)")
	fl.StringVar(&f.tdi, "tdi", "", "LA pin for JTAG TDI (default: jtag_tdi from the pod's wiring profile)")
	fl.StringVar(&f.tdo, "tdo", "", "LA pin for JTAG TDO (default: jtag_tdo from the pod's wiring profile)")
	fl.StringVar(&f.trst, "trst", "", "LA pin for JTAG nTRST, optional (default: jtag_trst from the pod's wiring profile)")
	fl.BoolVar(&f.nreset, "nreset", false, "the target's NRST is wired to the pod's reset pin ("+nrstPinLocation+"); enables connect-under-reset (default: swd_nreset from the pod's wiring profile)")
	fl.StringVar(&f.target, "target", "", "OpenOCD target config (passed as -f), e.g. target/stm32f1x.cfg (default: swd_target from the pod's wiring profile)")
	fl.StringVar(&f.file, "file", "", "firmware image to flash (used with --target)")
	fl.StringVar(&f.build, "build", "", "flash a build uploaded to embeddedci.com instead of --file: its build id, or latest (the newest one matching --repo/--ref)")
	fl.StringVar(&f.repo, "repo", "", "with --build: the GitHub repository of the uploaded build, owner/name (newest build of that repo)")
	fl.StringVar(&f.ref, "ref", "", "with --build: the branch or tag of the uploaded build, e.g. main")
	fl.StringVar(&f.artifact, "artifact", "", "with --build: the file to flash when the build holds several (default: its .elf, then .hex, then .bin)")
	fl.BoolVar(&f.serverSide, "server-side", false, "with --build: have embeddedci.com run OpenOCD (default over embeddedci.com); --server-side=false downloads the file and flashes it here (default on the network and USB)")
	fl.StringVar(&f.loadAddr, "load-address", "", "load address for a raw .bin image (appended to the program command)")
	fl.BoolVar(&f.noVerify, "no-verify", false, "do not verify after programming")
	fl.BoolVar(&f.noReset, "no-reset", false, "do not reset the target after programming")
	fl.BoolVar(&f.noConnectUnderReset, "no-connect-under-reset", false, "do not hold the target in reset while connecting (on by default when --nreset is set; it stops already-running firmware from disabling SWD before the debug port is read)")
	fl.BoolVar(&f.keepResetInit, "keep-reset-init", false, "keep the target cfg's reset-init/reset-start events (clock boost); by default they are cleared because the just-after-reset clock writes glitch the slow bit-banged link (flashing then runs at the default reset clock — slower but reliable)")
	fl.StringVar(&f.openocdBin, "openocd", "", "path to the openocd binary; defaults to the one on PATH")
	fl.IntVar(&f.targetPower, "target-power", 0, "enable a target power eFuse before flashing: 1 (internal 5V) or 2 (external)")
	fl.StringArrayVarP(&f.extraConfigs, "command", "c", nil, "extra OpenOCD command, repeatable (passthrough, appended last)")
	fl.StringArrayVar(&f.extraArgs, "openocd-arg", nil, "extra raw OpenOCD argument, repeatable (passthrough, appended last)")
	fl.StringVar(&f.espRx, "rx", "", "ESP32 (--target esp32...): LA pin on the target's TXD0 (default: uart_rx from the pod's wiring profile)")
	fl.StringVar(&f.espTx, "tx", "", "ESP32: LA pin on the target's RXD0 (default: uart_tx from the pod's wiring profile)")
	fl.StringVar(&f.espBoot, "boot", "", "ESP32: LA pin on the target's IO0 (IO9 on the C3/C6), driven open-drain (default: a wiring-profile signal named BOOT, IO0 or IO9)")
	return cmd
}

func runFlash(g *globalFlags, f *flashFlags) error {
	spec, err := g.resolveTarget()
	if err != nil {
		return err
	}
	spec = g.preferUSBNetwork(spec, "flash")
	fromBuild := f.fromBuild()
	if fromBuild && strings.TrimSpace(f.file) != "" {
		return fmt.Errorf("pass either --file or an uploaded build (--build, --repo, --ref, --artifact), not both")
	}
	if !fromBuild && f.serverSideSet {
		return usageErrorf("--server-side only applies to an uploaded build (--build, --repo, --ref)")
	}
	serverSide := spec.IsCloud()
	if f.serverSideSet {
		serverSide = f.serverSide
	}
	if spec.IsCloud() && !fromBuild && !isESPTarget(f.target) {
		return fmt.Errorf("flash over embeddedci.com (--connection %s%s) flashes a build uploaded there: pass --build, --repo or --ref. "+
			"A local --file needs a direct link to the pod: pass its network address (`benchpod discover` finds it) or `usb`",
			cloudConnPrefix, spec.Name)
	}
	if spec.IsCloud() && !serverSide {
		return usageErrorf("--server-side=false needs a direct link to the pod: pass its network address or `usb` as --connection")
	}
	if !isESPTarget(f.target) && (spec.IsNetwork() || spec.IsCloud()) {
		f.fillFromWiring(g) // an ESP32 target uses no SWD settings
	}
	if isESPTarget(f.target) {
		if fromBuild {
			return fmt.Errorf("an ESP32 target is flashed from --file or --image, not an uploaded build")
		}
		return runFlashESP(g, f, spec)
	}
	var art *serverapi.BuildArtifact
	if fromBuild && spec.IsCloud() && serverSide && cloudIsGitHubAction() {
		// A GitHub Actions token cannot look builds up on the user's routes: embeddedci.com's
		// cloud flash route takes the build's selector itself (flashOnServer).
	} else if fromBuild {
		var cleanup func()
		var err error
		art, cleanup, err = f.resolveBuild(g, !serverSide)
		if err != nil {
			return err
		}
		defer cleanup()
		if strings.TrimSpace(f.target) == "" && art.Target() != "" {
			f.target = art.Target()
			log.Printf("flash: target %s from the build", f.target)
		}
	}
	pins, err := f.resolveDAPPins()
	if err != nil {
		return err
	}
	if strings.TrimSpace(f.file) != "" && strings.TrimSpace(f.target) == "" {
		if fromBuild {
			return usageErrorf("--target is required: the build recorded none and the wiring profile has no swd_target")
		}
		return usageErrorf("--file requires --target")
	}
	if f.targetPower != 0 && f.targetPower != 1 && f.targetPower != 2 {
		return usageErrorf("--target-power must be 1 (internal 5V) or 2 (external)")
	}
	if fromBuild && serverSide {
		return f.flashOnServer(g, spec, art, pins)
	}
	// 1. Identify OpenOCD. 2. Validate it actually runs.
	bin := strings.TrimSpace(f.openocdBin)
	if bin == "" {
		found, err := openocd.Find()
		if err != nil {
			return fmt.Errorf("flash: %w", err)
		}
		bin = found
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashTimeout))
	defer cancel()
	defer installSignalHandler(ctx, cancel)()

	if err := openocd.Validate(ctx, bin); err != nil {
		return fmt.Errorf("flash: %w", err)
	}

	// The CMSIS-DAP path needs the cmsis_dap_tcp backend, which only landed in
	// OpenOCD after 0.12.0. Check now (config-stage probe, no hardware touched) so
	// an old OpenOCD fails with actionable advice instead of a cryptic adapter error.
	ok, err := openocd.SupportsCMSISDAPTCP(ctx, bin)
	if err != nil {
		return fmt.Errorf("flash: %w", err)
	}
	if !ok {
		return fmt.Errorf("flash: this needs an OpenOCD build with the cmsis-dap TCP backend, but %s does not have it; update OpenOCD (e.g. `brew install --HEAD open-ocd`)", bin)
	}

	// 3. Put the pod into CMSIS-DAP probe mode over the chosen transport. The
	// returned connection carries the length-framed CMSIS-DAP stream; closing it
	// returns the pod to a safe state. The handshake gets its own short deadline
	// (a child of the flash ctx) so that if the pod never reports ready we abort
	// here, with the firmware's output, rather than handing a dead link to OpenOCD.
	hsCtx, hsCancel := context.WithTimeout(ctx, dapHandshakeTimeout)
	podConn, hasNRST, err := openDAP(hsCtx, g, pins, f.targetPower)
	hsCancel()
	if err != nil {
		return err
	}
	defer podConn.Close()
	f.hasNRST = hasNRST

	// Connect-under-reset holds the target in reset while OpenOCD reads the debug
	// port, so already-running firmware can't remap/disable SWD or sleep before we
	// connect. Two things must both be true: the user says the target's reset is
	// wired (--nreset), and the pod actually has a reset pin to drive it with.
	// Since rev3 that pin is always the same one, so --nreset is a yes/no rather
	// than the LA channel it used to be.
	connectUnderReset := !f.noConnectUnderReset && f.nreset && hasNRST
	switch {
	case f.nreset && !hasNRST:
		fmt.Fprintln(os.Stderr, "flash: NRST — this pod has no reset pin (pre-rev3 board), so --nreset has nothing to drive; connecting without reset")
	case connectUnderReset:
		fmt.Fprintf(os.Stderr, "flash: NRST — driving the pod's reset pin, %s (%s)\n", nrstPinLocation, nrstPinDocsURL)
		fmt.Fprintln(os.Stderr, "flash: connecting under reset (NRST held asserted through the debug-port read)")
	case f.nreset:
		fmt.Fprintf(os.Stderr, "flash: NRST — driving the pod's reset pin, %s (%s)\n", nrstPinLocation, nrstPinDocsURL)
		fmt.Fprintln(os.Stderr, "flash: connect-under-reset disabled (--no-connect-under-reset)")
	}

	// 4. Build the OpenOCD argument list (excluding the cmsis-dap adapter config,
	// which BridgeDAP prepends), now that we know whether reset is available.
	ocArgs, err := buildOpenOCDArgs(f.target, f.file, f.loadAddr, f.noVerify, f.noReset, connectUnderReset, !f.keepResetInit, f.extraConfigs, f.extraArgs)
	if err != nil {
		return err
	}

	// 5. Run OpenOCD bridged to the pod connection over its cmsis-dap TCP backend
	// (BridgeDAP translates the per-packet framing). Its exit code is the verdict.
	if err := openocd.BridgeDAPTransport(ctx, bin, pins.transport(), ocArgs, podConn, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, openocd.ErrTargetUnreachable) {
			printTargetUnreachableHint(f, pins.jtag)
		}
		return fmt.Errorf("flash: %w", err)
	}
	fmt.Fprintln(os.Stderr, "flash: success")
	return nil
}

// fillFromWiring takes the SWD settings left out of the flags from the pod's wiring profile on
// embeddedci.com, and logs where each one came from.
func (f *flashFlags) fillFromWiring(g *globalFlags) {
	needed := f.swclk == "" || f.swdio == "" || f.tck == "" || f.tms == "" || f.tdi == "" || f.tdo == "" || f.trst == "" || !f.nresetSet || f.target == ""
	var w *podWiring
	if needed {
		w = loadPodWiring(g, os.Stderr)
	}
	if w == nil {
		log.Printf("flash: settings from the flags")
		return
	}
	var from []string
	for _, jp := range []struct {
		flag *string
		pin  *int
		name string
	}{{&f.tck, w.JtagTck, "TCK"}, {&f.tms, w.JtagTms, "TMS"}, {&f.tdi, w.JtagTdi, "TDI"}, {&f.tdo, w.JtagTdo, "TDO"}, {&f.trst, w.JtagTrst, "TRST"}} {
		if pinFromWiring(jp.flag, jp.pin) {
			from = append(from, jp.name+" LA"+*jp.flag)
		}
	}
	if pinFromWiring(&f.swclk, w.SwdSwclk) {
		from = append(from, "SWCLK LA"+f.swclk)
	}
	if pinFromWiring(&f.swdio, w.SwdSwdio) {
		from = append(from, "SWDIO LA"+f.swdio)
	}
	if !f.nresetSet && w.SwdNreset != nil {
		f.nreset = *w.SwdNreset
		from = append(from, fmt.Sprintf("nreset %v", f.nreset))
	}
	if f.target == "" && strings.TrimSpace(w.SwdTarget) != "" {
		f.target = strings.TrimSpace(w.SwdTarget)
		from = append(from, "target "+f.target)
	}
	if len(from) > 0 {
		log.Printf("flash: %s from %s; the other settings from the flags", strings.Join(from, ", "), wiringSource)
	} else {
		log.Printf("flash: settings from the flags")
	}
}

// printTargetUnreachableHint explains a failed DAP connect: the pod's probe is
// working but no target answered on SWD. The advice is ordered by likelihood and
// references the exact pins/flags the user passed.
func printTargetUnreachableHint(f *flashFlags, jtag bool) {
	if jtag {
		fmt.Fprintln(os.Stderr, "\nThe pod's JTAG probe is working, but the target never answered. Check, in order:")
	} else {
		fmt.Fprintln(os.Stderr, "\nThe pod's SWD probe is working, but the target MCU never answered. Check, in order:")
	}
	if f.targetPower == 0 {
		fmt.Fprintln(os.Stderr, "  • power — the target must be powered. The pod can do it: re-run with --target-power 1 (internal 5V) or 2 (external)")
	} else {
		fmt.Fprintf(os.Stderr, "  • power — eFuse %d was enabled; confirm the target actually receives voltage (check for an over-current fault)\n", f.targetPower)
	}
	if jtag {
		fmt.Fprintf(os.Stderr, "  • wiring — TCK (%s), TMS (%s), TDI (%s) and TDO (%s) must reach the target's TCK/TMS/TDI/TDO pins (TDI and TDO not swapped), with a common ground\n", f.tck, f.tms, f.tdi, f.tdo)
	} else {
		fmt.Fprintf(os.Stderr, "  • wiring — SWCLK (%s) and SWDIO (%s) must reach the target's SWCLK/SWDIO pins (not swapped), with a common ground\n", f.swclk, f.swdio)
	}
	switch {
	case !f.hasNRST:
		fmt.Fprintln(os.Stderr, "  • running firmware — this pod has no reset pin, so if the target already runs firmware that remaps/disables SWD or sleeps, there is no way to connect under reset. A rev3 pod has a dedicated NRST pin")
	case !f.nreset:
		fmt.Fprintf(os.Stderr, "  • running firmware — if the target already runs firmware that remaps/disables SWD or sleeps, wire its NRST to the pod's reset pin (%s, %s) and pass --nreset; the pod then connects under reset\n", nrstPinLocation, nrstPinDocsURL)
	case f.noConnectUnderReset:
		fmt.Fprintln(os.Stderr, "  • running firmware — connect-under-reset is disabled (--no-connect-under-reset); drop that flag to hold NRST asserted while connecting")
	default:
		fmt.Fprintf(os.Stderr, "  • reset wiring — connect-under-reset is on, so confirm the target's NRST really reaches the pod's reset pin, %s (%s)\n", nrstPinLocation, nrstPinDocsURL)
	}
}

// openDAP optionally powers the target, then arms the pod's CMSIS-DAP probe (dap_start over
// the network, or over USB on the pod's USB network adapter) and returns the length-framed DAP
// stream to bridge to OpenOCD.
// powerEfuse 0 leaves target power untouched; 1/2 enables that eFuse first.
func openDAP(ctx context.Context, g *globalFlags, pins dapPins, powerEfuse int) (io.ReadWriteCloser, bool, error) {
	spec, err := g.resolveConnection()
	if err != nil {
		return nil, false, err
	}
	spec = g.preferUSBNetwork(spec, "flash")
	if spec.IsNetwork() {
		client := &tcpclient.Client{Addr: spec.Addr}
		// Ask the pod whether it has the dedicated reset pin BEFORE arming the
		// probe — once the connection is in DAP mode it no longer speaks JSON.
		// A probe failure is not fatal: assume the pin is there (every current
		// board has it) and let the real failure surface at dap_start.
		if pins.jtag {
			if err := requireJTAGFeature(ctx, client); err != nil {
				return nil, false, err
			}
		}
		hasNRST := true
		if v, err := client.HasNRSTPin(ctx); err == nil {
			hasNRST = v
		}
		if powerEfuse != 0 {
			if err := client.TargetPower(ctx, powerEfuse, true); err != nil {
				return nil, false, fmt.Errorf("flash: enable target-power eFuse %d: %w", powerEfuse, err)
			}
			fmt.Fprintf(os.Stderr, "flash: target-power eFuse %d on\n", powerEfuse)
			settle(ctx, targetPowerSettle)
		}
		opts := dapOptions(powerEfuse != 0)
		swclk, swdio := pins.swclk, pins.swdio
		if pins.jtag {
			jp := pins.jtagPins
			opts.JTAG, opts.Port = &jp, "jtag"
			swclk, swdio = 0, 0
		}
		conn, err := client.DAPStartWith(ctx, swclk, swdio, opts)
		if err != nil {
			return nil, false, fmt.Errorf("flash: dap_start: %w", err)
		}
		return conn, hasNRST, nil
	}

	// USB without a usable USB network adapter: firmware 4.0 has no probe on its console.
	_, err = g.networkTarget("flash")
	return nil, false, err
}

// settle waits for d or until ctx is cancelled, whichever comes first, so the
// target-power boot delay still honors the handshake deadline.
func settle(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// clearResetEventsTCL empties the loaded target's reset-start/reset-init events.
// Stock STM32 (and similar) cfgs use these to boost the core clock for faster
// flashing — e.g. stm32f4x.cfg's reset-init reprograms the PLL and calls
// `adapter speed N`. Over the pod's SWD link both are harmful:
// `adapter speed` aborts the reset ("Translation from khz to adapter speed not
// implemented"), and the PLL writes fire microseconds after reset-deassert while
// the core is still coming up, which races/glitches the slow SWD link and the
// programming intermittently fails. Cleared, the internal `reset init` of the
// `program` command becomes a clean reset-halt: flashing runs at the default
// reset clock — slower but reliable. Default reset latency (0 wait states at the
// HSI clock) is correct, so nothing essential is lost for flashing. Runs after
// the target cfg is sourced and before the program command triggers the reset.
// __-prefixed locals avoid clobbering cfg globals.
const clearResetEventsTCL = `foreach __t [target names] {
    foreach __ev {reset-start reset-init} {
        if {[catch {$__t cget -event $__ev}]} { continue }
        $__t configure -event $__ev {}
    }
}`

// buildOpenOCDArgs assembles the OpenOCD args for a flash run: an optional
// target config, an auto-built program command when a file is given, then any
// passthrough configs/args appended last so users can extend or override.
var loadAddrRe = regexp.MustCompile(`^(0[xX][0-9a-fA-F]{1,8}|[0-9]{1,10})$`)

// tclWord is path as one Tcl word with no substitution: in braces. Backslashes (Windows) become
// forward slashes, which OpenOCD takes on Windows too; a path with a brace or a newline is refused.
func tclWord(path string) (string, error) {
	path = strings.ReplaceAll(path, `\`, "/")
	if strings.ContainsAny(path, "{}\n\r") {
		return "", fmt.Errorf("the firmware path %q has characters OpenOCD would read as commands", path)
	}
	return "{" + path + "}", nil
}

func buildOpenOCDArgs(target, file, loadAddr string, noVerify, noReset, connectUnderReset, clearResetEvents bool, extraConfigs, extraArgs []string) ([]string, error) {
	target = strings.TrimSpace(target)
	file = strings.TrimSpace(file)

	if target == "" && len(extraConfigs) == 0 && len(extraArgs) == 0 {
		return nil, fmt.Errorf("nothing to do; pass --target (with --file) or a passthrough -c/--openocd-arg")
	}

	var ocArgs []string
	if target != "" {
		ocArgs = append(ocArgs, "-f", target)
	}
	// Connect-under-reset: srst_only enables the SRST base mode (SWD targets have
	// no TRST), srst_nogate keeps SRST usable during the connect, and
	// connect_assert_srst holds it asserted through the first debug-port read.
	// connect_assert_srst alone is rejected ("BUG: can't assert SRST") unless the
	// base mode declares SRST. This must follow the target cfg (which sources its
	// own reset_config) but precede the program command, which triggers init.
	if connectUnderReset {
		ocArgs = append(ocArgs, "-c", "reset_config srst_only srst_nogate connect_assert_srst")
	}
	// Clear the target cfg's clock-boost reset events now that it is sourced,
	// before the program command runs the reset.
	if target != "" && clearResetEvents {
		ocArgs = append(ocArgs, "-c", clearResetEventsTCL)
	}
	if file != "" {
		// The program command is Tcl: "[...]" in a file name or address would run as a command.
		// The path goes in braces (no substitution inside); the address must be a number.
		word, err := tclWord(file)
		if err != nil {
			return nil, err
		}
		prog := []string{"program", word}
		if a := strings.TrimSpace(loadAddr); a != "" {
			if !loadAddrRe.MatchString(a) {
				return nil, usageErrorf("--load-address must be a number like 0x08000000, not %q", loadAddr)
			}
			prog = append(prog, a)
		}
		if !noVerify {
			prog = append(prog, "verify")
		}
		if !noReset {
			prog = append(prog, "reset")
		}
		prog = append(prog, "exit")
		ocArgs = append(ocArgs, "-c", strings.Join(prog, " "))
	}
	for _, a := range extraArgs {
		ocArgs = append(ocArgs, a)
	}
	for _, c := range extraConfigs {
		ocArgs = append(ocArgs, "-c", c)
	}
	return ocArgs, nil
}
