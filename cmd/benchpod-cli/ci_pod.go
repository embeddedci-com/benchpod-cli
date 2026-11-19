package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/ci"
	"github.com/embeddedci-com/benchpod-cli/internal/cloudpod"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── the pod a `benchpod ci` run drives ──────────────────────────────────────
//
// Native steps talk to the pod through ciPod, so the runner is the same over the LAN, the USB
// network link and embeddedci.com, and tests drive it with a fake. tracedPod records every call
// into the run's trace.

// ciPod is the pod of a run.
type ciPod interface {
	// Command sends one single-reply JSON command.
	Command(ctx context.Context, req map[string]any) (json.RawMessage, error)
	// Chunks sends a command with a chunked reply and returns its lines (direct links only).
	Chunks(ctx context.Context, req map[string]any) ([]json.RawMessage, error)
	// OpenUART starts uart_proxy_start and returns the byte stream (direct links only).
	OpenUART(ctx context.Context, req map[string]any) (io.ReadWriteCloser, error)
	// Flash flashes the DUT.
	Flash(ctx context.Context, fw ciFlash) error
	// Transport is lan, usb_net or cloud.
	Transport() string
}

// ciFlash is what a flash step flashes, with the YAML's overrides of the wiring profile.
type ciFlash struct {
	File                         string // a local image, or
	BuildID, Repo, Ref, Artifact string // an uploaded build
	Overrides                    ci.FlashOverrides
	Timeout                      time.Duration
}

// errNeedsDirectLink is a native step that needs a stream the cloud command channel does not carry.
var errNeedsDirectLink = errors.New("this step needs a direct link to the pod (its network address or `usb`); " +
	"over embeddedci.com only single-reply commands work: run the UART and power-profile checks from a pytest step, " +
	"which reaches them through the cloud tunnel")

// networkPod is a pod on its LAN or USB network address.
type networkPod struct {
	g      *globalFlags // the run's flags, with connection set to this pod
	client *tcpclient.Client
	usbNet bool
}

func (p *networkPod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	return p.client.Command(ctx, req)
}

func (p *networkPod) Chunks(ctx context.Context, req map[string]any) ([]json.RawMessage, error) {
	return p.client.Chunks(ctx, req)
}

func (p *networkPod) OpenUART(ctx context.Context, req map[string]any) (io.ReadWriteCloser, error) {
	return p.client.StartRaw(ctx, req)
}

func (p *networkPod) Flash(ctx context.Context, fw ciFlash) error { return flashForCI(p.g, fw) }

func (p *networkPod) Transport() string {
	if p.usbNet {
		return "usb_net"
	}
	return "lan"
}

// cloudPod is a pod on embeddedci.com.
type cloudPod struct {
	g      *globalFlags
	client *cloudpod.Client
}

func (p *cloudPod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	return p.client.Command(ctx, req)
}

func (p *cloudPod) Chunks(context.Context, map[string]any) ([]json.RawMessage, error) {
	return nil, errNeedsDirectLink
}

func (p *cloudPod) OpenUART(context.Context, map[string]any) (io.ReadWriteCloser, error) {
	return nil, errNeedsDirectLink
}

func (p *cloudPod) Flash(ctx context.Context, fw ciFlash) error {
	if fw.File != "" {
		return fmt.Errorf("over embeddedci.com a flash step flashes a build uploaded there (firmware.build_id, or repo/ref): " +
			"a local file needs a direct link to the pod (pass --pod with its address or `usb`)")
	}
	return flashForCI(p.g, fw)
}

func (p *cloudPod) Transport() string { return "cloud" }

// flashForCI runs `benchpod flash` for a run: the pod's wiring profile (the YAML's `wiring`)
// fills in the SWD settings, the YAML's firmware.flash overrides them.
func flashForCI(g *globalFlags, fw ciFlash) error {
	gc := *g
	gc.timeout = fw.Timeout
	f := &flashFlags{file: fw.File, build: fw.BuildID, repo: fw.Repo, ref: fw.Ref, artifact: fw.Artifact}
	if (f.repo != "" || f.ref != "") && f.build == "" {
		f.build = "latest"
	}
	o := fw.Overrides
	f.target, f.swclk, f.swdio, f.loadAddr, f.openocdBin, f.targetPower = o.Target, o.SWCLK, o.SWDIO, o.LoadAddress, o.OpenOCD, o.TargetPower
	if o.NReset != nil {
		f.nreset, f.nresetSet = *o.NReset, true
	}
	if o.Verify != nil {
		f.noVerify = !*o.Verify
	}
	if o.Reset != nil {
		f.noReset = !*o.Reset
	}
	if o.ConnectUnderReset != nil {
		f.noConnectUnderReset = !*o.ConnectUnderReset
	}
	return runFlash(&gc, f)
}

// tracedPod records every call of the inner pod into rec.
type tracedPod struct {
	inner ciPod
	rec   *ci.Recorder
}

func (t *tracedPod) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	start := time.Now()
	resp, err := t.inner.Command(ctx, req)
	t.rec.Record("command", cmdName(req), req, resp, time.Since(start), err)
	return resp, err
}

func (t *tracedPod) Chunks(ctx context.Context, req map[string]any) ([]json.RawMessage, error) {
	start := time.Now()
	lines, err := t.inner.Chunks(ctx, req)
	var resp any
	if len(lines) > 0 {
		resp = lines[len(lines)-1]
	}
	n := len(lines)
	t.rec.RecordLine(ci.TraceLine{Op: "samples", Cmd: cmdName(req), Req: req, Resp: resp, N: &n}, time.Since(start), err)
	return lines, err
}

func (t *tracedPod) OpenUART(ctx context.Context, req map[string]any) (io.ReadWriteCloser, error) {
	start := time.Now()
	rw, err := t.inner.OpenUART(ctx, req)
	t.rec.RecordLine(ci.TraceLine{Op: "stream", Cmd: cmdName(req), Req: req}, time.Since(start), err)
	return rw, err
}

func (t *tracedPod) Flash(ctx context.Context, fw ciFlash) error {
	start := time.Now()
	err := t.inner.Flash(ctx, fw)
	req := map[string]any{"file": fw.File, "build": fw.BuildID, "repo": fw.Repo, "ref": fw.Ref}
	t.rec.RecordLine(ci.TraceLine{Op: "raw", Mode: "dap", Req: req}, time.Since(start), err)
	return err
}

func (t *tracedPod) Transport() string { return t.inner.Transport() }

func cmdName(req map[string]any) string {
	s, _ := req["cmd"].(string)
	return s
}

// ciPodStatus is the part of `status` the runner reads.
type ciPodStatus struct {
	Version     string `json:"version"`
	Board       string `json:"board"`
	BoardRev    string `json:"board_rev"`
	Gateware    int    `json:"gateware"`
	FeaturesRev int    `json:"features_rev"`
	LAVccioMV   int    `json:"la_vccio_mv"`
	Analog      *bool  `json:"analog"`
	Reset       string `json:"reset"`
	UptimeS     *int64 `json:"uptime_s"`
	UptimeMS    *int64 `json:"uptime_ms"`
	BootID      string `json:"boot_id"`
	Module      *struct {
		Present bool `json:"present"`
		Analog  bool `json:"analog"`
	} `json:"module"`
	Lease *struct {
		Held   bool   `json:"held"`
		Holder string `json:"holder"`
	} `json:"lease"`
	LocalLease *struct {
		Held    bool   `json:"held"`
		Holder  string `json:"holder"`
		Purpose string `json:"purpose"`
		LeftS   int    `json:"left_s"`
		Mine    bool   `json:"mine"`
	} `json:"local_lease"`
}

// boardName is the pod's board as run.json names it ("analog-v3", "digital-v3").
func (s ciPodStatus) boardName() string {
	kind := "analog"
	if s.Analog != nil && !*s.Analog {
		kind = "digital"
	}
	if s.Module != nil {
		kind = "digital"
	}
	rev := strings.TrimSpace(s.BoardRev)
	if rev == "" || rev == "unknown" {
		return firstNonEmptyStr(s.Board, kind)
	}
	return kind + "-" + rev
}

// uptimeMS is the pod's uptime when its status reports one (-1 when it does not).
func (s ciPodStatus) uptimeMS() int64 {
	switch {
	case s.UptimeMS != nil:
		return *s.UptimeMS
	case s.UptimeS != nil:
		return *s.UptimeS * 1000
	}
	return -1
}

// held says another session holds the pod: a cloud lease, or a local lease that is not ours.
func (s ciPodStatus) held() (bool, string) {
	if s.LocalLease != nil && s.LocalLease.Held && !s.LocalLease.Mine {
		return true, s.LocalLease.Holder
	}
	if s.Lease != nil && s.Lease.Held {
		return true, s.Lease.Holder
	}
	return false, ""
}

func readPodStatus(ctx context.Context, pod ciPod) (ciPodStatus, json.RawMessage, error) {
	raw, err := pod.Command(ctx, map[string]any{"cmd": "status", "caps": false})
	if err != nil {
		return ciPodStatus{}, nil, err
	}
	var st ciPodStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return ciPodStatus{}, raw, fmt.Errorf("status: %w", err)
	}
	return st, raw, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
