package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serialconsole"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── which pod a run uses ────────────────────────────────────────────────────
//
// --pod (a name or any connection), else the default every command uses (--connection,
// BENCHPOD_CONNECTION, the saved set-connection target), else the pods on this LAN and USB
// network link: the one there is, or with several the first one nobody holds.

// podCandidate is a pod found on the LAN.
type podCandidate struct {
	Name string // benchpod-a1b2c3
	Addr string // host:port
}

// podFreeFunc says whether a candidate is free (online and not leased), and who holds it if not.
type podFreeFunc func(podCandidate) (free bool, holder string, err error)

// choosePodFrom picks the pod of a run among the discovered ones.
func choosePodFrom(cands []podCandidate, free podFreeFunc) (podCandidate, string, error) {
	switch len(cands) {
	case 0:
		return podCandidate{}, "", errors.New("no pod found: none was saved as the default and none answered on the LAN or " +
			"a USB network link. Pass --pod <name|address|usb|embeddedci:name> (or set BENCHPOD_CONNECTION, " +
			"or save one with `benchpod set-connection`)")
	case 1:
		return cands[0], "the only pod found", nil
	}
	var held []string
	for _, c := range cands {
		ok, holder, err := free(c)
		if err != nil {
			held = append(held, fmt.Sprintf("%s (%v)", c.label(), err))
			continue
		}
		if ok {
			return c, fmt.Sprintf("%d pods found; %s is free", len(cands), c.label()), nil
		}
		held = append(held, fmt.Sprintf("%s (in use by %s)", c.label(), firstNonEmptyStr(holder, "another session")))
	}
	return podCandidate{}, "", &ciBusyError{msg: "every pod found is in use or unreachable: " + strings.Join(held, ", ") +
		". Pass --pod to pick one (and --wait to wait for it)"}
}

func (c podCandidate) label() string { return firstNonEmptyStr(c.Name, c.Addr) }

// ciPodTarget is the chosen pod.
type ciPodTarget struct {
	Raw  string   // the connection as given ("benchpod-a1b2c3", "usb", "192.168.1.5")
	Spec ConnSpec // resolved: network (USB already turned into its network link) or cloud
	Name string   // the pod's name when known
	Why  string   // how it was chosen, for the log
}

// discoverForCI lists the pods on the LAN and USB network links. A variable so tests can replace it.
var discoverForCI = func() ([]podCandidate, error) {
	pods, err := discoverPods(3 * time.Second)
	if err != nil {
		return nil, err
	}
	var out []podCandidate
	for _, p := range pods {
		if p.addr == "" {
			continue
		}
		name := ""
		if short := serialconsole.NormalizePodName(unescapeDNSSD(p.instance)); short != "" {
			name = "benchpod-" + short
		}
		out = append(out, podCandidate{Name: name, Addr: p.addr})
	}
	return out, nil
}

// podIsFree asks a candidate's status whether a lease is held.
func podIsFree(c podCandidate) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cl := &tcpclient.Client{Addr: c.Addr, DialTimeout: 3 * time.Second, NoLeaseJoin: true}
	st, _, err := readPodStatus(ctx, &networkPod{client: cl})
	if err != nil {
		return false, "", err
	}
	held, holder := st.held()
	return !held, holder, nil
}

// resolveCIPod picks and resolves the pod of a run.
func resolveCIPod(g *globalFlags, podFlag string) (*ciPodTarget, error) {
	t := &ciPodTarget{Raw: strings.TrimSpace(podFlag), Why: "--pod"}
	if t.Raw == "" {
		raw, err := g.rawConnection()
		if err != nil {
			return nil, err
		}
		t.Raw, t.Why = raw, "the default connection"
		if strings.TrimSpace(g.connection) != "" {
			t.Why = "--connection / BENCHPOD_CONNECTION"
		}
	}
	if t.Raw == "" {
		cands, err := discoverForCI()
		if err != nil && len(cands) == 0 {
			return nil, fmt.Errorf("find a pod on the LAN: %w; pass --pod", err)
		}
		c, why, err := choosePodFrom(cands, podIsFree)
		if err != nil {
			return nil, err
		}
		t.Raw, t.Name, t.Why = c.Addr, c.Name, why
	}
	spec, err := parseTarget(t.Raw)
	if err != nil {
		return nil, err
	}
	switch {
	case spec.IsCloud():
		t.Name = spec.Name
	case spec.Pod != "":
		t.Name = "benchpod-" + spec.Pod
	}
	if short := podShortName(t.Raw); short != "" {
		t.Name = "benchpod-" + short
	}
	if spec.IsSerial() {
		gc := *g
		gc.connection = t.Raw
		spec, err = gc.networkTarget("benchpod ci")
		if err != nil {
			return nil, err
		}
		g.usbAddr = gc.usbAddr
	}
	t.Spec = spec
	return t, nil
}
