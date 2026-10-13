package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// flash-self --network updates the pod's own firmware over its network API instead of USB DFU:
// the image is staged in the pod's PSRAM (ota_begin / ota_data), its SHA-256 checked (ota_end),
// then written to flash by the pod itself, which restarts into it (ota_commit). That is the path
// the web app takes through embeddedci.com; here it runs over the pod's USB network adapter
// (`usb`, firmware 3.9.0+) or its Ethernet or Wi-Fi address. No dfu-util, no bootloader.
//
// DFU stays the default: it needs nothing from the running firmware, so it also recovers a pod
// whose firmware is broken. --network needs working firmware, and an interrupted flash write
// (power lost during the few seconds of ota_commit) leaves a pod only DFU can bring back.

// Seams for tests: how long the pod gets to come back after the commit, and how often it is tried.
var (
	networkFlashReturnWait  = 150 * time.Second
	networkFlashReturnRetry = 2 * time.Second
	// The pod acknowledges the commit before it writes its flash and restarts: give it that
	// long before asking, so the old firmware does not answer for the new one.
	networkFlashSettle = 5 * time.Second
)

// dfuOnlyFlags are the flash-self flags that only mean something for DFU.
var dfuOnlyFlags = []string{"enter-dfu", "no-leave", "dfu-util", "address", "wait"}

// podStatusInfo is what flash-self --network reads from the pod's JSON status.
type podStatusInfo struct {
	Version  string          `json:"version"`
	FlashKB  int             `json:"flash_kb"`
	Gateware json.RawMessage `json:"gateware"`
}

func (s podStatusInfo) gateware() int {
	var n int
	if json.Unmarshal(s.Gateware, &n) == nil {
		return n
	}
	var o struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(s.Gateware, &o) == nil {
		return o.Version
	}
	return 0
}

// lanVersions reads the pod's identity and versions over its network API.
func lanVersions(ctx context.Context, lan lanSession) (podVersions, podStatusInfo) {
	var v podVersions
	var st podStatusInfo
	if lan.cmd(ctx, map[string]any{"cmd": "status"}, &st) == nil {
		v.Version = strings.TrimPrefix(st.Version, "v")
		v.Gateware = st.gateware()
	}
	pub, short := lanBlobs{lan: lan}.Identity(ctx)
	v.PublicKey, v.ShortID = pub, short
	return v, st
}

// networkFlashAddr is the pod's network API for flash-self --network: its USB network adapter
// for a USB connection, the address for a network one. The cloud is refused: the web app
// updates a pod through embeddedci.com.
func networkFlashAddr(g *globalFlags) (string, error) {
	spec, err := g.resolveTarget()
	if err != nil {
		return "", err
	}
	if spec.IsCloud() {
		return "", fmt.Errorf("flash-self --network does not go through embeddedci.com (--connection %s%s): "+
			"update a cloud pod from the web app (BenchPods, Update next to the pod, or the pod's Settings tab), "+
			"or connect it over USB-C and run `benchpod flash-self --enter-dfu`", cloudConnPrefix, spec.Name)
	}
	spec, err = g.networkTarget("flash-self --network")
	if err != nil {
		return "", fmt.Errorf("%w. Without it, update over USB DFU: `benchpod flash-self --enter-dfu`", err)
	}
	return spec.Addr, nil
}

// uploadFirmwareLAN stages image on the pod and commits it. The commit's reply is best effort:
// the pod writes its flash and restarts right after acknowledging, so a connection that drops
// there is the expected end, not a failure.
func uploadFirmwareLAN(ctx context.Context, lan lanSession, image, manifest []byte, progress func(done, total int)) error {
	sum := sha256.Sum256(image)
	begin := map[string]any{"cmd": "ota_begin", "size": len(image), "sha256": hex.EncodeToString(sum[:])}
	if len(manifest) == serialconsole.ManifestLen {
		begin["sig"] = base64.RawURLEncoding.EncodeToString(manifest)
	}
	var st otaSession
	if err := lan.cmd(ctx, begin, &st); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if st.Sig != "" {
		fmt.Fprintf(os.Stderr, "flash-self: pod signature check: %s\n", valueOr(strings.TrimSpace(st.Sig+" "+st.SigKey), st.Sig))
	}
	abort := func() {
		if !lan.s.Broken() {
			_ = lan.cmd(ctx, map[string]any{"cmd": "ota_abort"}, nil)
		}
	}
	for off := 0; off < len(image); off += lanBlobChunk {
		end := min(off+lanBlobChunk, len(image))
		if err := lan.cmd(ctx, map[string]any{"cmd": "ota_data", "offset": off,
			"data": base64.RawURLEncoding.EncodeToString(image[off:end])}, nil); err != nil {
			abort()
			return fmt.Errorf("send at byte %d: %w", off, err)
		}
		if progress != nil {
			progress(end, len(image))
		}
	}
	if err := lan.cmd(ctx, map[string]any{"cmd": "ota_end"}, &st); err != nil {
		abort()
		return fmt.Errorf("verify: %w", err)
	}
	if st.State != "verified" {
		abort()
		return fmt.Errorf("verify: %s", valueOr(st.Error, st.State))
	}
	fmt.Fprintln(os.Stderr, "flash-self: image staged and SHA-256 verified on the pod; writing it to flash (do not unplug the pod)")
	if err := lan.cmd(ctx, map[string]any{"cmd": "ota_commit"}, nil); err != nil && !lan.s.Broken() {
		// A refusal (no verified image, busy, locked, signature policy) comes back as the pod's
		// error reply. A connection that drops is the pod restarting: the expected end.
		if pe, ok := tcpclient.AsPodError(err); ok && !strings.Contains(strings.ToLower(pe.Reason), "timeout") &&
			!errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("commit: %s", pe.Reason)
		}
	}
	return nil
}

// waitForPodOnNetwork waits for the restarted pod's network API and returns it with its versions.
// The address is looked up again each try: over USB the adapter comes back after the restart.
func waitForPodOnNetwork(g *globalFlags, ctx context.Context) (lanSession, string, podVersions, error) {
	select {
	case <-ctx.Done():
		return lanSession{}, "", podVersions{}, ctx.Err()
	case <-time.After(networkFlashSettle):
	}
	deadline := time.Now().Add(networkFlashReturnWait)
	var lastErr error
	for {
		g.usbAddr, g.usbErr = "", nil
		addr, err := networkFlashAddr(g)
		if err == nil {
			lan, oerr := openLAN(ctx, addr)
			if oerr == nil {
				v, _ := lanVersions(ctx, lan)
				if v.Version != "" {
					return lan, addr, v, nil
				}
				lan.s.Close()
				err = errors.New("the pod did not answer status yet")
			} else {
				err = oerr
			}
		}
		lastErr = err
		if time.Now().After(deadline) {
			return lanSession{}, "", podVersions{}, lastErr
		}
		select {
		case <-ctx.Done():
			return lanSession{}, "", podVersions{}, ctx.Err()
		case <-time.After(networkFlashReturnRetry):
		}
	}
}

// runFlashSelfNetwork is flash-self --network.
func runFlashSelfNetwork(g *globalFlags, f *flashSelfFlags, changed func(string) bool, args []string) error {
	for _, name := range dfuOnlyFlags {
		if changed(name) {
			return fmt.Errorf("--%s is for USB DFU and does not apply with --network", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.effectiveTimeout(flashSelfTimeout))
	defer cancel()

	addr, err := networkFlashAddr(g)
	if err != nil {
		return err
	}
	lan, err := openLAN(ctx, addr)
	if err != nil {
		return fmt.Errorf("flash-self --network: connect to the pod at %s: %w", addr, err)
	}
	before, st := lanVersions(ctx, lan)
	lan.s.Close()
	method := lanBlobs{addr: addr}.Method()
	fmt.Fprintf(os.Stderr, "flash-self: pod at %s (%s), running firmware %s\n", addr,
		map[string]string{"usb_network": "USB network", "lan": "network"}[method], valueOr(before.Version, "unknown"))

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
	image, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("firmware file: %w", err)
	}
	if need := imageNeedsKB(image); st.FlashKB > 0 && need > st.FlashKB {
		return fmt.Errorf("this firmware needs %d KB of flash and the pod has %d KB: it was built for the 2 MB H563; use a newer release", need, st.FlashKB)
	}
	sig := checkFirmwareSignature(ctx, location, file)
	var manifest []byte
	if sig.Forwardable() {
		manifest = sig.Manifest.Raw
	}

	lan, err = openLAN(ctx, addr)
	if err != nil {
		return fmt.Errorf("flash-self --network: connect to the pod at %s: %w", addr, err)
	}
	fmt.Fprintf(os.Stderr, "flash-self: sending %d KB to the pod\n", (len(image)+1023)/1024)
	lastPct := -1
	err = uploadFirmwareLAN(ctx, lan, image, manifest, func(done, total int) {
		if pct := done * 100 / total; pct/10 != lastPct/10 {
			lastPct = pct
			fmt.Fprintf(os.Stderr, "flash-self: %d%%\n", pct)
		}
	})
	lan.s.Close()
	if err != nil {
		return fmt.Errorf("flash-self --network: %w (nothing was written: the pod keeps its firmware)", err)
	}

	fmt.Fprintln(os.Stderr, "flash-self: the pod is restarting into the new firmware; waiting for it to come back...")
	lan, addr, after, err := waitForPodOnNetwork(g, ctx)
	if err != nil {
		g.reportFirmware(networkFirmwareEvent(before, podVersions{}, f.firmwareVer, nil, method))
		return fmt.Errorf("flash-self: the firmware was written, but the pod did not come back on the network within %s "+
			"(%v). Check `benchpod discover`; if the pod does not show up, unplug USB-C and plug it back in", networkFlashReturnWait, err)
	}
	defer lan.s.Close()
	fmt.Fprintf(os.Stderr, "flash-self: success, the pod runs firmware %s\n", valueOr(after.Version, "unknown"))

	var installed []blobEntry
	if !f.skipBlobs {
		src := blobSourceForFirmware(arg, f.firmwareURL, f.firmwareVer)
		if strings.TrimSpace(f.blobsDir) != "" {
			src = blobSource{base: f.blobsDir}
		}
		target := lanBlobs{lan: lan, addr: addr}
		if _, berr := target.Blobs(ctx); errors.Is(berr, serialconsole.ErrNoBlobSlots) {
			fmt.Fprintln(os.Stderr, "flash-self: this firmware keeps its blobs built in; nothing to install")
		} else if berr != nil {
			err = fmt.Errorf("flash-self: the firmware is updated, but reading the pod's blob slots failed, so its blobs are not installed (retry with `benchpod install-blobs`): %w", berr)
		} else if installed, berr = installBlobsList(ctx, target, src, false, nil); berr != nil {
			err = fmt.Errorf("flash-self: the firmware is updated, but installing its blobs failed (retry with `benchpod install-blobs`): %w", berr)
		}
	}
	g.reportFirmware(networkFirmwareEvent(before, after, f.firmwareVer, installed, method))
	return err
}

// networkFirmwareEvent is firmwareEvent for an update over the network API.
func networkFirmwareEvent(before, after podVersions, tag string, installed []blobEntry, method string) fwEvent {
	ev := firmwareEvent(before, after, tag, installed)
	ev.Method = method
	return ev
}
