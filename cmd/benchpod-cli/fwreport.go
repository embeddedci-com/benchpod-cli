package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
)

// Firmware-update reports: after this CLI changes a pod's firmware (flash-self) or its blobs
// (install-blobs), it tells embeddedci.com which pod went from which version to which, so the
// server keeps the pod's version history next to what the pod reports when it connects.
//
// Best effort: it never blocks or fails the update (a short timeout), says nothing when nobody
// is signed in, and keeps a report that could not be sent for one more try on a later run.
// Opt out with BENCHPOD_NO_FIRMWARE_REPORT=1 or --no-firmware-report.

// fwEvent is POST /api/benchpod/firmware-events.
type fwEvent struct {
	PublicKey   string      `json:"public_key"`
	DeviceID    string      `json:"device_id,omitempty"`
	Name        string      `json:"name,omitempty"`
	Kind        string      `json:"kind"` // "firmware" or "blobs"
	OldVersion  string      `json:"old_version"`
	NewVersion  string      `json:"new_version"`
	OldGateware int         `json:"old_gateware"`
	NewGateware int         `json:"new_gateware"`
	Blobs       []fwBlobVer `json:"blobs,omitempty"`
	Method      string      `json:"method"` // "dfu", "usb_network", "lan", "cloud"
	CLIVersion  string      `json:"cli_version"`
	At          string      `json:"at"`
}

type fwBlobVer struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// podVersions is what a report says about the pod at one moment.
type podVersions struct {
	PublicKey, ShortID, Version string
	Gateware                    int
}

var gatewareRe = regexp.MustCompile(`gateware v(\d+)`)

// consoleVersions reads the pod's identity and versions from its console; empty fields when
// unknown.
func consoleVersions(ctx context.Context, c *serialconsole.Console) podVersions {
	var v podVersions
	if id, err := c.Identity(ctx); err == nil {
		v.PublicKey, v.ShortID = id.PublicKey, id.ShortID
	}
	if raw, err := c.Status(ctx); err == nil {
		v.Version = strings.TrimPrefix(serialconsole.FirmwareVersion(raw), "v")
		if m := gatewareRe.FindStringSubmatch(raw); m != nil {
			v.Gateware, _ = strconv.Atoi(m[1])
		}
	}
	return v
}

// fwReportOptOut reports whether the user turned the reports off.
func (g *globalFlags) fwReportOptOut() bool {
	if g.noFirmwareReport {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BENCHPOD_NO_FIRMWARE_REPORT"))) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// fwReportTimeout bounds one send.
const fwReportTimeout = 5 * time.Second

// fwReportQueueMax caps the reports kept for a later try.
const fwReportQueueMax = 20

// fwReportPost sends one report. A variable so tests can replace it.
var fwReportPost = func(ctx context.Context, ev fwEvent) error {
	api := &serverapi.Client{BaseURL: cloudServerURL()}
	cred, err := cloudCredential(ctx, api)
	if err != nil {
		return errNoCloudCredential // not signed in: nothing to send it with
	}
	body, _ := json.Marshal(ev)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api.BaseURL+"/api/benchpod/firmware-events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", serverapi.Authorization(cred))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode/100 == 4 && resp.StatusCode != http.StatusTooManyRequests {
			return errFWReportRejected // the server will not take it: no point retrying
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// errFWReportRejected is a report the server refused (4xx other than 429): dropped, not queued.
var errFWReportRejected = errors.New("firmware report rejected")

// fwReportQueuePath is where unsent reports wait: next to the sign-in token.
var fwReportQueuePath = func() string {
	p, err := resolveTokenPath("")
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "firmware-reports.jsonl")
}

type queuedEvent struct {
	Event fwEvent `json:"event"`
	Tries int     `json:"tries"`
}

// reportFirmware sends ev (and anything left from earlier runs) without ever failing the
// caller. A report whose first send fails is queued for one more try on a later run.
func (g *globalFlags) reportFirmware(ev fwEvent) {
	if g.fwReportOptOut() || ev.PublicKey == "" {
		return
	}
	ev.CLIVersion = version
	if ev.At == "" {
		ev.At = time.Now().UTC().Format(time.RFC3339)
	}
	pending := append(readFWQueue(), queuedEvent{Event: ev})
	var keep []queuedEvent
	for _, q := range pending {
		ctx, cancel := context.WithTimeout(context.Background(), fwReportTimeout)
		err := fwReportPost(ctx, q.Event)
		cancel()
		if err == errNoCloudCredential {
			return // not signed in: send nothing, keep nothing (the queue stays as it was)
		}
		if err == nil || err == errFWReportRejected {
			continue
		}
		if q.Tries++; q.Tries < 2 {
			keep = append(keep, q)
		}
	}
	writeFWQueue(keep)
}

func readFWQueue() []queuedEvent {
	path := fwReportQueuePath()
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []queuedEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var q queuedEvent
		if json.Unmarshal(sc.Bytes(), &q) == nil && q.Event.PublicKey != "" {
			out = append(out, q)
		}
	}
	return out
}

func writeFWQueue(q []queuedEvent) {
	path := fwReportQueuePath()
	if path == "" {
		return
	}
	if len(q) == 0 {
		_ = os.Remove(path)
		return
	}
	if len(q) > fwReportQueueMax {
		q = q[len(q)-fwReportQueueMax:]
	}
	var b bytes.Buffer
	for _, e := range q {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, b.Bytes(), 0o600)
}

// podName is "benchpod-<short id>", "" when unknown.
func podName(shortID string) string {
	if shortID == "" {
		return ""
	}
	return "benchpod-" + shortID
}

// gatewareOf is the newest gateware version among the gw slots (0 when none).
func gatewareOf(slots []serialconsole.BlobSlot) int {
	v := 0
	for _, s := range slots {
		if strings.HasPrefix(s.Name, "gw") && s.Version > v {
			v = s.Version
		}
	}
	return v
}

// blobsEvent is the report of an install-blobs run that installed something.
func blobsEvent(ctx context.Context, t blobTarget, before []serialconsole.BlobSlot, installed []blobEntry) fwEvent {
	pub, short := t.Identity(ctx)
	ver := strings.TrimPrefix(t.Release(ctx), "v")
	ev := fwEvent{PublicKey: pub, Name: podName(short), Kind: "blobs", OldVersion: ver, NewVersion: ver,
		OldGateware: gatewareOf(before), Method: t.Method()}
	for _, e := range installed {
		ev.Blobs = append(ev.Blobs, fwBlobVer{Name: e.Name, Version: e.Version})
		if strings.HasPrefix(e.Name, "gw") && e.Version > ev.NewGateware {
			ev.NewGateware = e.Version
		}
	}
	if ev.NewGateware == 0 {
		ev.NewGateware = ev.OldGateware
	}
	return ev
}

// firmwareEvent is the report of a flash-self: the pod before (when it was running) and after
// (when it came back); tag is the release flashed ("v3.9.0", "" for a local file).
func firmwareEvent(before, after podVersions, tag string, installed []blobEntry) fwEvent {
	ev := fwEvent{PublicKey: after.PublicKey, Name: podName(after.ShortID), Kind: "firmware", Method: "dfu",
		OldVersion: before.Version, NewVersion: after.Version, OldGateware: before.Gateware, NewGateware: after.Gateware}
	if ev.PublicKey == "" {
		ev.PublicKey, ev.Name = before.PublicKey, podName(before.ShortID)
	}
	if ev.NewVersion == "" {
		ev.NewVersion = strings.TrimPrefix(tag, "v")
	}
	for _, e := range installed {
		ev.Blobs = append(ev.Blobs, fwBlobVer{Name: e.Name, Version: e.Version})
	}
	return ev
}

// runningPodVersions reads the pod's identity and versions over its USB console, when one
// answers quickly (zero values otherwise).
func (g *globalFlags) runningPodVersions() podVersions {
	console, _, err := g.openBenchpodSerial(g.serialDevice(), 2*time.Second)
	if err != nil {
		return podVersions{}
	}
	defer console.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return consoleVersions(ctx, console)
}
