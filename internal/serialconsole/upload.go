package serialconsole

import "errors"

// The types the blob installer shares (cmd/benchpod-cli blobs_target.go): the pod's W25Q slots as
// blob_status reports them, and what a signed upload learned. The console upload (upload-begin and
// friends, firmware before 3.9.0) is gone: blobs and firmware go over the network API.

// BlobSlot is one of the pod's W25Q blob slots (blob_status).
type BlobSlot struct {
	Name    string // "gw0", "gw1", "gw2", "esp", "bridge" (the Wi-Fi bridge's firmware, on pods that support one)
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
// ManifestLen is the size of a signed manifest (a release's .sig file).
const ManifestLen = 128

// SigReport is what a signed upload learned about the pod's own signature check.
type SigReport struct {
	Supported bool   // the firmware takes upload-sig
	Sent      bool   // the manifest reached the pod before upload-begin
	Result    string // the pod's check of this upload ("ok", "none", "signature", ...); "" if not known
	KeyID     string // the key_id the pod matched, "" if none
}

// FirmwareVersion reads the firmware version ("v3.4.0") from `status` console output, "" if
// the board line does not carry one.
func FirmwareVersion(statusRaw string) string {
	return parseSerialPod("", statusRaw).Firmware
}
