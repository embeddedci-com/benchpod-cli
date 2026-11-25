package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/spf13/cobra"
)

// The blobs a firmware release goes with: the iCE40 gateware images (gw0 loop, gw1 deep replay
// and, from gateware v54, gw2 sensor emulation) and the ESP32-C3 esp-hosted image. The pod keeps
// them in slots of its W25Q flash rather than in the firmware image, so whoever installs firmware
// installs these next to it. A release publishes them as blob-gw0.bin, blob-gw1.bin,
// blob-gw2.bin, blob-esp.bin, blob-xip.bin, blob-xip_prog.bin plus blobs-manifest.json; a local firmware build
// leaves the same files in stm32h563/build/blobs/.

const blobsManifestName = "blobs-manifest.json"

// blobsInstallTimeout bounds a whole install: ~1.3 MB over USB plus the slot writes.
const blobsInstallTimeout = 10 * time.Minute

type blobEntry struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	Size    int    `json:"size"`
	SHA256  string `json:"sha256"`
	Version int    `json:"version"`
}

type blobsManifest struct {
	Format          int         `json:"format"`
	GatewareVersion int         `json:"gateware_version"`
	Blobs           []blobEntry `json:"blobs"`
}

// blobSource is where a manifest and its blob files live: a local directory or a URL prefix
// (a release download directory). Files are named relative to it.
type blobSource struct {
	base string
}

func (s blobSource) String() string { return s.base }

func (s blobSource) read(ctx context.Context, name string) ([]byte, error) {
	if isHTTPURL(s.base) {
		body, err := httpGet(ctx, strings.TrimRight(s.base, "/")+"/"+name)
		if err != nil {
			return nil, err
		}
		defer body.Close()
		return io.ReadAll(io.LimitReader(body, 16<<20))
	}
	return os.ReadFile(filepath.Join(s.base, name))
}

// releaseBlobSource is the download directory of a firmware release ("" or "latest" = the latest).
func releaseBlobSource(tag string) blobSource {
	t := strings.TrimSpace(tag)
	if t == "" || t == "latest" {
		return blobSource{base: fmt.Sprintf("https://github.com/%s/releases/latest/download", defaultFirmwareRepo)}
	}
	return blobSource{base: fmt.Sprintf("https://github.com/%s/releases/download/%s", defaultFirmwareRepo, t)}
}

// blobSourceForFirmware picks the blobs that go with a firmware image: next to a local build
// (build/blobs/ beside build/bench_pod_stm32.bin), or the same release/URL directory.
func blobSourceForFirmware(firmwareArg, firmwareURL, firmwareVer string) blobSource {
	arg := strings.TrimSpace(firmwareArg)
	switch {
	case arg != "" && !isHTTPURL(arg):
		return blobSource{base: filepath.Join(filepath.Dir(arg), "blobs")}
	case arg != "":
		return blobSource{base: arg[:strings.LastIndex(arg, "/")]}
	case strings.TrimSpace(firmwareURL) != "":
		u := strings.TrimSpace(firmwareURL)
		return blobSource{base: u[:strings.LastIndex(u, "/")]}
	default:
		return releaseBlobSource(firmwareVer)
	}
}

func loadBlobsManifest(ctx context.Context, src blobSource) (blobsManifest, error) {
	var m blobsManifest
	raw, err := src.read(ctx, blobsManifestName)
	if err != nil {
		return m, fmt.Errorf("blobs manifest from %s: %w", src, err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("blobs manifest from %s: %w", src, err)
	}
	return m, nil
}

// fetchBlob reads one blob and checks it against the manifest.
func fetchBlob(ctx context.Context, src blobSource, e blobEntry) ([]byte, error) {
	data, err := src.read(ctx, e.File)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", e.Name, err)
	}
	sum := sha256.Sum256(data)
	if len(data) != e.Size || !strings.EqualFold(hex.EncodeToString(sum[:]), e.SHA256) {
		return nil, fmt.Errorf("blob %s from %s does not match its manifest (%d bytes, want %d)", e.Name, src, len(data), e.Size)
	}
	return data, nil
}

// blobsToInstall picks the manifest entries a pod lacks: its slot is empty or holds something
// else. force installs every blob the manifest has.
func blobsToInstall(m blobsManifest, slots []serialconsole.BlobSlot, force bool) []blobEntry {
	have := map[string]serialconsole.BlobSlot{}
	for _, s := range slots {
		have[s.Name] = s
	}
	var out []blobEntry
	for _, e := range m.Blobs {
		if e.Size == 0 || e.File == "" {
			continue
		}
		s, ok := have[e.Name]
		if !ok {
			continue // a slot this firmware does not know
		}
		if force || !strings.EqualFold(s.SHA256, e.SHA256) || s.Size != e.Size {
			out = append(out, e)
		}
	}
	return out
}

// blobStatusError is a failed read of the pod's blob slots (blob_status, or the console's
// `blobs`): nothing can be compared, so nothing is installed.
type blobStatusError struct{ err error }

func (e *blobStatusError) Error() string {
	return fmt.Sprintf("the pod did not list its blob slots (blob_status: %v), so no blobs were installed", e.err)
}

func (e *blobStatusError) Unwrap() error { return e.err }

// blobNamesAt names the blobs the manifest at src lists ("gw0, gw1, gw2, esp"), or a generic
// phrase when the manifest cannot be read.
func blobNamesAt(ctx context.Context, src blobSource) string {
	m, err := loadBlobsManifest(ctx, src)
	var names []string
	if err == nil {
		for _, e := range m.Blobs {
			if e.Size > 0 && e.File != "" {
				names = append(names, e.Name)
			}
		}
	}
	if len(names) == 0 {
		return "the gateware and ESP32-C3 images that go with this firmware"
	}
	return strings.Join(names, ", ")
}

// blobStatusWarning is what flash-self prints when the flashed pod did not list its blob
// slots: the firmware is in, the blobs are not, and how to retry.
func blobStatusWarning(ctx context.Context, src blobSource, err error) string {
	return fmt.Sprintf("flash-self: WARNING: the firmware is flashed, but reading the pod's blob slots failed (%v),\n"+
		"  so these blobs were NOT installed: %s.\n"+
		"  The pod may run old or missing gateware until they are. Install them with `benchpod install-blobs`.",
		errors.Unwrap(err), blobNamesAt(ctx, src))
}

// installBlobs brings the pod's blob slots in line with the manifest at src, over the USB
// console. only, when non-empty, limits it to those slot names.
func installBlobs(ctx context.Context, console blobTarget, src blobSource, force bool, only []string) error {
	_, err := installBlobsList(ctx, console, src, force, only)
	return err
}

// installBlobsList is installBlobs, returning the blobs it installed (for the firmware report).
func installBlobsList(ctx context.Context, console blobTarget, src blobSource, force bool, only []string) ([]blobEntry, error) {
	slots, err := console.Blobs(ctx)
	if err != nil {
		if errors.Is(err, serialconsole.ErrNoBlobSlots) {
			return nil, err
		}
		return nil, &blobStatusError{err: err}
	}
	m, err := loadBlobsManifest(ctx, src)
	if err != nil {
		return nil, err
	}
	todo := blobsToInstall(m, slots, force)
	if len(only) > 0 {
		keep := map[string]bool{}
		for _, n := range only {
			keep[strings.TrimSpace(n)] = true
		}
		var f []blobEntry
		for _, e := range todo {
			if keep[e.Name] {
				f = append(f, e)
			}
		}
		todo = f
	}
	if len(todo) == 0 {
		fmt.Fprintln(os.Stderr, "blobs: the pod already holds every blob this firmware goes with")
		return nil, nil
	}
	todo = espLast(todo)
	var done []blobEntry
	reloaded := false
	reload := func() {
		if r, ok := console.(gatewareReloader); ok && !reloaded && installedGateware(done) {
			r.ReloadStaleGateware(ctx, os.Stderr)
		}
		reloaded = true
	}
	for _, e := range todo {
		if e.Name == "esp" {
			reload() // before the C3 write: the pod's Wi-Fi is down during it
		}
		data, err := fetchBlob(ctx, src, e)
		if err != nil {
			return done, err
		}
		manifest := checkBlobSignature(ctx, src, e, data) // report only
		fmt.Fprintf(os.Stderr, "blobs: installing %s (%d KB) through %s...\n", e.Name, (e.Size+1023)/1024, console.Via())
		last := -1
		start := time.Now()
		rep, err := console.UploadSigned(ctx, e.Name, data, e.Version, manifest, func(done, total int) {
			if pct := done * 100 / total; pct/10 != last/10 {
				last = pct
				fmt.Fprintf(os.Stderr, "  %s %3d%%\n", e.Name, pct)
			}
		})
		if err != nil {
			return done, err
		}
		if line := podSigLine(rep); line != "" {
			fmt.Fprintf(os.Stderr, "  %s %s\n", e.Name, line)
		}
		fmt.Fprintf(os.Stderr, "blobs: %s installed (%.1f s)\n", e.Name, time.Since(start).Seconds())
		done = append(done, e)
	}
	reload()
	return done, nil
}

// espLast moves the ESP32-C3 image to the end: firmware that keeps no copy of it writes it into
// the C3 at once, about 2 minutes with the pod's Wi-Fi down, so nothing else should wait behind it.
func espLast(todo []blobEntry) []blobEntry {
	out := make([]blobEntry, 0, len(todo))
	var esp []blobEntry
	for _, e := range todo {
		if e.Name == "esp" {
			esp = append(esp, e)
		} else {
			out = append(out, e)
		}
	}
	return append(out, esp...)
}

func newInstallBlobsCmd(g *globalFlags) *cobra.Command {
	var dir, release, only string
	var force, noBridgeUpdate bool
	cmd := &cobra.Command{
		Use:   "install-blobs",
		Short: "Install the gateware and ESP32-C3 images the pod's firmware goes with",
		Long: "The pod keeps its iCE40 gateware images and its ESP32-C3 Wi-Fi image in the\n" +
			"W25Q flash next to the FPGA, not in its firmware. This sends the ones it lacks\n" +
			"to the pod --connection names: a network address, or a pod name found on the\n" +
			"LAN, gets them over the network. With usb (or no connection) they go over the\n" +
			"pod's USB network adapter (firmware 3.9.0+), else its network address.\n" +
			"USB networking off: `benchpod usb-network on --connection usb` (the console's\n" +
			"`usb-network on`). A pod that answers under another name than the one the\n" +
			"connection gives is refused. By default they come from the GitHub release that matches\n" +
			"the firmware the pod runs; --dir takes them from a local build (stm32h563/build/blobs).\n\n" +
			"Each blob's signature (blob-*.bin.sig) is checked and reported, and handed to\n" +
			"firmware that checks it too; for now a missing or failing one never stops an install.\n\n" +
			"With --force, after it writes the \"bridge\" slot (the Wi-Fi bridge's firmware), it tells\n" +
			"the pod to push that image to the bridge on its Ethernet port, even an older or EMC test\n" +
			"image ({\"cmd\":\"wifi_bridge\",\"action\":\"update\"}), and waits up to a minute for the\n" +
			"bridge to report it. With no bridge connected it says so and succeeds. Without --force\n" +
			"the pod updates the bridge by itself when the slot holds a newer release image.\n" +
			"--no-bridge-update skips the push.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			conn, err := g.blobConnection()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(blobsInstallTimeout))
			defer cancel()
			target, path, tctx, closeTarget, err := openBlobTarget(ctx, g, conn)
			if err != nil {
				return err
			}
			defer closeTarget()
			ctx = tctx
			if err := checkSamePod(ctx, target, conn.want); err != nil {
				return err
			}
			src := blobSource{base: dir}
			if strings.TrimSpace(dir) == "" {
				tag := release
				if strings.TrimSpace(tag) == "" {
					tag = target.Release(ctx)
				}
				src = releaseBlobSource(tag)
			}
			fmt.Fprintf(os.Stderr, "blobs: pod on %s (through %s), blobs from %s\n", path, target.Via(), src)
			var sel []string
			if strings.TrimSpace(only) != "" {
				sel = strings.Split(only, ",")
			}
			before, err := target.Blobs(ctx)
			if err != nil && !errors.Is(err, serialconsole.ErrNoBlobSlots) {
				return fmt.Errorf("install-blobs: %w", &blobStatusError{err: err})
			}
			installed, err := installBlobsList(ctx, target, src, force, sel)
			if len(installed) > 0 {
				g.reportFirmware(blobsEvent(ctx, target, before, installed))
			}
			if err == nil && force && !noBridgeUpdate && installedBlob(installed, "bridge") {
				if c, ok := target.(podCommander); ok {
					updateBridge(ctx, c, os.Stderr)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "take the blobs from this directory (holding blobs-manifest.json)")
	cmd.Flags().StringVar(&release, "release", "", "take the blobs from this firmware release tag (default: the one the pod runs)")
	cmd.Flags().StringVar(&only, "only", "", "comma-separated slots to install (gw0, gw1, gw2, esp, xip, xip_prog; bridge on pods with Wi-Fi bridge support)")
	cmd.Flags().BoolVar(&force, "force", false, "install even blobs the pod already holds")
	cmd.Flags().BoolVar(&noBridgeUpdate, "no-bridge-update", false, "with --force: do not tell the pod to push the installed bridge image to its Wi-Fi bridge")
	return cmd
}

func installedBlob(list []blobEntry, name string) bool {
	for _, e := range list {
		if e.Name == name {
			return true
		}
	}
	return false
}

// How long updateBridge follows the pod's push to the bridge, and how often it asks. Variables
// for the tests.
var (
	bridgeUpdateWait = 60 * time.Second
	bridgeUpdatePoll = 2 * time.Second
)

// bridgeUpdateDone says whether the pod's bridge update status (wifi_status bridge.update) has
// reached an end: "updated to X", or a failure that is not a retry.
func bridgeUpdateDone(status string) bool {
	return strings.HasPrefix(status, "updated to ") ||
		(strings.Contains(status, "failed") && !strings.Contains(status, "will retry")) ||
		strings.HasPrefix(status, "cannot update")
}

// updateBridge asks the pod to push its "bridge" slot to the Wi-Fi bridge on its Ethernet port and
// follows wifi_status until the bridge reports the outcome. It never fails the install: the image
// is in the pod's slot, and with no bridge (or firmware without the command) it only says so.
func updateBridge(ctx context.Context, c podCommander, w io.Writer) {
	_, before := readBridgeStatus(ctx, c)
	if _, err := c.Command(ctx, map[string]any{"cmd": "wifi_bridge", "action": "update"}); err != nil {
		if strings.Contains(err.Error(), "no Wi-Fi bridge") {
			fmt.Fprintln(w, "bridge: no Wi-Fi bridge on the pod's Ethernet port, so nothing was pushed. "+
				"The image is in the pod's \"bridge\" slot; connect the bridge and run `benchpod install-blobs --only bridge --force` again.")
		} else {
			fmt.Fprintf(w, "bridge: the image is installed, but the pod would not start the bridge update (%v)\n", err)
		}
		return
	}
	fmt.Fprintln(w, "bridge: the pod is pushing the new image to the Wi-Fi bridge...")
	start := time.Now()
	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(bridgeUpdatePoll):
		}
		fw, status := readBridgeStatus(ctx, c)
		if status != "" && status != last {
			last = status
			fmt.Fprintf(w, "  bridge: %s\n", status)
		}
		// A status left by an earlier update stays until the new one starts: wait for a change
		// (or a few polls, when this update changed nothing, e.g. a refusal repeated).
		if bridgeUpdateDone(status) && (status != before || time.Since(start) >= 3*bridgeUpdatePoll) {
			if strings.HasPrefix(status, "updated to ") {
				fmt.Fprintf(w, "bridge: updated, the bridge reports firmware %s\n", valueOrDash(fw))
			} else {
				fmt.Fprintf(w, "bridge: the update did not finish (%s); the image stays in the pod's slot\n", status)
			}
			return
		}
		if time.Since(start) >= bridgeUpdateWait {
			fmt.Fprintf(w, "bridge: not finished after %s (last status: %s); `benchpod show-network` shows it\n",
				bridgeUpdateWait, valueOrDash(last))
			return
		}
	}
}

// readBridgeStatus reads the bridge's firmware version and update status from wifi_status.
func readBridgeStatus(ctx context.Context, c podCommander) (fw, update string) {
	raw, err := c.Command(ctx, map[string]any{"cmd": "wifi_status"})
	if err != nil {
		return "", ""
	}
	var st struct {
		Bridge *struct {
			FW     string `json:"fw"`
			Update string `json:"update"`
		} `json:"bridge"`
	}
	if json.Unmarshal(raw, &st) != nil || st.Bridge == nil {
		return "", ""
	}
	return st.Bridge.FW, st.Bridge.Update
}

func (l lanBlobs) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	return l.lan.s.Command(ctx, req)
}
