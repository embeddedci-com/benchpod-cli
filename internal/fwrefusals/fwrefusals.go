// Package fwrefusals holds the firmware refusal texts the CLI shows or matches on, copied from
// benchpod-firmware, for tests. Each entry keeps the C string literal as it appears in the
// firmware source and one rendering of it; Check verifies the literals against a firmware
// checkout, so a wording change in the firmware fails the CLI tests instead of silently breaking
// a hint or a retry.
package fwrefusals

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed refusals.json
var raw []byte

// Refusal is one firmware text.
type Refusal struct {
	ID      string `json:"id"`
	File    string `json:"file"`    // relative to the firmware repository root
	Kind    string `json:"kind"`    // "json" (an error reply), "console", "console_upload"
	Format  string `json:"format"`  // the C literal, escapes kept
	Example string `json:"example"` // one rendering, without a trailing newline or prompt
}

type doc struct {
	Refusals []Refusal        `json:"refusals"`
	Tiers    map[string]string `json:"tiers"`
}

func load() doc {
	var d doc
	if err := json.Unmarshal(raw, &d); err != nil {
		panic(fmt.Sprintf("fwrefusals: %v", err))
	}
	return d
}

// All returns every refusal.
func All() []Refusal { return load().Refusals }

// Example returns the example text of refusal id; it panics on an unknown id.
func Example(id string) string {
	for _, r := range All() {
		if r.ID == id {
			return r.Example
		}
	}
	panic("fwrefusals: unknown id " + id)
}

// TierFile is the firmware file the tiers come from.
func TierFile() string { return load().Tiers["_file"] }

// Tiers maps a command to its pinned tier name ("T0".."T3").
func Tiers() map[string]string {
	out := map[string]string{}
	for k, v := range load().Tiers {
		if !strings.HasPrefix(k, "_") {
			out[k] = v
		}
	}
	return out
}

var cEscapes = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\r`, "\r", `\n`, "\n", `\t`, "\t")

// Pattern turns a refusal's C format into a regexp that matches its example: printf verbs
// match any text, a trailing newline and console prompt are dropped.
func (r Refusal) Pattern() *regexp.Regexp {
	text := strings.TrimRight(cEscapes.Replace(r.Format), "\r\n> ")
	verb := regexp.MustCompile(`%[-+ #0]*[0-9]*(?:l|ll|z|h)?[sdiuxX]`)
	var b strings.Builder
	b.WriteString("^")
	last := 0
	for _, loc := range verb.FindAllStringIndex(text, -1) {
		b.WriteString(regexp.QuoteMeta(text[last:loc[0]]))
		b.WriteString(".+")
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(text[last:]))
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// FirmwareDir returns a benchpod-firmware checkout to check against: $BENCHPOD_FIRMWARE_DIR, else
// a benchpod-firmware directory next to this repository. "" when there is none.
func FirmwareDir() string {
	if d := os.Getenv("BENCHPOD_FIRMWARE_DIR"); d != "" {
		return d
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	for dir := wd; ; {
		cand := filepath.Join(filepath.Dir(dir), "benchpod-firmware")
		if _, err := os.Stat(filepath.Join(cand, "stm32h563", "src")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return cand
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// adjacent joins C string literals split over lines ("a " "b" -> "a b").
var adjacent = regexp.MustCompile(`"\s*\n\s*"`)

// Check returns one problem per refusal format or tier missing from the firmware at dir.
func Check(dir string) []string {
	var problems []string
	sources := map[string]string{}
	read := func(file string) (string, error) {
		if s, ok := sources[file]; ok {
			return s, nil
		}
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return "", err
		}
		s := adjacent.ReplaceAllString(string(b), "")
		sources[file] = s
		return s, nil
	}
	for _, r := range All() {
		src, err := read(r.File)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", r.ID, err))
			continue
		}
		if !strings.Contains(src, `"`+r.Format+`"`) {
			problems = append(problems, fmt.Sprintf("%s: %q is no longer in %s", r.ID, r.Format, r.File))
		}
	}
	src, err := read(TierFile())
	if err != nil {
		return append(problems, fmt.Sprintf("tiers: %v", err))
	}
	tiers := Tiers()
	cmds := make([]string, 0, len(tiers))
	for c := range tiers {
		cmds = append(cmds, c)
	}
	sort.Strings(cmds)
	for _, c := range cmds {
		re := regexp.MustCompile(`\{\s*"` + regexp.QuoteMeta(c) + `"\s*,\s*CMD_TIER_` + tiers[c] + `\s*\}`)
		if !re.MatchString(src) {
			problems = append(problems, fmt.Sprintf("tier: %s is no longer %s in %s", c, tiers[c], TierFile()))
		}
	}
	return problems
}
