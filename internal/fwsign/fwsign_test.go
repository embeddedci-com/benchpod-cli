package fwsign

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testdata/fwsign_vectors.json is copied from benchpod-firmware
// stm32h563/test/vectors/fwsign_vectors.json (written by tools/fwsign.py vectors), so this check,
// the firmware's and the Python one agree on the same bytes.
type vectors struct {
	PublicKey string            `json:"public_key"`
	KeyID     string            `json:"key_id"`
	Images    map[string]string `json:"images"`
	Cases     []struct {
		Name   string `json:"name"`
		Sig    string `json:"sig"`
		Image  string `json:"image"`
		Target string `json:"target"`
		Want   string `json:"want"`
	} `json:"cases"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fwsign_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVectors(t *testing.T) {
	v := loadVectors(t)
	key := Key{Name: "vector", Public: mustHex(t, v.PublicKey)}
	if key.IDHex() != v.KeyID {
		t.Fatalf("key_id %s, want %s", key.IDHex(), v.KeyID)
	}
	keys := append(ReleaseKeys(), key)
	if len(v.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			target, ok := TargetByName(c.Target)
			if !ok {
				t.Fatalf("target %q", c.Target)
			}
			img, ok := v.Images[c.Image]
			if !ok {
				t.Fatalf("image %q", c.Image)
			}
			got, name, m := Verify(mustHex(t, c.Sig), mustHex(t, img), target, keys)
			if got != c.Want {
				t.Fatalf("got %q, want %q", got, c.Want)
			}
			if got == ResultOK && name != "vector" {
				t.Fatalf("matched key %q", name)
			}
			if (got == ResultFormat) != (m == nil) {
				t.Fatalf("manifest %v for %q", m, got)
			}
		})
	}
}

func TestParseFields(t *testing.T) {
	v := loadVectors(t)
	m, err := Parse(mustHex(t, v.Cases[0].Sig)) // good firmware, release 3.6.0
	if err != nil {
		t.Fatal(err)
	}
	if m.Target != TargetFirmware || m.ReleaseString() != "3.6.0" || m.Version != 0x030600 ||
		m.Layout != 2 || m.MinFlashKB != 1024 || m.KeyIDHex() != v.KeyID || int(m.Size) != len(v.Images["firmware"])/2 {
		t.Fatalf("%+v", m)
	}
	blob, err := Parse(mustHex(t, v.Cases[1].Sig))
	if err != nil || blob.Target != TargetGW1 || blob.Version != 45 {
		t.Fatalf("%+v %v", blob, err)
	}
}

func TestReleaseKeys(t *testing.T) {
	keys := ReleaseKeys()
	if len(keys) != 2 || keys[0].Name != "release-1" || keys[1].Name != "release-2" {
		t.Fatalf("%+v", keys)
	}
	if keys[0].IDHex() == keys[1].IDHex() {
		t.Fatal("same key_id twice")
	}
}

// A developer's key file makes images signed with it check "ok" under the name "dev".
func TestDevKey(t *testing.T) {
	dir := t.TempDir()
	orig := DevKeyPath
	t.Cleanup(func() { DevKeyPath = orig })
	DevKeyPath = func() string { return filepath.Join(dir, "fw-signing-dev.key") }

	if _, ok := DevKey(); ok {
		t.Fatal("dev key without a file")
	}
	if len(TrustedKeys()) != 2 {
		t.Fatal("trusted keys without a dev key")
	}
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i) // the vectors' signing key
	}
	if err := os.WriteFile(DevKeyPath(), []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, ok := DevKey()
	if !ok || k.Name != "dev" {
		t.Fatalf("%+v %v", k, ok)
	}
	v := loadVectors(t)
	if k.IDHex() != v.KeyID {
		t.Fatalf("dev key_id %s, want %s", k.IDHex(), v.KeyID)
	}
	r := Check(mustHex(t, v.Cases[0].Sig), mustHex(t, v.Images["firmware"]), TargetFirmware)
	if r.Result != ResultOK || r.KeyName != "dev" || !r.Forwardable() {
		t.Fatalf("%+v", r)
	}
	if d := r.Detail(); d != "dev key "+v.KeyID+", release 3.6.0" {
		t.Fatalf("detail %q", d)
	}
}

func TestCheckWithoutSig(t *testing.T) {
	r := Check(nil, []byte("x"), TargetFirmware)
	if r.Result != ResultNone || r.Forwardable() || r.Detail() != "" {
		t.Fatalf("%+v", r)
	}
}
