package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/spf13/cobra"
)

// flash-self writes the bench pod's OWN firmware over USB, with no SWD probe, by
// driving the device's ROM USB DFU bootloader through dfu-util. This is the
// STM32 counterpart to the RP2350 drag-and-drop UF2 flow. (The separate `flash`
// command, by contrast, flashes a target/DUT wired to the pod's SWD pins.)
//
// A first-time user runs `benchpod flash-self` with NO arguments: the prebuilt
// firmware is fetched from the public release repo, the pod is detected in DFU
// mode (with on-screen guidance if it isn't yet), and dfu-util writes it — no
// toolchain, no manual download. A path or URL argument overrides the fetch.

// flashSelfTimeout bounds a whole flash session (download + wait + write).
const flashSelfTimeout = 5 * time.Minute

// stmDfuBaseAddr is the STM32 main-flash base address; firmware is written here.
const stmDfuBaseAddr = "0x08000000"

// stmDfuVidPid is how the STM32 ROM bootloader enumerates (ST DFU).
const stmDfuVidPid = "0483:df11"

// defaultFirmwareRepo is the PUBLIC GitHub repo that hosts prebuilt bench pod
// firmware binaries (the firmware source repo is private). defaultFirmwareAsset
// is the STM32 release asset name. Together they form the default fetch URL.
const (
	defaultFirmwareRepo  = "embeddedci-com/benchpod-firmware"
	defaultFirmwareAsset = "bench_pod_stm32.bin"
)

// Test seams over os/exec and net/http.
var (
	flashSelfLookPath       = exec.LookPath
	flashSelfCommandContext = exec.CommandContext
	flashSelfHTTPClient     = http.DefaultClient
)

// flashSelfFlags holds the flash-self-specific (non-global) flags.
type flashSelfFlags struct {
	address     string
	dfuUtil     string
	noLeave     bool
	enterDfu    bool
	wait        time.Duration
	firmwareURL string
	firmwareVer string
	skipBlobs   bool
	blobsDir    string
}

// firmwareReleaseURL returns the GitHub release download URL for the STM32
// firmware: the latest release by default, or a specific tag when pinned.
func firmwareReleaseURL(version string) string {
	v := strings.TrimSpace(version)
	if v == "" || v == "latest" {
		return fmt.Sprintf("https://github.com/%s/releases/latest/download/%s", defaultFirmwareRepo, defaultFirmwareAsset)
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", defaultFirmwareRepo, v, defaultFirmwareAsset)
}

// isHTTPURL reports whether s is an http(s) URL (vs a local file path).
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// dfuUtilDownloadArgs builds the dfu-util argument list to write `file` to the
// dfuLeaveBenign reports whether a failing dfu-util run in fact flashed the pod. With `:leave`
// the bootloader starts the new firmware at once, so dfu-util's final status poll finds no
// device ("Error during download get_status") and it exits 74, after "File downloaded
// successfully". That run succeeded; anything else did not.
func dfuLeaveBenign(leave bool, output string) bool {
	return leave && strings.Contains(output, "File downloaded successfully")
}

// STM32 main flash. The trailing `:leave` makes the bootloader start the new
// firmware once the download verifies, instead of staying in DFU.
func dfuUtilDownloadArgs(address string, leave bool, file string) []string {
	addr := address
	if leave {
		addr += ":leave"
	}
	return []string{"-a", "0", "--dfuse-address", addr, "-D", file}
}

func newFlashSelfCmd(g *globalFlags) *cobra.Command {
	f := &flashSelfFlags{}
	cmd := &cobra.Command{
		Use:   "flash-self [firmware.bin | URL]",
		Short: "Flash the pod's own firmware over USB DFU (STM32, no SWD probe)",
		Long: "Flash the pod's OWN firmware over USB using its ROM DFU bootloader\n" +
			"(via dfu-util) — the STM32 counterpart of the RP2350 drag-and-drop UF2 flow.\n" +
			"This is NOT the same as `flash`, which programs a target wired to the pod.\n\n" +
			"With no argument the latest prebuilt firmware is fetched automatically from\n" +
			"the public release repo — no toolchain or manual download needed. Pass a\n" +
			"local .bin path or an http(s) URL to flash that instead.\n\n" +
			"The pod must be in its DFU bootloader; if it isn't, this prints how to get\n" +
			"there and waits (use --enter-dfu to reboot a running pod automatically).\n\n" +
			"The release's signature (bench_pod_stm32.bin.sig, or <file>.sig next to a local\n" +
			"file) is checked and reported before writing; for now it never stops a flash.\n\n" +
			"Requires dfu-util on PATH (`brew install dfu-util`; the Homebrew cask pulls\n" +
			"it in automatically).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if _, err := g.usbDevice("flash-self"); err != nil {
				return err
			}
			bin := strings.TrimSpace(f.dfuUtil)
			if bin == "" {
				bin = "dfu-util"
			}
			dfuPath, err := flashSelfLookPath(bin)
			if err != nil {
				return fmt.Errorf("dfu-util not found on PATH — install it with `brew install dfu-util`: %w", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashSelfTimeout))
			defer cancel()

			// 1. Resolve the firmware image (local file, explicit URL, or the
			//    default latest release), downloading to a temp file if needed.
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			file, location, cleanup, err := f.resolveFirmware(ctx, arg)
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return err
			}
			// Report on the release's signature (<asset>.sig). Report only: never refuses.
			checkFirmwareSignature(ctx, location, file)

			// 2. Make sure the pod is in DFU mode (guiding the user if not).
			if err := ensureDfuReady(ctx, g, f, dfuPath); err != nil {
				return err
			}

			// 3. Refuse an image that does not fit this pod's flash (a 1 MB H563 cannot take
			//    firmware built for the 2 MB part).
			if err := checkImageFitsPod(ctx, dfuPath, file); err != nil {
				return err
			}

			// 4. Hand off to dfu-util.
			dfuArgs := dfuUtilDownloadArgs(f.address, !f.noLeave, file)
			fmt.Fprintf(os.Stderr, "flash-self: flashing — %s %s\n", dfuPath, strings.Join(dfuArgs, " "))
			c := flashSelfCommandContext(ctx, dfuPath, dfuArgs...)
			var out bytes.Buffer // also kept, to tell a real failure from the :leave one
			c.Stdout = io.MultiWriter(os.Stdout, &out)
			c.Stderr = io.MultiWriter(os.Stderr, &out)
			if err := c.Run(); err != nil && !dfuLeaveBenign(!f.noLeave, out.String()) {
				return fmt.Errorf("dfu-util: %w", err)
			}
			if f.noLeave {
				fmt.Fprintln(os.Stderr, "flash-self: success — firmware written (device left in DFU mode)")
			} else {
				fmt.Fprintln(os.Stderr, "flash-self: success, the pod is rebooting into the new firmware.")
				if !f.skipBlobs {
					// 5. The gateware and ESP32-C3 images live in the pod's W25Q, not in the
					//    firmware: install the ones that go with it over the same cable.
					src := blobSourceForFirmware(arg, f.firmwareURL, f.firmwareVer)
					if strings.TrimSpace(f.blobsDir) != "" {
						src = blobSource{base: f.blobsDir}
					}
					return installBlobsAfterFlash(g, src)
				}
				fmt.Fprintln(os.Stderr, "  If `benchpod discover` does not find it within 30 s, unplug USB-C and plug it back in (without BOOT0).")
			}
			return nil
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.address, "address", stmDfuBaseAddr, "flash base address (STM32 main flash)")
	fl.StringVar(&f.dfuUtil, "dfu-util", "", "path to the dfu-util binary; defaults to the one on PATH")
	fl.BoolVar(&f.noLeave, "no-leave", false, "stay in DFU after flashing instead of starting the new firmware")
	fl.BoolVar(&f.enterDfu, "enter-dfu", false, "first reboot a running pod into DFU over the USB console, then flash")
	fl.DurationVar(&f.wait, "wait", 60*time.Second, "how long to wait for the pod to enter / re-enumerate in DFU mode")
	fl.StringVar(&f.firmwareURL, "firmware-url", "", "override the firmware download URL (default: the latest public release)")
	fl.StringVar(&f.firmwareVer, "firmware-version", "", "fetch a specific firmware release tag instead of the latest")
	fl.BoolVar(&f.skipBlobs, "skip-blobs", false, "do not install the gateware and ESP32-C3 images after flashing")
	fl.StringVar(&f.blobsDir, "blobs-dir", "", "take the blobs from this directory instead of next to the firmware")
	return cmd
}

// resolveFirmware turns the optional argument into a local file path to flash.
// Precedence: explicit local file or URL argument > --firmware-url > the default
// release URL (latest, or --firmware-version). Anything fetched lands in a temp
// file; the returned cleanup (may be nil) removes it. location is where the image came from
// (the path or the URL), for finding its .sig.
func (f *flashSelfFlags) resolveFirmware(ctx context.Context, arg string) (file, location string, cleanup func(), err error) {
	arg = strings.TrimSpace(arg)

	// An explicit local file path wins outright.
	if arg != "" && !isHTTPURL(arg) {
		if _, err := os.Stat(arg); err != nil {
			return "", "", nil, fmt.Errorf("firmware file: %w", err)
		}
		fmt.Fprintf(os.Stderr, "flash-self: using firmware %s\n", arg)
		return arg, arg, nil, nil
	}

	// Otherwise pick a URL to download from.
	url := arg
	switch {
	case url != "": // explicit URL argument
	case strings.TrimSpace(f.firmwareURL) != "":
		url = strings.TrimSpace(f.firmwareURL)
	default:
		url = firmwareReleaseURL(f.firmwareVer)
	}

	which := "latest release"
	if v := strings.TrimSpace(f.firmwareVer); v != "" && v != "latest" {
		which = "release " + v
	}
	if arg != "" || strings.TrimSpace(f.firmwareURL) != "" {
		which = "" // a custom URL — don't claim it's "the latest release"
	}
	if which != "" {
		fmt.Fprintf(os.Stderr, "flash-self: fetching the %s firmware (%s)\n", which, defaultFirmwareRepo)
	}

	path, err := downloadFirmware(ctx, url)
	if err != nil {
		return "", "", nil, err
	}
	return path, url, func() { _ = os.Remove(path) }, nil
}

// downloadFirmware fetches url into a temp file and best-effort verifies a
// companion `<url>.sha256` if the release publishes one.
func downloadFirmware(ctx context.Context, url string) (string, error) {
	fmt.Fprintf(os.Stderr, "flash-self: downloading %s\n", url)
	body, err := httpGet(ctx, url)
	if err != nil {
		return "", err
	}
	defer body.Close()

	tmp, err := os.CreateTemp("", "bench_pod_stm32-*.bin")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	hasher := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(body, hasher))
	closeErr := tmp.Close()
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if closeErr != nil {
		_ = os.Remove(tmp.Name())
		return "", closeErr
	}
	if n == 0 {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("download %s: empty file", url)
	}
	fmt.Fprintf(os.Stderr, "flash-self: downloaded %s (%d KB)\n", defaultFirmwareAsset, (n+1023)/1024)

	if err := verifyChecksum(ctx, url, hex.EncodeToString(hasher.Sum(nil))); err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// verifyChecksum fetches `<url>.sha256` and compares it to got. A 404 (no
// checksum published) is not an error; a mismatch is.
func verifyChecksum(ctx context.Context, url, got string) error {
	body, err := httpGet(ctx, url+".sha256")
	if err != nil {
		// No checksum asset (or it's unreachable) — skip rather than fail.
		return nil
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 256))
	if err != nil {
		return nil
	}
	// Files are typically "<hex>  <name>"; take the first field.
	want := strings.ToLower(strings.TrimSpace(string(raw)))
	if i := strings.IndexAny(want, " \t"); i > 0 {
		want = want[:i]
	}
	if want == "" {
		return nil
	}
	if want != strings.ToLower(got) {
		return fmt.Errorf("firmware checksum mismatch: got %s, expected %s", got, want)
	}
	fmt.Fprintln(os.Stderr, "flash-self: verified SHA-256 checksum")
	return nil
}

// httpGet issues a GET and returns the response body on HTTP 200, following
// redirects (GitHub's releases/latest/download is a redirect to the asset).
func httpGet(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := flashSelfHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %s", url, resp.Status)
	}
	return resp.Body, nil
}

// ensureDfuReady makes sure an STM32 DFU device is present before flashing,
// guiding the user (and waiting) when it isn't.
func ensureDfuReady(ctx context.Context, g *globalFlags, f *flashSelfFlags, dfuPath string) error {
	if f.enterDfu {
		if err := enterDfuViaConsole(g); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "flash-self: waiting up to %s for the DFU device (%s) to re-enumerate...\n", f.wait, stmDfuVidPid)
		return waitForDfuDevice(ctx, dfuPath, f.wait)
	}
	if dfuDevicePresent(ctx, dfuPath) {
		fmt.Fprintln(os.Stderr, "flash-self: found the pod in DFU mode")
		return nil
	}
	printDfuInstructions()
	fmt.Fprintf(os.Stderr, "flash-self: waiting up to %s for the pod to enter DFU mode (Ctrl-C to abort)...\n", f.wait)
	return waitForDfuDevice(ctx, dfuPath, f.wait)
}

// printDfuInstructions tells a first-time user how to get the pod into DFU mode.
func printDfuInstructions() {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "The pod isn't in DFU mode yet. To enter it:")
	fmt.Fprintln(os.Stderr, "  • blank board / first flash: hold the BOOT0 button while you press RESET")
	fmt.Fprintln(os.Stderr, "    (or power-cycle the board), then release BOOT0.")
	fmt.Fprintln(os.Stderr, "  • a pod already running firmware: run `benchpod dfu` in another terminal,")
	fmt.Fprintln(os.Stderr, "    or re-run this command with --enter-dfu to do it for you.")
	fmt.Fprintln(os.Stderr, "")
}

// dfuDevicePresent runs `dfu-util -l` once and reports whether the STM32 ROM
// bootloader is currently enumerated.
func dfuDevicePresent(ctx context.Context, dfuPath string) bool {
	lc, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := flashSelfCommandContext(lc, dfuPath, "-l").CombinedOutput()
	return strings.Contains(string(out), stmDfuVidPid)
}

// enterDfuViaConsole opens the serial console and sends the `dfu` command to
// reboot a running pod into its ROM USB DFU bootloader.
func enterDfuViaConsole(g *globalFlags) error {
	console, _, ctx, cancel, err := g.openSerialConsole(g.serialDevice(), g.effectiveTimeout(10*time.Second))
	if err != nil {
		return fmt.Errorf("enter-dfu: open USB console: %w", err)
	}
	defer cancel()
	defer console.Close()
	if err := console.Dfu(ctx); err != nil {
		return fmt.Errorf("enter-dfu: %w", err)
	}
	fmt.Fprintln(os.Stderr, "flash-self: device entering DFU; USB console disconnected (expected)")
	return nil
}

// waitForDfuDevice polls `dfu-util -l` until the STM32 ROM bootloader enumerates
// (or the deadline / context expires), so we don't race dfu-util against a USB
// port that is still re-enumerating.
func waitForDfuDevice(ctx context.Context, dfuPath string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		if dfuDevicePresent(ctx, dfuPath) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the STM32 DFU device (%s) to enumerate", wait, stmDfuVidPid)
		}
		select {
		case <-ctx.Done():
			return errors.New("cancelled waiting for the DFU device")
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// fwInfoOffset is where every firmware image carries its fw_info block (stm32h563/src/fw_info.h):
// magic "BPFW", layout, min_flash_kb.
const fwInfoOffset = 0x400

// imageNeedsKB reads the internal flash an image needs. Images from before the block existed
// were all built for the 2 MB part.
func imageNeedsKB(image []byte) int {
	if len(image) < fwInfoOffset+16 || string(image[fwInfoOffset:fwInfoOffset+4]) != "BPFW" {
		return 2048
	}
	kb := int(image[fwInfoOffset+6]) | int(image[fwInfoOffset+7])<<8
	if kb == 0 {
		return 2048
	}
	return kb
}

// dfuFlashKB reads the internal flash size from `dfu-util -l`: the ST bootloader names its
// flash alt setting like "@Internal Flash   /0x08000000/256*08Kg" (sector count * size). 0 when
// there is no such line.
func dfuFlashKB(listing string) int {
	i := strings.Index(listing, "/0x08000000/")
	if i < 0 {
		return 0
	}
	spec := listing[i+len("/0x08000000/"):]
	if j := strings.IndexAny(spec, "\"' \n"); j >= 0 {
		spec = spec[:j]
	}
	total := 0
	for _, part := range strings.Split(spec, ",") {
		var n, size int
		var unit byte
		if _, err := fmt.Sscanf(part, "%d*%d%c", &n, &size, &unit); err != nil {
			return 0
		}
		switch unit {
		case 'K':
			total += n * size
		case 'M':
			total += n * size * 1024
		case 'B', ' ':
			total += n * size / 1024
		default:
			return 0
		}
	}
	return total
}

// checkImageFitsPod refuses to DFU an image the pod in DFU mode cannot hold.
func checkImageFitsPod(ctx context.Context, dfuPath, file string) error {
	image, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("firmware file: %w", err)
	}
	need := imageNeedsKB(image)
	lc, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := flashSelfCommandContext(lc, dfuPath, "-l").CombinedOutput()
	have := dfuFlashKB(string(out))
	if have == 0 {
		fmt.Fprintln(os.Stderr, "flash-self: could not read the pod's flash size from dfu-util; not checking the image fits")
		return nil
	}
	if need > have {
		return fmt.Errorf("this firmware needs %d KB of flash and the pod has %d KB: it was built for the 2 MB H563; use a newer release", need, have)
	}
	return nil
}

// Seams for tests: how long installBlobsAfterFlash waits for the flashed pod's USB console,
// how often it tries, and how it opens it.
var (
	flashedConsoleWait  = 60 * time.Second
	flashedConsoleRetry = time.Second
	openFlashedConsole  = func(g *globalFlags) (*serialconsole.Console, error) {
		c, _, err := g.openBenchpodSerial(g.serialDevice(), 2*time.Second)
		return c, err
	}
)

// installBlobsAfterFlash waits for the freshly flashed pod's USB console, then installs the
// blobs that go with its firmware. A pod whose firmware still embeds its blobs (older releases)
// has no slots to fill and is left alone. Anything else that keeps the blobs from being
// installed is an error, so flash-self does not report success for a pod that is missing them.
func installBlobsAfterFlash(g *globalFlags, src blobSource) error {
	fmt.Fprintln(os.Stderr, "flash-self: waiting for the pod's USB console to install its blobs...")
	var console *serialconsole.Console
	deadline := time.Now().Add(flashedConsoleWait)
	for {
		c, err := openFlashedConsole(g)
		if err == nil {
			console = c
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("flash-self: the firmware is flashed, but the pod's USB console did not come back, so its blobs are not installed: unplug USB-C and plug it back in (without BOOT0), then run `benchpod install-blobs` (%w)", err)
		}
		time.Sleep(flashedConsoleRetry)
	}
	defer console.Close()
	ctx, cancel := context.WithTimeout(context.Background(), blobsInstallTimeout)
	defer cancel()
	if _, err := console.Blobs(ctx); err != nil {
		if errors.Is(err, serialconsole.ErrNoBlobSlots) {
			fmt.Fprintln(os.Stderr, "flash-self: this firmware keeps its blobs built in; nothing to install")
			return nil
		}
		return fmt.Errorf("flash-self: the firmware is flashed, but reading the pod's blob slots failed, so its blobs are not installed (retry with `benchpod install-blobs`): %w", err)
	}
	if err := installBlobs(ctx, console, src, false, nil); err != nil {
		return fmt.Errorf("flash-self: the firmware is flashed, but installing its blobs failed (retry with `benchpod install-blobs`): %w", err)
	}
	return nil
}
