package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/openocd"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
	"github.com/spf13/cobra"
)

// flashTimeout bounds a whole flash session; flashing over the WiFi/AT bridge
// (or the USB serial console) is slow, so this is generous compared to the 30s
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

// flashFlags holds the flash-specific (non-global) flags.
type flashFlags struct {
	swclk, swdio, nreset string
	target, file         string
	loadAddr             string
	noVerify, noReset    bool
	noConnectUnderReset  bool
	keepResetInit        bool
	openocdBin           string
	targetPower          int
	extraConfigs         []string
	extraArgs            []string
}

// newFlashCmd builds the flash subcommand. It flashes an SWD target wired to the
// pod's LA pins (1-12, mapped to the ice40/FPGA) by arming the pod's CMSIS-DAP
// probe, then running host-side OpenOCD bridged over the chosen transport. The
// pod holds no flash intelligence; OpenOCD's exit code is the verdict.
//
// The pod runs a CMSIS-DAP processor locally and OpenOCD's cmsis-dap TCP backend
// ships whole DAP transfers (DAP_Transfer / DAP_TransferBlock), collapsing
// thousands of per-bit round-trips into one per DAP command. --connection wifi
// uses the TCP dap_start handshake (tcpclient.DAPStart); --connection serial / a
// device path uses the console dap-start handshake (serialconsole.DAPStart). Both
// need a recent OpenOCD with the cmsis_dap_tcp backend.
func newFlashCmd(g *globalFlags) *cobra.Command {
	f := &flashFlags{}
	cmd := &cobra.Command{
		Use:   "flash",
		Short: "Flash an SWD target via OpenOCD's CMSIS-DAP backend (wifi or serial)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runFlash(g, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.swclk, "swclk", "", "LA pin for SWCLK, 1-12, e.g. 1 or la1 (required)")
	fl.StringVar(&f.swdio, "swdio", "", "LA pin for SWDIO, 1-12, e.g. 2 or la2 (required)")
	fl.StringVar(&f.nreset, "nreset", "", "optional LA pin for target reset (active-low), 1-12")
	fl.StringVar(&f.target, "target", "", "OpenOCD target config (passed as -f), e.g. target/stm32f1x.cfg")
	fl.StringVar(&f.file, "file", "", "firmware image to flash (used with --target)")
	fl.StringVar(&f.loadAddr, "load-address", "", "load address for a raw .bin image (appended to the program command)")
	fl.BoolVar(&f.noVerify, "no-verify", false, "do not verify after programming")
	fl.BoolVar(&f.noReset, "no-reset", false, "do not reset the target after programming")
	fl.BoolVar(&f.noConnectUnderReset, "no-connect-under-reset", false, "do not hold the target in reset while connecting (connect-under-reset is on by default when --nreset is set; it stops already-running firmware from disabling SWD before the debug port is read)")
	fl.BoolVar(&f.keepResetInit, "keep-reset-init", false, "keep the target cfg's reset-init/reset-start events (clock boost); by default they are cleared because the just-after-reset clock writes glitch the slow bit-banged link (flashing then runs at the default reset clock — slower but reliable)")
	fl.StringVar(&f.openocdBin, "openocd", "", "path to the openocd binary; defaults to the one on PATH")
	fl.IntVar(&f.targetPower, "target-power", 0, "enable a target power eFuse before flashing: 1 (internal 5V) or 2 (external)")
	fl.StringArrayVarP(&f.extraConfigs, "command", "c", nil, "extra OpenOCD command, repeatable (passthrough, appended last)")
	fl.StringArrayVar(&f.extraArgs, "openocd-arg", nil, "extra raw OpenOCD argument, repeatable (passthrough, appended last)")
	return cmd
}

func runFlash(g *globalFlags, f *flashFlags) error {
	if strings.TrimSpace(f.swclk) == "" || strings.TrimSpace(f.swdio) == "" {
		return fmt.Errorf("--swclk and --swdio are required")
	}
	swclk, err := parseLAPin(f.swclk)
	if err != nil {
		return fmt.Errorf("--swclk: %w", err)
	}
	swdio, err := parseLAPin(f.swdio)
	if err != nil {
		return fmt.Errorf("--swdio: %w", err)
	}
	if swclk == swdio {
		return fmt.Errorf("--swclk and --swdio must be different pins")
	}
	var nresetPtr *int
	if strings.TrimSpace(f.nreset) != "" {
		nreset, err := parseLAPin(f.nreset)
		if err != nil {
			return fmt.Errorf("--nreset: %w", err)
		}
		nresetPtr = &nreset
	}
	if strings.TrimSpace(f.file) != "" && strings.TrimSpace(f.target) == "" {
		return fmt.Errorf("--file requires --target")
	}
	if f.targetPower != 0 && f.targetPower != 1 && f.targetPower != 2 {
		return fmt.Errorf("--target-power must be 1 (internal 5V) or 2 (external)")
	}

	// Connect-under-reset holds the target in reset while OpenOCD reads the debug
	// port, so already-running firmware can't remap/disable SWD or sleep before we
	// connect. It needs a wired reset line, so it's only meaningful with --nreset.
	connectUnderReset := !f.noConnectUnderReset && nresetPtr != nil
	if connectUnderReset {
		fmt.Fprintln(os.Stderr, "flash: connecting under reset (NRST held asserted through the debug-port read)")
	}

	// Build the OpenOCD argument list (excluding the cmsis-dap adapter config,
	// which BridgeDAP prepends).
	ocArgs, err := buildOpenOCDArgs(f.target, f.file, f.loadAddr, f.noVerify, f.noReset, connectUnderReset, !f.keepResetInit, f.extraConfigs, f.extraArgs)
	if err != nil {
		return err
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
	podConn, err := openDAP(hsCtx, g, swclk, swdio, nresetPtr, f.targetPower)
	hsCancel()
	if err != nil {
		return err
	}
	defer podConn.Close()

	// 4. Run OpenOCD bridged to the pod connection over its cmsis-dap TCP backend
	// (BridgeDAP translates the per-packet framing). Its exit code is the verdict.
	if err := openocd.BridgeDAP(ctx, bin, ocArgs, podConn, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, openocd.ErrTargetUnreachable) {
			printTargetUnreachableHint(f)
		}
		return fmt.Errorf("flash: %w", err)
	}
	fmt.Fprintln(os.Stderr, "flash: success")
	return nil
}

// printTargetUnreachableHint explains a failed DAP connect: the pod's probe is
// working but no target answered on SWD. The advice is ordered by likelihood and
// references the exact pins/flags the user passed.
func printTargetUnreachableHint(f *flashFlags) {
	fmt.Fprintln(os.Stderr, "\nThe pod's SWD probe is working, but the target MCU never answered. Check, in order:")
	if f.targetPower == 0 {
		fmt.Fprintln(os.Stderr, "  • power — the target must be powered. The pod can do it: re-run with --target-power 1 (internal 5V) or 2 (external)")
	} else {
		fmt.Fprintf(os.Stderr, "  • power — eFuse %d was enabled; confirm the target actually receives voltage (check for an over-current fault)\n", f.targetPower)
	}
	fmt.Fprintf(os.Stderr, "  • wiring — SWCLK (%s) and SWDIO (%s) must reach the target's SWCLK/SWDIO pins (not swapped), with a common ground\n", f.swclk, f.swdio)
	switch {
	case strings.TrimSpace(f.nreset) == "":
		fmt.Fprintln(os.Stderr, "  • running firmware — if the target already runs firmware that remaps/disables SWD or sleeps, wire its NRST to an LA pin and pass --nreset; the pod then connects under reset by default")
	case f.noConnectUnderReset:
		fmt.Fprintln(os.Stderr, "  • running firmware — connect-under-reset is disabled (--no-connect-under-reset); drop that flag to hold NRST asserted while connecting")
	default:
		fmt.Fprintln(os.Stderr, "  • reset wiring — connect-under-reset is on, so confirm --nreset really reaches the target's NRST pin and is active-low")
	}
}

// openDAP optionally powers the target, then arms the pod's CMSIS-DAP probe over
// the selected transport (dap_start over wifi/TCP, the dap-start console command
// over serial) and returns the length-framed DAP stream to bridge to OpenOCD.
// powerEfuse 0 leaves target power untouched; 1/2 enables that eFuse first.
func openDAP(ctx context.Context, g *globalFlags, swclk, swdio int, nreset *int, powerEfuse int) (io.ReadWriteCloser, error) {
	spec, err := g.resolveConnection()
	if err != nil {
		return nil, err
	}
	if spec.IsWifi() {
		client := &tcpclient.Client{Addr: spec.Addr}
		if powerEfuse != 0 {
			if err := client.TargetPower(ctx, powerEfuse, true); err != nil {
				return nil, fmt.Errorf("flash: enable target-power eFuse %d: %w", powerEfuse, err)
			}
			fmt.Fprintf(os.Stderr, "flash: target-power eFuse %d on\n", powerEfuse)
			settle(ctx, targetPowerSettle)
		}
		conn, err := client.DAPStart(ctx, swclk, swdio, nreset)
		if err != nil {
			return nil, fmt.Errorf("flash: dap_start: %w", err)
		}
		return conn, nil
	}

	// Serial transport: open the console, optionally power the target, perform
	// the dap-start handshake, and hand back the framed stream. DAPStart's
	// returned closer owns the port (its Close sends a leave frame and closes it),
	// so we only close the console explicitly when we bail out before the
	// handshake succeeds. Same auto-detect as the other serial commands: explicit
	// device verbatim, else probe USB serial ports for the bench-pod identifier.
	console, path, err := g.openBenchpodSerial(spec.Device, 2*time.Second)
	if err != nil {
		return nil, err
	}
	if powerEfuse != 0 {
		if err := console.TargetPower(ctx, powerEfuse, true); err != nil {
			console.Close()
			return nil, fmt.Errorf("flash: enable target-power eFuse %d over %s: %w", powerEfuse, path, err)
		}
		fmt.Fprintf(os.Stderr, "flash: target-power eFuse %d on\n", powerEfuse)
		settle(ctx, targetPowerSettle)
	}
	conn, err := console.DAPStart(ctx, swclk, swdio, nreset)
	if err != nil {
		console.Close()
		return nil, fmt.Errorf("flash: dap-start over %s: %w", path, err)
	}
	return conn, nil
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
		prog := []string{"program", file}
		if a := strings.TrimSpace(loadAddr); a != "" {
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
