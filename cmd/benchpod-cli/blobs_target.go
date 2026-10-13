package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// blobTarget is where install-blobs reads the pod's W25Q slots and sends blobs: the pod's network
// API (over its USB network adapter, firmware 3.9.0+: blob_status and ota_* with a target) or its
// USB console (blobs and upload-*, older firmware).
type blobTarget interface {
	Blobs(ctx context.Context) ([]serialconsole.BlobSlot, error)
	UploadSigned(ctx context.Context, target string, data []byte, version int, manifest []byte,
		progress func(done, total int)) (serialconsole.SigReport, error)
	// Release is the tag of the firmware the pod runs ("v3.9.0"), "" if unknown.
	Release(ctx context.Context) string
	// Via names the way in for messages: "/dev/cu.usbmodem1", "10.237.8.85:8080".
	Via() string
}

// consoleBlobs is the USB console.
type consoleBlobs struct {
	*serialconsole.Console
	path string
}

func (c consoleBlobs) Release(ctx context.Context) string { return podFirmwareRelease(ctx, c.Console) }
func (c consoleBlobs) Via() string                        { return c.path }

// lanBlobChunk is the raw bytes per ota_data line (as the LAN OTA in hwe2e sends).
const lanBlobChunk = 768

// lanBlobs is the pod's JSON API on one connection.
type lanBlobs struct {
	lan  lanSession
	addr string
}

func (l lanBlobs) Via() string { return l.addr }

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
		if pe, ok := tcpclient.AsPodError(err); ok && strings.Contains(pe.Reason, "unknown cmd") {
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
	return rep, nil
}

// blobTargetFor picks the pod's USB network over its console when it is up (the console's
// status names the address); the caller closes the returned closer.
func blobTargetFor(ctx context.Context, console *serialconsole.Console, path string) (blobTarget, func(), error) {
	if u, err := console.USBNetwork(ctx); err == nil && u.On && u.Link {
		addr := u.PodIP + ":8080"
		if dialUSBPod(addr) == nil {
			if lan, err := openLAN(ctx, addr); err == nil {
				return lanBlobs{lan: lan, addr: addr}, func() { lan.s.Close() }, nil
			}
		}
	}
	return consoleBlobs{Console: console, path: path}, func() {}, nil
}
