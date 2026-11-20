package main

import (
	"testing"

	"github.com/spf13/cobra"
)

// The commands that show or change a pod's firmware still run against firmware this CLI is too
// old for; everything else stops with exit code 6.
func TestAllowsNewerPod(t *testing.T) {
	root := &cobra.Command{Use: "benchpod"}
	sub := func(name string, parent *cobra.Command) *cobra.Command {
		c := &cobra.Command{Use: name}
		parent.AddCommand(c)
		return c
	}
	for _, name := range []string{"status", "flash-self", "install-blobs", "dfu", "bootsel", "discover", "identity"} {
		if !allowsAnyPodAPI(sub(name, root)) {
			t.Errorf("%s: want allowed", name)
		}
	}
	for _, name := range []string{"uart", "la", "power"} {
		if allowsAnyPodAPI(sub(name, root)) {
			t.Errorf("%s: want stopped", name)
		}
	}
	// A nested "status" (sensor status, bus status) is not the top-level status.
	sensor := sub("sensor", root)
	if allowsAnyPodAPI(sub("status", sensor)) {
		t.Error("sensor status: want stopped")
	}
	if allowsAnyPodAPI(root) {
		t.Error("root: want stopped")
	}
}
