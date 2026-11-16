// Package errcatalog is the BenchPod error catalog: benchpod-firmware's errors/errors.json
// (generated from errors/catalog.toml), copied here by scripts/sync-error-catalog.sh. A pod that
// refuses a request sends a stable code (BP0001 and up) with its values; the CLI shows the
// message and the fix for the CLI from this catalog.
package errcatalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed errors.json
var raw []byte

// Entry is one code's catalog entry.
type Entry struct {
	Name       string            `json:"name"`
	Area       string            `json:"area"`
	Since      string            `json:"since"`
	Message    string            `json:"message"`
	Cause      string            `json:"cause"`
	Args       map[string]string `json:"args"`
	Fix        map[string]string `json:"fix"`
	Retired    string            `json:"retired"`
	ReplacedBy string            `json:"replaced_by"`
}

type catalog struct {
	DocsURL string           `json:"docs_url"`
	Codes   map[string]Entry `json:"codes"`
}

var cat = func() catalog {
	var c catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		panic("errcatalog: " + err.Error())
	}
	return c
}()

// Lookup returns a code's entry.
func Lookup(code string) (Entry, bool) {
	e, ok := cat.Codes[strings.ToUpper(strings.TrimSpace(code))]
	return e, ok
}

// Count is how many codes the catalog has.
func Count() int { return len(cat.Codes) }

// DocsURL is a code's docs page.
func DocsURL(code string) string {
	return strings.ReplaceAll(cat.DocsURL, "{code}", strings.ToUpper(strings.TrimSpace(code)))
}

var field = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

// Fill replaces {name} with the value of that arg ("?" when it is missing).
func Fill(template string, args map[string]any) string {
	return field.ReplaceAllStringFunc(template, func(m string) string {
		if v, ok := args[m[1:len(m)-1]]; ok {
			return fmt.Sprint(v)
		}
		return "?"
	})
}

// Render is one refusal for a surface ("cli", "sdk", "mcp", "web"): "BP0009: <message>; <fix>
// (<docs>)". ok is false for a code the catalog does not have.
func Render(code string, args map[string]any, surface string) (string, bool) {
	e, found := Lookup(code)
	if !found {
		return "", false
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	return fmt.Sprintf("%s: %s; %s (%s)", code, Fill(e.Message, args), Fill(e.Fix[surface], args), DocsURL(code)), true
}

// Explain is a code's whole entry as text, for `benchpod explain`.
func Explain(code string) (string, bool) {
	e, found := Lookup(code)
	if !found {
		return "", false
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n%s\n\n%s\n\nFix: %s\n", code, e.Name, e.Message, e.Cause, e.Fix["cli"])
	if e.Retired != "" {
		fmt.Fprintf(&b, "\nRetired in firmware %s", e.Retired)
		if e.ReplacedBy != "" {
			fmt.Fprintf(&b, ", replaced by %s", e.ReplacedBy)
		}
		b.WriteString(".\n")
	}
	fmt.Fprintf(&b, "\n%s\n", DocsURL(code))
	return b.String(), true
}
