package serialconsole

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"strconv"
	"strings"
	"time"
)

// Uploads over the USB console: the firmware itself or one of the blobs the pod keeps in its
// W25Q slots (the ESP32-C3 image and the two iCE40 gateware images). The firmware stages the
// data like an OTA, checks its SHA-256, then writes it (stm32h563/src/upload_rx.h):
//
//	upload-begin <target> <size> <sha256> [version]   -> upload-begin ok | error <why>
//	upload-data <offset> <len> <crc32>\n<len raw bytes> -> upload-data ok <off> | retry <why> | busy
//	upload-end                                          -> upload-end ok | error <why>
//	upload-commit                                       -> upload-commit ok | resetting | error <why>
//
// Firmware that checks signatures also takes the image's 128-byte signed manifest (the release's
// .sig file) before upload-begin. As base64url it is 171 characters, too long for one console
// line next to anything else, so it goes in two halves; a bare upload-sig reports the check of
// the last upload-begin:
//
//	upload-sig <0|1> <base64url half>                   -> upload-sig ok | error <why>
//	upload-sig                                          -> upload-sig result <word> <key_id|->
//
// Older firmware answers upload-sig with "unknown command", so a signed upload first asks once
// per console session and sends nothing more to a pod that does not know it.
//
// Chunks are at most 512 bytes, so one always fits the pod's 1 KB receive ring; each waits for
// its reply before the next goes out.

// UploadChunk is the largest upload-data chunk the firmware takes.
const UploadChunk = 512

// Vars only so tests can shorten them.
var (
	uploadReplyTimeout  = 5 * time.Second   // begin, one chunk
	uploadEndTimeout    = 30 * time.Second  // the pod hashes the whole staged image
	uploadCommitTimeout = 180 * time.Second // erase + program + hash back a 4 MB slot, or a gateware swap
)

const uploadChunkAttempts = 5

// BlobSlot is one line of the firmware's `blobs` command.
type BlobSlot struct {
	Name    string // "gw0", "gw1", "esp"
	State   string // "ok", "outdated", "missing", "unknown" (against the blobs the firmware was built with)
	Size    int
	Version int
	SHA256  string // "" when the slot is empty
}

// NeedsInstall reports whether an installer should send this slot's blob.
func (b BlobSlot) NeedsInstall() bool { return b.State == "missing" || b.State == "outdated" }

// ErrNoBlobSlots is what Blobs returns for firmware without the blobs command: it keeps the
// gateware and ESP32-C3 images built in, so there is nothing to install.
var ErrNoBlobSlots = errors.New("this firmware keeps its blobs built in (no blob slots)")

// Blobs lists the pod's W25Q blob slots.
func (c *Console) Blobs(ctx context.Context) ([]BlobSlot, error) {
	out, err := c.sendCommand(ctx, "blobs")
	if err != nil {
		return nil, err
	}
	if strings.Contains(out, "unknown command") {
		return nil, ErrNoBlobSlots
	}
	slots := parseBlobs(out)
	if len(slots) == 0 {
		return nil, fmt.Errorf("the pod did not list its blob slots (firmware too old?): %q", tail([]byte(out), 200))
	}
	return slots, nil
}

// parseBlobs reads "blob <name> <state> <size> <version> <sha256|->" lines.
func parseBlobs(out string) []BlobSlot {
	var slots []BlobSlot
	for _, ln := range strings.Split(strings.ReplaceAll(out, "\r", "\n"), "\n") {
		f := strings.Fields(ln)
		if len(f) != 6 || f[0] != "blob" {
			continue
		}
		size, err1 := strconv.Atoi(f[3])
		ver, err2 := strconv.Atoi(f[4])
		if err1 != nil || err2 != nil {
			continue
		}
		sha := f[5]
		if sha == "-" {
			sha = ""
		}
		slots = append(slots, BlobSlot{Name: f[1], State: f[2], Size: size, Version: ver, SHA256: sha})
	}
	return slots
}

// Upload sends data to target ("firmware", "gw0", "gw1", "esp") and commits it. version is
// stored with a gateware blob (0 = unknown). progress, when non-nil, is called after each chunk.
// A firmware commit resets the pod: Upload then returns nil once the pod says "resetting".
func (c *Console) Upload(ctx context.Context, target string, data []byte, version int, progress func(done, total int)) error {
	_, err := c.UploadSigned(ctx, target, data, version, nil, progress)
	return err
}

// ManifestLen is the size of a signed manifest (a release's .sig file).
const ManifestLen = 128

// SigReport is what a signed upload learned about the pod's own signature check.
type SigReport struct {
	Supported bool   // the firmware takes upload-sig
	Sent      bool   // the manifest reached the pod before upload-begin
	Result    string // the pod's check of this upload ("ok", "none", "signature", ...); "" if not known
	KeyID     string // the key_id the pod matched, "" if none
}

// UploadSigned is Upload with the image's signed manifest (nil = none). The manifest is only
// sent to firmware that understands it, and only reported on: the upload goes ahead exactly as
// Upload does whether or not the pod takes it.
func (c *Console) UploadSigned(ctx context.Context, target string, data []byte, version int, manifest []byte, progress func(done, total int)) (SigReport, error) {
	var rep SigReport
	if len(data) == 0 {
		return rep, fmt.Errorf("upload %s: empty image", target)
	}
	if rt, ok := c.rw.(readTimeoutSetter); ok {
		_ = rt.SetReadTimeout(perReadTimeout)
	}
	if len(manifest) == ManifestLen {
		rep.Supported = c.uploadSigSupported(ctx)
		if rep.Supported {
			rep.Sent = c.sendUploadSig(ctx, manifest)
		}
	}
	sum := sha256.Sum256(data)
	begin := fmt.Sprintf("upload-begin %s %d %s %d", target, len(data), hex.EncodeToString(sum[:]), version)
	c.logln("> %s", begin)
	if err := c.writeLine(begin); err != nil {
		return rep, fmt.Errorf("upload %s: %w", target, err)
	}
	if reply, err := c.awaitReply(ctx, "upload-begin", uploadReplyTimeout); err != nil {
		return rep, fmt.Errorf("upload %s: %w", target, err)
	} else if reply != "ok" {
		return rep, fmt.Errorf("upload %s: begin: %s", target, reply)
	}
	if rep.Supported {
		if res, kid, err := c.queryUploadSig(ctx); err == nil {
			rep.Result, rep.KeyID = res, kid
		}
	}

	for off := 0; off < len(data); off += UploadChunk {
		end := off + UploadChunk
		if end > len(data) {
			end = len(data)
		}
		if err := c.uploadChunk(ctx, off, data[off:end]); err != nil {
			_ = c.writeLine("upload-abort")
			return rep, fmt.Errorf("upload %s: %w", target, err)
		}
		if progress != nil {
			progress(end, len(data))
		}
	}

	if err := c.writeLine("upload-end"); err != nil {
		return rep, fmt.Errorf("upload %s: %w", target, err)
	}
	if reply, err := c.awaitReply(ctx, "upload-end", uploadEndTimeout); err != nil {
		return rep, fmt.Errorf("upload %s: %w", target, err)
	} else if reply != "ok" {
		return rep, fmt.Errorf("upload %s: verify: %s", target, reply)
	}

	if err := c.writeLine("upload-commit"); err != nil {
		return rep, fmt.Errorf("upload %s: %w", target, err)
	}
	reply, err := c.awaitReply(ctx, "upload-commit", uploadCommitTimeout)
	if err != nil {
		return rep, fmt.Errorf("upload %s: %w", target, err)
	}
	if reply != "ok" && reply != "resetting" {
		return rep, fmt.Errorf("upload %s: commit: %s", target, reply)
	}
	return rep, nil
}

// errUploadSigUnknown is older firmware's answer to upload-sig.
var errUploadSigUnknown = errors.New("the firmware does not know upload-sig")

// uploadSigSupported asks the pod once per console session whether it takes upload-sig.
func (c *Console) uploadSigSupported(ctx context.Context) bool {
	if !c.sigProbed {
		c.sigProbed = true
		_, _, err := c.queryUploadSig(ctx)
		c.sigSupported = err == nil
		if err != nil {
			c.logln("< upload-sig: %v; uploading without the signed manifest", err)
		}
	}
	return c.sigSupported
}

// queryUploadSig sends a bare upload-sig: new firmware answers "upload-sig result <word>
// <key_id|->", older firmware "unknown command 'upload-sig' (try 'help')".
func (c *Console) queryUploadSig(ctx context.Context) (result, keyID string, err error) {
	c.logln("> upload-sig")
	if err := c.writeLine("upload-sig"); err != nil {
		return "", "", err
	}
	rctx, cancel := context.WithTimeout(ctx, uploadReplyTimeout)
	defer cancel()
	for {
		ln, err := c.readLine(rctx)
		if err != nil {
			return "", "", fmt.Errorf("waiting for upload-sig reply: %w", err)
		}
		if strings.Contains(ln, "unknown command") {
			return "", "", errUploadSigUnknown
		}
		i := strings.Index(ln, "upload-sig result ")
		if i < 0 {
			continue // the echo, or a log line
		}
		f := strings.Fields(ln[i+len("upload-sig result "):])
		if len(f) == 0 {
			continue
		}
		kid := ""
		if len(f) > 1 && f[1] != "-" {
			kid = f[1]
		}
		c.logln("< upload-sig result %s", strings.Join(f, " "))
		return f[0], kid, nil
	}
}

// sendUploadSig hands the pod the manifest for the next upload-begin, in two halves (each one
// console line, answered before the next goes out). On any trouble it clears what the pod
// collected so far and reports false: the upload then goes ahead without a manifest.
func (c *Console) sendUploadSig(ctx context.Context, manifest []byte) bool {
	b64 := base64.RawURLEncoding.EncodeToString(manifest) // 171 characters
	half := (len(b64) + 1) / 2
	for part, s := range []string{b64[:half], b64[half:]} {
		line := fmt.Sprintf("upload-sig %d %s", part, s)
		c.logln("> %s", line)
		if err := c.writeLine(line); err != nil {
			return false
		}
		reply, err := c.awaitReply(ctx, "upload-sig", uploadReplyTimeout)
		if err != nil || reply != "ok" {
			c.logln("< upload-sig part %d: %q %v; uploading without the signed manifest", part, reply, err)
			if err := c.writeLine("upload-sig clear"); err == nil {
				_, _ = c.awaitReply(ctx, "upload-sig", uploadReplyTimeout)
			}
			return false
		}
	}
	return true
}

// uploadChunk sends one chunk and waits for "ok", resending on retry/busy. A chunk whose reply
// never comes (the pod's console can stall for seconds while it is still booting) is not resent
// blindly: the pod is first asked how far it got, so a chunk that did land is not sent twice into
// a receive ring that may still hold it.
func (c *Console) uploadChunk(ctx context.Context, off int, chunk []byte) error {
	head := fmt.Sprintf("upload-data %d %d %08x\n", off, len(chunk), crc32.ChecksumIEEE(chunk))
	frame := append([]byte(head), chunk...)
	var last string
	for attempt := 1; attempt <= uploadChunkAttempts; attempt++ {
		if _, err := c.rw.Write(frame); err != nil {
			return err
		}
		reply, err := c.awaitReply(ctx, "upload-data", uploadReplyTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("chunk at %d: %w", off, err)
			}
			got, serr := c.uploadReceived(ctx)
			if serr == nil && got >= off+len(chunk) {
				c.logln("< chunk at %d: reply lost, but the pod has it", off)
				return nil
			}
			last = "no reply"
			c.logln("< chunk at %d: no reply (attempt %d)", off, attempt)
			continue
		}
		if strings.HasPrefix(reply, "ok") {
			return nil
		}
		last = reply
		if strings.HasPrefix(reply, "error") {
			break
		}
		c.logln("< chunk at %d: %s (attempt %d)", off, reply, attempt)
		if strings.HasPrefix(reply, "busy") {
			time.Sleep(20 * time.Millisecond)
		}
	}
	return fmt.Errorf("chunk at %d: %s", off, last)
}

// uploadStatusWait is how long to wait for upload-status after a chunk reply went missing: long
// enough for a console that is busy right after boot to catch up.
const uploadStatusWait = 15 * time.Second

// uploadReceived asks the pod how many bytes of the current upload it has staged.
func (c *Console) uploadReceived(ctx context.Context) (int, error) {
	if err := c.writeLine("upload-status"); err != nil {
		return 0, err
	}
	rctx, cancel := context.WithTimeout(ctx, uploadStatusWait)
	defer cancel()
	for {
		ln, err := c.readLine(rctx)
		if err != nil {
			return 0, err
		}
		i := strings.Index(ln, "upload-status ")
		if i < 0 {
			continue
		}
		// "upload-status <state> <target> <received>/<size> <error>"
		f := strings.Fields(ln[i+len("upload-status "):])
		if len(f) < 3 || !strings.Contains(f[2], "/") {
			continue // the echo of the command
		}
		n, err := strconv.Atoi(f[2][:strings.Index(f[2], "/")])
		if err != nil {
			continue
		}
		return n, nil
	}
}

// uploadReplyWords are the first words of a reply (as opposed to the echo of the command,
// which also starts with the command name).
var uploadReplyWords = map[string]bool{"ok": true, "error": true, "retry": true, "busy": true, "resetting": true}

// awaitReply reads lines until "<cmd> <reply word> ..." and returns everything after cmd.
// The echo of the command line and interleaved log lines are skipped.
func (c *Console) awaitReply(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		ln, err := c.readLine(rctx)
		if err != nil {
			return "", fmt.Errorf("waiting for %s reply: %w", cmd, err)
		}
		ln = strings.TrimSpace(strings.TrimLeft(ln, "> \r\x08"))
		if i := strings.Index(ln, cmd+" "); i >= 0 {
			rest := strings.TrimSpace(ln[i+len(cmd)+1:])
			if f := strings.Fields(rest); len(f) > 0 && uploadReplyWords[f[0]] {
				c.logln("< %s %s", cmd, rest)
				return rest, nil
			}
		}
	}
}

// FirmwareVersion reads the firmware version ("v3.4.0") from `status` console output, "" if
// the board line does not carry one.
func FirmwareVersion(statusRaw string) string {
	return parseSerialPod("", statusRaw).Firmware
}
