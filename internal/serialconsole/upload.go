package serialconsole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// Chunks are at most 512 bytes, so one always fits the pod's 1 KB receive ring; each waits for
// its reply before the next goes out.

// UploadChunk is the largest upload-data chunk the firmware takes.
const UploadChunk = 512

const (
	uploadReplyTimeout  = 5 * time.Second   // begin, one chunk
	uploadEndTimeout    = 30 * time.Second  // the pod hashes the whole staged image
	uploadCommitTimeout = 180 * time.Second // erase + program + hash back a 4 MB slot, or a gateware swap
	uploadChunkAttempts = 5
)

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

// Blobs lists the pod's W25Q blob slots.
func (c *Console) Blobs(ctx context.Context) ([]BlobSlot, error) {
	out, err := c.sendCommand(ctx, "blobs")
	if err != nil {
		return nil, err
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
	if len(data) == 0 {
		return fmt.Errorf("upload %s: empty image", target)
	}
	if rt, ok := c.rw.(readTimeoutSetter); ok {
		_ = rt.SetReadTimeout(perReadTimeout)
	}
	sum := sha256.Sum256(data)
	begin := fmt.Sprintf("upload-begin %s %d %s %d", target, len(data), hex.EncodeToString(sum[:]), version)
	c.logln("> %s", begin)
	if err := c.writeLine(begin); err != nil {
		return fmt.Errorf("upload %s: %w", target, err)
	}
	if reply, err := c.awaitReply(ctx, "upload-begin", uploadReplyTimeout); err != nil {
		return fmt.Errorf("upload %s: %w", target, err)
	} else if reply != "ok" {
		return fmt.Errorf("upload %s: begin: %s", target, reply)
	}

	for off := 0; off < len(data); off += UploadChunk {
		end := off + UploadChunk
		if end > len(data) {
			end = len(data)
		}
		if err := c.uploadChunk(ctx, off, data[off:end]); err != nil {
			_ = c.writeLine("upload-abort")
			return fmt.Errorf("upload %s: %w", target, err)
		}
		if progress != nil {
			progress(end, len(data))
		}
	}

	if err := c.writeLine("upload-end"); err != nil {
		return fmt.Errorf("upload %s: %w", target, err)
	}
	if reply, err := c.awaitReply(ctx, "upload-end", uploadEndTimeout); err != nil {
		return fmt.Errorf("upload %s: %w", target, err)
	} else if reply != "ok" {
		return fmt.Errorf("upload %s: verify: %s", target, reply)
	}

	if err := c.writeLine("upload-commit"); err != nil {
		return fmt.Errorf("upload %s: %w", target, err)
	}
	reply, err := c.awaitReply(ctx, "upload-commit", uploadCommitTimeout)
	if err != nil {
		return fmt.Errorf("upload %s: %w", target, err)
	}
	if reply != "ok" && reply != "resetting" {
		return fmt.Errorf("upload %s: commit: %s", target, reply)
	}
	return nil
}

// uploadChunk sends one chunk and waits for "ok", resending on retry/busy.
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
			return fmt.Errorf("chunk at %d: %w", off, err)
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
