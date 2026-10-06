// Package fwsign checks the detached signed manifests ("<asset>.sig") that BenchPod firmware
// releases publish next to bench_pod_stm32.bin and the blob-*.bin images. The format is fixed by
// benchpod-firmware docs/design/firmware-signing.md and tools/fwsign.py; the firmware's
// fw_sign.c gives the same result words.
//
// The CLI only reports what it finds: nothing here refuses an install.
//
// Manifest layout (128 bytes, little-endian):
//
//	 0  4 magic "BPSG"
//	 4  2 format (1)
//	 6  1 target: 0 firmware, 1 gw0, 2 gw1, 3 esp
//	 7  1 flags
//	 8  4 image size
//	12 32 SHA-256 of the image
//	44  4 asset version
//	48  4 release, packed major<<16|minor<<8|patch
//	52  2 fw_info layout (firmware only)
//	54  2 fw_info min_flash_kb (firmware only)
//	56  8 key_id: first 8 bytes of SHA-512(public key)
//	64 64 Ed25519 signature over Context || bytes 0..63
package fwsign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManifestLen is the size of a .sig file.
const ManifestLen = 128

const (
	signedLen = 64
	format    = 1
	magic     = "BPSG"
)

// Context is prefixed to the signed bytes (the "\x00" ends the context string).
const Context = "benchpod-fw-sign:v1\x00"

// Result words, the same as the firmware and tools/fwsign.py.
const (
	ResultOK         = "ok"
	ResultNone       = "none"        // no .sig
	ResultFormat     = "format"      // wrong length, magic, format or target number
	ResultUnknownKey = "unknown-key" // signed by a key this host does not trust
	ResultSignature  = "signature"   // the Ed25519 signature does not verify
	ResultTarget     = "target"      // signed for another slot (a firmware manifest on a blob, ...)
	ResultImage      = "image"       // size or SHA-256 differ from the image
)

// Target numbers, as in the firmware's ota_target_t.
const (
	TargetFirmware uint8 = 0
	TargetGW0      uint8 = 1
	TargetGW1      uint8 = 2
	TargetESP      uint8 = 3
)

var targetNames = []string{"firmware", "gw0", "gw1", "esp"}

// TargetByName maps "firmware", "gw0", "gw1", "esp" to its number.
func TargetByName(name string) (uint8, bool) {
	for i, n := range targetNames {
		if n == name {
			return uint8(i), true
		}
	}
	return 0, false
}

// TargetName is the name of a target number, "" if unknown.
func TargetName(t uint8) string {
	if int(t) < len(targetNames) {
		return targetNames[t]
	}
	return ""
}

// Manifest is a parsed .sig.
type Manifest struct {
	Target     uint8
	Flags      uint8
	Size       uint32
	SHA256     [32]byte
	Version    uint32
	Release    uint32 // packed major<<16|minor<<8|patch
	Layout     uint16
	MinFlashKB uint16
	KeyID      [8]byte
	Signature  [64]byte
	Raw        []byte // the 128 bytes as read
}

// ReleaseString is the release as "X.Y.Z".
func (m Manifest) ReleaseString() string { return UnpackVersion(m.Release) }

// KeyIDHex is the key_id in hex.
func (m Manifest) KeyIDHex() string { return hex.EncodeToString(m.KeyID[:]) }

// UnpackVersion turns a packed major<<16|minor<<8|patch into "X.Y.Z".
func UnpackVersion(v uint32) string {
	return fmt.Sprintf("%d.%d.%d", v>>16, (v>>8)&0xFF, v&0xFF)
}

// Parse reads a manifest. It checks the length, magic, format and target number only.
func Parse(sig []byte) (Manifest, error) {
	var m Manifest
	if len(sig) != ManifestLen {
		return m, fmt.Errorf("manifest is %d bytes, want %d", len(sig), ManifestLen)
	}
	if string(sig[:4]) != magic {
		return m, errors.New("bad magic")
	}
	if f := binary.LittleEndian.Uint16(sig[4:]); f != format {
		return m, fmt.Errorf("unknown format %d", f)
	}
	m.Target = sig[6]
	if TargetName(m.Target) == "" {
		return m, fmt.Errorf("unknown target %d", m.Target)
	}
	m.Flags = sig[7]
	m.Size = binary.LittleEndian.Uint32(sig[8:])
	copy(m.SHA256[:], sig[12:44])
	m.Version = binary.LittleEndian.Uint32(sig[44:])
	m.Release = binary.LittleEndian.Uint32(sig[48:])
	m.Layout = binary.LittleEndian.Uint16(sig[52:])
	m.MinFlashKB = binary.LittleEndian.Uint16(sig[54:])
	copy(m.KeyID[:], sig[56:64])
	copy(m.Signature[:], sig[64:])
	m.Raw = append([]byte(nil), sig...)
	return m, nil
}

// Key is a trusted public key with a name for reports ("release-1", "dev").
type Key struct {
	Name   string
	Public ed25519.PublicKey
}

// ID is the key's key_id: the first 8 bytes of SHA-512(public key).
func (k Key) ID() [8]byte {
	sum := sha512.Sum512(k.Public)
	var id [8]byte
	copy(id[:], sum[:8])
	return id
}

// IDHex is the key_id in hex.
func (k Key) IDHex() string {
	id := k.ID()
	return hex.EncodeToString(id[:])
}

// Verify checks sig against image for target with keys. It returns the result word, the name
// of the key that matched ("" unless the signature verified) and the manifest (nil when it does
// not parse). The checks run in the firmware's order: format, unknown-key, signature, target,
// image.
func Verify(sig, image []byte, target uint8, keys []Key) (result, keyName string, m *Manifest) {
	parsed, err := Parse(sig)
	if err != nil {
		return ResultFormat, "", nil
	}
	m = &parsed
	var key *Key
	for i := range keys {
		if keys[i].ID() == parsed.KeyID && len(keys[i].Public) == ed25519.PublicKeySize {
			key = &keys[i]
			break
		}
	}
	if key == nil {
		return ResultUnknownKey, "", m
	}
	msg := append([]byte(Context), sig[:signedLen]...)
	if !ed25519.Verify(key.Public, msg, sig[signedLen:]) {
		return ResultSignature, "", m
	}
	if parsed.Target != target {
		return ResultTarget, key.Name, m
	}
	sum := sha256.Sum256(image)
	if int(parsed.Size) != len(image) || !bytes.Equal(parsed.SHA256[:], sum[:]) {
		return ResultImage, key.Name, m
	}
	return ResultOK, key.Name, m
}

// Release public keys, from benchpod-firmware stm32h563/keys/release-1.pub and release-2.pub
// (the firmware embeds the same two: the current key and an offline spare for rotation).
var releaseKeys = []struct{ name, hex string }{
	{"release-1", "3cf627d9ece2f574a1c1414deadb7291a91d87b2bb4c57e3ebf957c52a1178c7"},
	{"release-2", "660fe3d0c0edb30393c9a5f046b8222c62a20bbab66da8292b8318b5bc04a315"},
}

// ReleaseKeys are the keys release builds are signed with.
func ReleaseKeys() []Key {
	out := make([]Key, 0, len(releaseKeys))
	for _, k := range releaseKeys {
		pub, err := hex.DecodeString(k.hex)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			panic("fwsign: bad embedded key " + k.name)
		}
		out = append(out, Key{Name: k.name, Public: pub})
	}
	return out
}

// DevKeyPath is where `make dev-key` in benchpod-firmware leaves the developer signing key: the
// 32-byte Ed25519 seed as 64 hex characters. A var so tests can point it elsewhere.
var DevKeyPath = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "benchpod", "fw-signing-dev.key")
}

// DevKey reads the developer key, if there is one.
func DevKey() (Key, bool) {
	p := DevKeyPath()
	if p == "" {
		return Key{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return Key{}, false
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return Key{}, false
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return Key{Name: "dev", Public: priv.Public().(ed25519.PublicKey)}, true
}

// TrustedKeys are the release keys plus the developer key when this host has one, so a
// developer's own builds check "ok" too.
func TrustedKeys() []Key {
	keys := ReleaseKeys()
	if k, ok := DevKey(); ok {
		keys = append(keys, k)
	}
	return keys
}

// Report is the outcome of checking one asset, for printing.
type Report struct {
	Result   string    // one of the Result* words
	KeyName  string    // the trusted key that matched ("release-1", "dev"), "" if none did
	Manifest *Manifest // nil for "none" and "format"
}

// Check verifies sig (nil = there is no .sig) against image for target with TrustedKeys.
func Check(sig, image []byte, target uint8) Report {
	if sig == nil {
		return Report{Result: ResultNone}
	}
	res, name, m := Verify(sig, image, target, TrustedKeys())
	return Report{Result: res, KeyName: name, Manifest: m}
}

// Detail describes the key and release for a report line: "release-1 key 9b5cc58445e62d88,
// release 3.6.0", or "key 0011223344556677" for a key this host does not trust. "" when the
// manifest did not parse.
func (r Report) Detail() string {
	if r.Manifest == nil {
		return ""
	}
	key := "key " + r.Manifest.KeyIDHex()
	if r.KeyName != "" {
		key = r.KeyName + " " + key
	}
	if r.Manifest.Release != 0 {
		key += ", release " + r.Manifest.ReleaseString()
	}
	return key
}

// Forwardable reports whether the manifest is worth handing to the pod: it parses and nothing
// on this host proved it wrong. An "unknown-key" manifest still goes, since the pod may trust a
// key this host lacks (another developer's key, say).
func (r Report) Forwardable() bool {
	return r.Manifest != nil && (r.Result == ResultOK || r.Result == ResultUnknownKey)
}
