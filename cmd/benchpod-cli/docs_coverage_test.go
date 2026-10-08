package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// The README's "Command reference" is the public reference for every command and flag (the
// embeddedci.com CLI page is built from the same list). These tests walk the real cobra tree so
// a new command or flag cannot ship undocumented, and a removed one cannot stay documented.
//
// To refresh the embeddedci.com snapshot (embeddedci-server webapp/src/docs/coverage/cli.json):
//
//	BENCHPOD_DOCS_TREE=/path/to/cli.json go test ./cmd/benchpod-cli -run TestDocsCommandTree

// docsUndocumentedFlags lists flags that are deliberately left out of the reference, as
// "command path --flag". Keep it short: a flag a user can type belongs in the README.
var docsUndocumentedFlags = map[string]bool{}

// docsNonCommands are words that follow "benchpod " in the README without being a subcommand:
// cobra's built-in commands and prose.
var docsNonCommands = map[string]bool{
	"help":       true,
	"completion": true,
}

type docsFlag struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default"`
	Usage   string `json:"usage"`
}

type docsCommand struct {
	Path  string     `json:"path"`
	Use   string     `json:"use"`
	Short string     `json:"short"`
	Flags []docsFlag `json:"flags"`
}

// docsCommandTree flattens the visible cobra tree: every command with its own (local) flags.
// The root's persistent flags are the global flags.
func docsCommandTree() []docsCommand {
	var out []docsCommand
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Hidden {
			return
		}
		dc := docsCommand{Path: c.CommandPath(), Use: c.Use, Short: c.Short, Flags: []docsFlag{}}
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden || f.Name == "help" || f.Name == "version" {
				return
			}
			dc.Flags = append(dc.Flags, docsFlag{Name: f.Name, Type: f.Value.Type(), Default: f.DefValue, Usage: f.Usage})
		})
		out = append(out, dc)
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(newRootCmd())
	return out
}

// readmeReference returns the README's "## Command reference" section split by its "###"
// headings, keyed by the command path each heading names.
func readmeReference(t *testing.T) (map[string]string, string) {
	t.Helper()
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	readme := string(raw)
	start := strings.Index(readme, "\n## Command reference\n")
	if start < 0 {
		t.Fatal(`README.md has no "## Command reference" section`)
	}
	ref := readme[start+1:]
	if end := strings.Index(ref[3:], "\n## "); end >= 0 {
		ref = ref[:end+3]
	}
	// Every "###" heading ends the section before it; the ones that name a command start one.
	anyHeading := regexp.MustCompile("(?m)^### ")
	command := regexp.MustCompile("^### `(benchpod(?: [a-z][a-z0-9-]*)*)")
	sections := map[string]string{}
	locs := anyHeading.FindAllStringIndex(ref, -1)
	for i, loc := range locs {
		end := len(ref)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		m := command.FindStringSubmatch(ref[loc[0]:end])
		if m == nil {
			continue
		}
		path := m[1]
		if _, dup := sections[path]; dup {
			t.Errorf("README command reference: %q has two sections", path)
		}
		sections[path] = ref[loc[0]:end]
	}
	return sections, readme
}

// defaultForms is how a flag's default may be written in its reference row: as cobra prints it,
// and for a duration also compactly ("1m" for 1m0s) or in seconds ("60s").
func defaultForms(f docsFlag) []string {
	forms := []string{f.Default}
	if f.Type != "duration" {
		return forms
	}
	d, err := time.ParseDuration(f.Default)
	if err != nil {
		return forms
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") || strings.HasSuffix(s, "h0m0s") {
		forms = append(forms, strings.TrimSuffix(strings.TrimSuffix(s, "0s"), "0m"))
	}
	if d%time.Second == 0 {
		forms = append(forms, fmt.Sprintf("%ds", int64(d/time.Second)))
	}
	return forms
}

// trivialDefault reports whether a default is the zero value, which the reference may leave out.
func trivialDefault(v string) bool {
	switch v {
	case "", "0", "false", "[]", "0s":
		return true
	}
	return false
}

func TestDocsCommandTree(t *testing.T) {
	tree := docsCommandTree()
	if out := os.Getenv("BENCHPOD_DOCS_TREE"); out != "" {
		raw, err := json.MarshalIndent(tree, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	sections, _ := readmeReference(t)
	known := map[string]bool{}
	for _, c := range tree {
		known[c.Path] = true
		section, ok := sections[c.Path]
		if !ok {
			t.Errorf("README command reference has no \"### `%s`\" section", c.Path)
			continue
		}
		for _, f := range c.Flags {
			if docsUndocumentedFlags[c.Path+" --"+f.Name] {
				continue
			}
			flagRe := regexp.MustCompile("`--" + regexp.QuoteMeta(f.Name) + "(?:[ =`])")
			row := ""
			for _, line := range strings.Split(section, "\n") {
				// A table row whose first cell names the flag.
				cells := strings.Split(line, "|")
				if strings.HasPrefix(line, "|") && len(cells) > 2 && flagRe.MatchString(cells[1]+" ") {
					row = line
					break
				}
			}
			if row == "" {
				t.Errorf("README: %s --%s has no row in its reference section's flag table", c.Path, f.Name)
				continue
			}
			if trivialDefault(f.Default) {
				continue
			}
			found := false
			for _, form := range defaultForms(f) {
				if form != "" && strings.Contains(row, form) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("README: %s --%s: its row does not show the default %q:\n%s", c.Path, f.Name, f.Default, row)
			}
		}
	}
	var stale []string
	for path := range sections {
		if !known[path] {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	for _, path := range stale {
		t.Errorf("README command reference documents %q, which is not a command", path)
	}
}

// TestDocsNoUnknownCommands catches examples anywhere in the README that name a command the CLI
// does not have (for example one that was renamed or removed).
func TestDocsNoUnknownCommands(t *testing.T) {
	_, readme := readmeReference(t)
	root := newRootCmd()
	top := map[string]bool{}
	for _, c := range root.Commands() {
		top[c.Name()] = true
		for _, a := range c.Aliases {
			top[a] = true
		}
	}
	use := regexp.MustCompile(`(?:^|[\s(` + "`" + `$])benchpod ([a-z][a-z0-9-]*)`)
	for _, m := range use.FindAllStringSubmatch(readme, -1) {
		if !top[m[1]] && !docsNonCommands[m[1]] {
			t.Errorf("README mentions `benchpod %s`, which is not a command", m[1])
		}
	}
}
