package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
)

// blobTarget is where install-blobs reads the pod's W25Q slots and sends blobs: the pod's network
// API (blob_status and ota_* with a target), over its USB network adapter or its network address
// (firmware 3.9.0 and later; older firmware had a console upload this CLI no longer sends).
type blobTarget interface {
	Blobs(ctx context.Context) ([]serialconsole.BlobSlot, error)
	UploadSigned(ctx context.Context, target string, data []byte, version int, manifest []byte,
		progress func(done, total int)) (serialconsole.SigReport, error)
	// Release is the tag of the firmware the pod runs ("v3.9.0"), "" if unknown.
	Release(ctx context.Context) string
	// Via names the way in for messages: "/dev/cu.usbmodem1", "10.237.8.85:8080".
	Via() string
	// Identity is the pod's identity public key and short id ("" when unknown), for the
	// firmware report.
	Identity(ctx context.Context) (publicKey, shortID string)
	// Method is the firmware report's name for the way in: "usb_console", "usb_network", "lan".
	Method() string
}

// lanBlobChunk is the raw bytes per ota_data line (as the LAN OTA in hwe2e sends).
const lanBlobChunk = 768

// lanBlobs is the pod's JSON API on one connection.
type lanBlobs struct {
	lan  lanSession
	addr string
}

func (l lanBlobs) Via() string { return l.addr }

func (l lanBlobs) Method() string {
	if host, _, err := net.SplitHostPort(l.addr); err == nil && usbLinkNet.Contains(net.ParseIP(host)) {
		return "usb_network"
	}
	return "lan"
}

func (l lanBlobs) Identity(ctx context.Context) (string, string) {
	var rep struct {
		Public string `json:"public"`
	}
	if l.lan.cmd(ctx, map[string]any{"cmd": "identity_public"}, &rep) != nil || rep.Public == "" {
		return "", ""
	}
	return rep.Public, shortIDOf(rep.Public)
}

func (l lanBlobs) Release(ctx context.Context) string {
	var st struct {
		Version string `json:"version"`
	}
	if l.lan.cmd(ctx, map[string]any{"cmd": "status"}, &st) != nil || st.Version == "" {
		return ""
	}
	if strings.HasPrefix(st.Version, "v") {
		return st.Version
	}
	return "v" + st.Version
}

func (l lanBlobs) Blobs(ctx context.Context) ([]serialconsole.BlobSlot, error) {
	var rep struct {
		Blobs []struct {
			Name    string `json:"name"`
			State   string `json:"state"`
			Size    int    `json:"size"`
			Version int    `json:"version"`
			SHA256  string `json:"sha256"`
			Present bool   `json:"present"`
		} `json:"blobs"`
	}
	if err := l.lan.cmd(ctx, map[string]any{"cmd": "blob_status"}, &rep); err != nil {
		if isUnknownCommand(err) {
			return nil, serialconsole.ErrNoBlobSlots
		}
		return nil, err
	}
	slots := make([]serialconsole.BlobSlot, 0, len(rep.Blobs))
	for _, b := range rep.Blobs {
		sha := b.SHA256
		if !b.Present {
			sha = ""
		}
		slots = append(slots, serialconsole.BlobSlot{Name: b.Name, State: b.State, Size: b.Size, Version: b.Version, SHA256: sha})
	}
	return slots, nil
}

// otaSession is the pod's ota_* reply.
type otaSession struct {
	State  string `json:"state"`
	Error  string `json:"error"`
	Sig    string `json:"sig"`
	SigKey string `json:"sig_key"`
}

func (l lanBlobs) UploadSigned(ctx context.Context, target string, data []byte, version int, manifest []byte,
	progress func(done, total int)) (serialconsole.SigReport, error) {
	var rep serialconsole.SigReport
	if len(data) == 0 {
		return rep, fmt.Errorf("upload %s: empty image", target)
	}
	sum := sha256.Sum256(data)
	begin := map[string]any{"cmd": "ota_begin", "size": len(data), "sha256": hex.EncodeToString(sum[:]),
		"target": target, "version": version}
	if len(manifest) == serialconsole.ManifestLen {
		begin["sig"] = base64.RawURLEncoding.EncodeToString(manifest)
		rep.Supported, rep.Sent = true, true
	}
	var st otaSession
	if err := l.lan.cmd(ctx, begin, &st); err != nil {
		return rep, fmt.Errorf("upload %s: begin: %w", target, err)
	}
	rep.Result, rep.KeyID = st.Sig, st.SigKey
	abort := func() {
		if !l.lan.s.Broken() {
			_ = l.lan.cmd(ctx, map[string]any{"cmd": "ota_abort"}, nil)
		}
	}
	for off := 0; off < len(data); off += lanBlobChunk {
		end := min(off+lanBlobChunk, len(data))
		if err := l.lan.cmd(ctx, map[string]any{"cmd": "ota_data", "offset": off,
			"data": base64.RawURLEncoding.EncodeToString(data[off:end])}, nil); err != nil {
			abort()
			return rep, fmt.Errorf("upload %s at %d: %w", target, off, err)
		}
		if progress != nil {
			progress(end, len(data))
		}
	}
	if err := l.lan.cmd(ctx, map[string]any{"cmd": "ota_end"}, &st); err != nil {
		abort()
		return rep, fmt.Errorf("upload %s: end: %w", target, err)
	}
	if st.State != "verified" {
		abort()
		return rep, fmt.Errorf("upload %s: verify: %s", target, valueOr(st.Error, st.State))
	}
	if err := l.lan.cmd(ctx, map[string]any{"cmd": "ota_commit"}, &st); err != nil {
		return rep, fmt.Errorf("upload %s: commit: %w", target, err)
	}
	if st.State == "error" {
		return rep, fmt.Errorf("upload %s: commit: %s", target, valueOr(st.Error, "failed"))
	}
	if st.State == "applying" {
		return rep, l.waitApplied(ctx, target)
	}
	return rep, nil
}

// How long an esp image may take to go into the ESP32-C3 (about 2 minutes on the bench), and how
// often the pod is asked meanwhile. Variables for the tests.
var (
	espApplyWait = 6 * time.Minute
	espApplyPoll = 2 * time.Second
	espApplyNote = 30 * time.Second
)

// waitApplied follows an esp update that the pod writes into its ESP32-C3 (state "applying":
// firmware that keeps no copy of the C3's image) until it reports "installed" or "error". The pod's
// Wi-Fi is down meanwhile, so a connection over Wi-Fi drops: then it asks again on fresh
// connections until the pod answers.
func (l lanBlobs) waitApplied(ctx context.Context, target string) error {
	fmt.Fprintf(os.Stderr, "  %s: the pod is writing it into the ESP32-C3 (about 2 minutes; the pod's Wi-Fi is down meanwhile)\n", target)
	deadline := time.Now().Add(espApplyWait)
	sess := l.lan
	var fresh *lanSession
	defer func() {
		if fresh != nil {
			fresh.s.Close()
		}
	}()
	lastNote := time.Now()
	for {
		var st otaSession
		err := sess.cmd(ctx, map[string]any{"cmd": "ota_status"}, &st)
		switch {
		case err == nil && st.State == "installed":
			return nil
		case err == nil && st.State == "applying":
		case err == nil:
			return fmt.Errorf("upload %s: writing it into the ESP32-C3: %s", target, valueOr(st.Error, st.State))
		case sess.s == nil || sess.s.Broken():
			// The link went with the C3 (Wi-Fi), or never came back: try a new connection.
			if s, oerr := openLAN(ctx, l.addr); oerr == nil {
				if fresh != nil {
					fresh.s.Close()
				}
				fresh, sess = &s, s
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("upload %s: the pod did not finish writing it into the ESP32-C3 within %s "+
				"(run `benchpod install-blobs --only %s` again once the pod answers)", target, espApplyWait, target)
		}
		if time.Since(lastNote) >= espApplyNote {
			lastNote = time.Now()
			fmt.Fprintf(os.Stderr, "  %s: still writing it into the ESP32-C3...\n", target)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(espApplyPoll):
		}
	}
}

// blobTargetFor picks the pod's USB network over its console when it is up (the console's
// status names the address); the caller closes the returned closer.
func blobTargetFor(ctx context.Context, console *serialconsole.Console, path string) (blobTarget, func(), error) {
	u, err := console.USBNetwork(ctx)
	if errors.Is(err, serialconsole.ErrNoUSBNet) {
		return nil, func() {}, errors.New("this pod's firmware is older than 3.9.0 and takes blobs only over its USB console, " +
			"which this CLI no longer uses: update the pod with `benchpod flash-self` (USB DFU), which installs its blobs")
	}
	if err != nil {
		return nil, func() {}, fmt.Errorf("read the pod's status: %w", err)
	}
	try := func(addr string, check func(string) error) (blobTarget, func(), bool) {
		if check(addr) != nil {
			return nil, nil, false
		}
		lan, err := openLAN(ctx, addr)
		if err != nil {
			return nil, nil, false
		}
		return lanBlobs{lan: lan, addr: addr}, func() { lan.s.Close() }, true
	}
	if u.On && u.Link {
		if t, c, ok := try(u.PodIP+":8080", dialUSBPod); ok {
			return t, c, nil
		}
	}
	// Firmware 3.9.0+ has no console upload: without the USB network, the pod's Ethernet or Wi-Fi
	// address (its external LAN policy applies there).
	if u.LANIP != "" {
		if t, c, ok := try(u.LANIP+":8080", pingLANPod); ok {
			fmt.Fprintf(usbNote, "blobs: the pod's USB network is not usable; using its network address %s:8080\n", u.LANIP)
			return t, c, nil
		}
	}
	return nil, func() {}, errors.New("the pod takes blobs over the network, and neither its USB network adapter " +
		"nor its network address answers. " + usbNetworkOffHint)
}

// localBlobTarget is the pod's network API on this computer's end of its USB network link
// (firmware 3.9.0+), found without opening the console; nil when there is no such link.
func (g *globalFlags) localBlobTarget(ctx context.Context) (blobTarget, func()) {
	spec, err := g.resolveTarget()
	if err != nil || !spec.IsSerial() || spec.Device != "" {
		return nil, nil
	}
	addr := localUSBPod(spec.Pod)
	if addr == "" {
		return nil, nil
	}
	lan, err := openLAN(ctx, addr)
	if err != nil {
		return nil, nil
	}
	return lanBlobs{lan: lan, addr: addr}, func() { lan.s.Close() }
}

// blobConn is the pod install-blobs was told to use: a network address (Addr set; a pod name
// found on the LAN comes back as its address) or USB (Device or Pod set, or neither for
// auto-detect). want is the pod's short id when the connection named the pod ("" for a bare
// address or plain usb), which the pod reached must then match.
type blobConn struct {
	spec ConnSpec
	want string
}

// blobConnection resolves install-blobs' connection. A network address or a pod name found on the
// LAN goes to that pod over the network, never to whatever pod is on USB. An explicit
// embeddedci: target is refused; a saved one (like no connection at all) means the pod on USB.
func (g *globalFlags) blobConnection() (blobConn, error) {
	if _, err := g.usbDevice("install-blobs"); err != nil {
		return blobConn{}, err
	}
	raw, err := g.rawConnection()
	if err != nil {
		return blobConn{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return blobConn{spec: ConnSpec{Kind: connSerial}}, nil
	}
	spec, err := parseTarget(raw)
	if err != nil {
		return blobConn{}, err
	}
	if spec.IsCloud() {
		return blobConn{spec: ConnSpec{Kind: connSerial}}, nil
	}
	want := spec.Pod
	if want == "" {
		want = podShortName(strings.TrimSpace(raw))
	}
	return blobConn{spec: spec, want: want}, nil
}

// openBlobTarget opens the way to the pod conn names. Over the network that is the address
// itself. Over USB it is this computer's end of the pod's USB network link when there is one, else
// the pod's console, which names its USB network or network address. The returned context
// replaces ctx (the console path has its own timeout and signal handling); the caller calls the
// closer when done.
func openBlobTarget(ctx context.Context, g *globalFlags, conn blobConn) (blobTarget, string, context.Context, func(), error) {
	if conn.spec.IsNetwork() {
		lan, err := openLAN(ctx, conn.spec.Addr)
		if err != nil {
			return nil, "", ctx, nil, fmt.Errorf("install-blobs: connect to the pod at %s: %w", conn.spec.Addr, err)
		}
		return lanBlobs{lan: lan, addr: conn.spec.Addr}, "the network", ctx, func() { lan.s.Close() }, nil
	}
	if target, closeTarget := g.localBlobTarget(ctx); target != nil {
		return target, "the USB network", ctx, closeTarget, nil
	}
	return openBlobConsole(g, conn.spec.Device)
}

// openBlobConsole opens the pod's USB console and the blob target it names. A variable so tests
// can tell whether install-blobs went to USB at all.
var openBlobConsole = func(g *globalFlags, device string) (blobTarget, string, context.Context, func(), error) {
	console, path, ctx, cancel, err := g.openSerialConsole(device, g.effectiveTimeout(blobsInstallTimeout))
	if err != nil {
		return nil, "", nil, nil, err
	}
	target, closeTarget, err := blobTargetFor(ctx, console, path)
	if err != nil {
		console.Close()
		cancel()
		return nil, "", nil, nil, err
	}
	return target, path, ctx, func() {
		closeTarget()
		console.Close()
		cancel()
	}, nil
}

// checkSamePod refuses a pod other than the one the connection named (want, a short id): blobs
// built for one pod's firmware must not go to another one that happens to be on USB. A pod that
// does not tell its identity (older firmware) is let through.
func checkSamePod(ctx context.Context, target blobTarget, want string) error {
	if want == "" {
		return nil
	}
	_, got := target.Identity(ctx)
	if got == "" || got == want {
		return nil
	}
	return fmt.Errorf("install-blobs: the connection names benchpod-%s, but the pod reached through %s is benchpod-%s, "+
		"so nothing was installed. Pass that pod's network address (`benchpod discover` lists them) "+
		"or --connection usb:benchpod-%s when it is on this computer's USB", want, target.Via(), got, want)
}
