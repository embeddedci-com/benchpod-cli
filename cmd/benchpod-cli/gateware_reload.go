package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// After a downgrade (a dev build with gateware v55 back to a release that carries v54) the pod can
// keep running the newer gateware: its boot check runs before the CLI installs the blobs, so the
// slots still hold the dev build's images then, and the pod's own post-install check misses some
// cases (the emulation image on firmware 3.10.3 and earlier). Status then shows
// gateware != gateware_embedded. Once the blobs are in, the CLI reloads the running image kind
// from its new slot: fpga_image reflashes the iCE40 from the slot even when that kind already
// runs, so one call is enough.

// gatewareReloader is a blob target that can bring the running gateware in line with the
// firmware's after an install (the network API; the console has no fpga_image).
type gatewareReloader interface {
	ReloadStaleGateware(ctx context.Context, w io.Writer)
}

// installedGateware reports whether any of the installed blobs is a gateware image (gw0, gw1, gw2).
func installedGateware(done []blobEntry) bool {
	for _, e := range done {
		if strings.HasPrefix(e.Name, "gw") {
			return true
		}
	}
	return false
}

type gatewareVersions struct {
	Gateware         json.RawMessage `json:"gateware"`
	GatewareEmbedded int             `json:"gateware_embedded"`
}

func (s gatewareVersions) running() int { return podStatusInfo{Gateware: s.Gateware}.gateware() }

type fpgaImageReply struct {
	Image   int    `json:"image"`
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// lanCmd is one JSON command with its reply decoded into out (lanSession.cmd).
type lanCmd func(ctx context.Context, req map[string]any, out any) error

// reloadStaleGateware checks the pod's gateware against the one its firmware carries and, when
// they differ, reloads the running image kind from its slot. It only reports: the firmware and
// the blobs are in either way, and a restart of the pod also loads the right gateware.
func reloadStaleGateware(ctx context.Context, cmd lanCmd, w io.Writer) {
	var st gatewareVersions
	if cmd(ctx, map[string]any{"cmd": "status"}, &st) != nil {
		return
	}
	running, embedded := st.running(), st.GatewareEmbedded
	if running == 0 || embedded == 0 || running == embedded {
		return
	}
	restart := fmt.Sprintf("restart the pod (tap RESET) to load gateware v%d", embedded)
	var cur fpgaImageReply
	if err := cmd(ctx, map[string]any{"cmd": "fpga_image"}, &cur); err != nil {
		fmt.Fprintf(w, "blobs: the pod runs gateware v%d and its firmware carries v%d, and it does not report its image (%v): %s\n",
			running, embedded, err, restart)
		return
	}
	kind := cur.Image
	if kind < 0 || kind > 2 {
		kind = 0
	}
	name := cur.Name
	if name == "" {
		name = fmt.Sprintf("image %d", kind)
	}
	fmt.Fprintf(w, "blobs: the pod still runs gateware v%d and its firmware carries v%d: reloading the %s image from its new blob...\n",
		running, embedded, name)
	var rep fpgaImageReply
	if err := cmd(ctx, map[string]any{"cmd": "fpga_image", "image": kind}, &rep); err != nil {
		fmt.Fprintf(w, "blobs: the reload failed (%v): %s\n", err, restart)
		return
	}
	var after gatewareVersions
	if cmd(ctx, map[string]any{"cmd": "status"}, &after) == nil && after.running() != 0 {
		rep.Version = after.running()
	}
	if rep.Version != embedded {
		fmt.Fprintf(w, "blobs: the pod still runs gateware v%d after the reload: %s\n", rep.Version, restart)
		return
	}
	fmt.Fprintf(w, "blobs: the pod now runs gateware v%d\n", rep.Version)
}

func (l lanBlobs) ReloadStaleGateware(ctx context.Context, w io.Writer) {
	reloadStaleGateware(ctx, l.lan.cmd, w)
}
