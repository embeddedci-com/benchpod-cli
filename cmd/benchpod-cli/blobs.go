package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/spf13/cobra"
)

// The blobs a firmware release goes with: the two iCE40 gateware images and the ESP32-C3
// esp-hosted image. The pod keeps them in slots of its W25Q flash rather than in the firmware
// image, so whoever installs firmware installs these next to it. A release publishes them as
// blob-gw0.bin, blob-gw1.bin, blob-esp.bin plus blobs-manifest.json; a local firmware build
// leaves the same files in stm32h563/build/blobs/.

const blobsManifestName = "blobs-manifest.json"

// blobsInstallTimeout bounds a whole install: ~1.3 MB over the USB console plus the slot writes.
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

// installBlobs brings the pod's blob slots in line with the manifest at src, over the USB
// console. only, when non-empty, limits it to those slot names.
func installBlobs(ctx context.Context, console *serialconsole.Console, src blobSource, force bool, only []string) error {
	slots, err := console.Blobs(ctx)
	if err != nil {
		return err
	}
	m, err := loadBlobsManifest(ctx, src)
	if err != nil {
		return err
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
		return nil
	}
	for _, e := range todo {
		data, err := fetchBlob(ctx, src, e)
		if err != nil {
			return err
		}
		manifest := checkBlobSignature(ctx, src, e, data) // report only
		fmt.Fprintf(os.Stderr, "blobs: installing %s (%d KB) over USB...\n", e.Name, (e.Size+1023)/1024)
		last := -1
		start := time.Now()
		rep, err := console.UploadSigned(ctx, e.Name, data, e.Version, manifest, func(done, total int) {
			if pct := done * 100 / total; pct/10 != last/10 {
				last = pct
				fmt.Fprintf(os.Stderr, "  %s %3d%%\n", e.Name, pct)
			}
		})
		if err != nil {
			return err
		}
		if line := podSigLine(rep); line != "" {
			fmt.Fprintf(os.Stderr, "  %s %s\n", e.Name, line)
		}
		fmt.Fprintf(os.Stderr, "blobs: %s installed (%.1f s)\n", e.Name, time.Since(start).Seconds())
	}
	return nil
}

// podFirmwareRelease is the release tag of the firmware a pod runs ("v3.4.0"), "" if unknown.
func podFirmwareRelease(ctx context.Context, console *serialconsole.Console) string {
	raw, err := console.Status(ctx)
	if err != nil {
		return ""
	}
	v := serialconsole.FirmwareVersion(raw)
	if v != "" && !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

func newInstallBlobsCmd(g *globalFlags) *cobra.Command {
	var dir, release, only string
	var force bool
	cmd := &cobra.Command{
		Use:   "install-blobs",
		Short: "Install the gateware and ESP32-C3 images the pod's firmware goes with (over USB)",
		Long: "The pod keeps its iCE40 gateware images and its ESP32-C3 Wi-Fi image in the\n" +
			"W25Q flash next to the FPGA, not in its firmware. This sends the ones it lacks\n" +
			"over the USB console. By default they come from the GitHub release that matches\n" +
			"the firmware the pod runs; --dir takes them from a local build (stm32h563/build/blobs).\n\n" +
			"Each blob's signature (blob-*.bin.sig) is checked and reported, and handed to\n" +
			"firmware that checks it too; for now a missing or failing one never stops an install.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			device, err := g.usbDevice("install-blobs")
			if err != nil {
				return err
			}
			console, path, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(blobsInstallTimeout))
			if err != nil {
				return err
			}
			defer cancel()
			defer console.Close()
			src := blobSource{base: dir}
			if strings.TrimSpace(dir) == "" {
				tag := release
				if strings.TrimSpace(tag) == "" {
					tag = podFirmwareRelease(ctx, console)
				}
				src = releaseBlobSource(tag)
			}
			fmt.Fprintf(os.Stderr, "blobs: pod on %s, blobs from %s\n", path, src)
			var sel []string
			if strings.TrimSpace(only) != "" {
				sel = strings.Split(only, ",")
			}
			return installBlobs(ctx, console, src, force, sel)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "take the blobs from this directory (holding blobs-manifest.json)")
	cmd.Flags().StringVar(&release, "release", "", "take the blobs from this firmware release tag (default: the one the pod runs)")
	cmd.Flags().StringVar(&only, "only", "", "comma-separated slots to install (gw0, gw1, esp)")
	cmd.Flags().BoolVar(&force, "force", false, "install even blobs the pod already holds")
	return cmd
}
