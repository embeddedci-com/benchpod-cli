package fwrefusals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryExampleMatchesItsFormat(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range All() {
		if seen[r.ID] {
			t.Errorf("duplicate id %s", r.ID)
		}
		seen[r.ID] = true
		if r.File == "" || r.Format == "" || r.Example == "" || r.Kind == "" {
			t.Errorf("%s: incomplete entry %+v", r.ID, r)
		}
		if !r.Pattern().MatchString(r.Example) {
			t.Errorf("%s: example %q does not match format %q (%s)", r.ID, r.Example, r.Format, r.Pattern())
		}
	}
	if len(Tiers()) == 0 || TierFile() == "" {
		t.Fatal("no tiers pinned")
	}
}

// The firmware still has every pinned text. CI checks benchpod-firmware out and points
// BENCHPOD_FIRMWARE_DIR at it; locally a sibling checkout is used. A failure means the firmware's
// wording changed: update refusals.json and whatever in the CLI matches on that text.
func TestFirmwareStillSendsThePinnedTexts(t *testing.T) {
	dir := FirmwareDir()
	if dir == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("no benchpod-firmware checkout: set BENCHPOD_FIRMWARE_DIR")
		}
		t.Skip("no benchpod-firmware checkout next to this repository (set BENCHPOD_FIRMWARE_DIR)")
	}
	for _, p := range Check(dir) {
		t.Error(p)
	}
}

func TestCheckReportsAChangedText(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A firmware tree with every text as pinned, then one reworded.
	files := map[string]*strings.Builder{}
	for _, r := range All() {
		if files[r.File] == nil {
			files[r.File] = &strings.Builder{}
		}
		files[r.File].WriteString(`send_error(c, "` + r.Format + "\");\n")
	}
	tiers := &strings.Builder{}
	for c, tier := range Tiers() {
		tiers.WriteString(`{ "` + c + `", CMD_TIER_` + tier + " },\n")
	}
	files[TierFile()] = tiers
	for f, b := range files {
		write(f, b.String())
	}
	if p := Check(dir); len(p) != 0 {
		t.Fatalf("a matching tree reported %v", p)
	}
	lockedFile := ""
	for _, r := range All() {
		if r.ID == "lan_locked" {
			lockedFile = r.File
		}
	}
	write(lockedFile, strings.Replace(files[lockedFile].String(), "needs the cloud", "requires the cloud", 1))
	write(TierFile(), strings.Replace(tiers.String(), `"ota_begin", CMD_TIER_T3`, `"ota_begin", CMD_TIER_T2`, 1))
	p := Check(dir)
	if len(p) != 2 || !strings.Contains(p[0], "lan_locked") || !strings.Contains(p[1], "ota_begin") {
		t.Fatalf("problems = %v", p)
	}
}

func TestAdjacentLiteralsAreJoined(t *testing.T) {
	src := "return x ? \"busy: a capture, upload or update is using the PSRAM bus; \"\n" +
		"                \"try again when it ends\" : NULL;\n"
	if got := adjacent.ReplaceAllString(src, ""); !strings.Contains(got, `"`+
		"busy: a capture, upload or update is using the PSRAM bus; try again when it ends"+`"`) {
		t.Fatalf("not joined: %q", got)
	}
}
