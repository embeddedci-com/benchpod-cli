package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// orgFor runs the root command with args (plus a probe subcommand) and returns the organization
// a cloud client built in that run acts in.
func orgFor(t *testing.T, args ...string) string {
	t.Helper()
	t.Cleanup(func() { cloudOrg = "" })
	var got string
	root := newRootCmd()
	root.AddCommand(&cobra.Command{Use: "probe-org", RunE: func(*cobra.Command, []string) error {
		got = newServerAPI("http://example.invalid").Org
		return nil
	}})
	root.SetArgs(append(args, "probe-org"))
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestOrgFlagAndEnv(t *testing.T) {
	if got := orgFor(t); got != "" {
		t.Errorf("no --org: %q, want the active organization (empty)", got)
	}
	if got := orgFor(t, "--org", " test-lab "); got != "test-lab" {
		t.Errorf("--org: %q", got)
	}
	t.Setenv("BENCHPOD_ORG", "from-env")
	if got := orgFor(t); got != "from-env" {
		t.Errorf("BENCHPOD_ORG: %q", got)
	}
	if got := orgFor(t, "--org", "flag-wins"); got != "flag-wins" {
		t.Errorf("--org over BENCHPOD_ORG: %q", got)
	}
}
