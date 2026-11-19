package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/embeddedci-com/benchpod-cli/internal/serverapi"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// ── the pod lease a run holds ───────────────────────────────────────────────
//
// `benchpod ci` takes the pod for the whole run: on the LAN and USB the pod's own lease in RAM
// (lease_acquire), through embeddedci.com the server's device lease. Every connection the CLI
// opens joins it (tcpclient.SetLeaseJoin), cloud commands carry it (X-Benchpod-Lease), and the
// child processes get its id in BENCHPOD_LEASE so the SDK joins the same lease.

const (
	ciLeaseTTL      = 300               // seconds
	ciLeaseInterval = 100 * time.Second // renew period, a third of the TTL
	ciLeasePoll     = 5 * time.Second   // how often --wait asks again
)

// ciLease is the lease a run holds.
type ciLease interface {
	ID() string
	// Supported is false when the pod (or server) has no lease: the run goes on without one.
	Supported() bool
	// Lost is why the lease was lost during the run, "" while it holds.
	Lost() string
	// MarkLost records a loss seen elsewhere (a command answered "lease lost").
	MarkLost(why string)
	Release(ctx context.Context)
}

// ciBusyError is the pod held by someone else (exit 4).
type ciBusyError struct{ msg string }

func (e *ciBusyError) Error() string { return e.msg }

// newLeaseID is "L-<uuid v4>".
func newLeaseID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return "L-" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// leaseHolder is "benchpod ci: <who>" (at most 40 characters): the GitHub actor in Actions, else
// the user, else the host.
func leaseHolder() string {
	who := strings.TrimSpace(os.Getenv("GITHUB_ACTOR"))
	if who == "" {
		if u, err := user.Current(); err == nil {
			who = u.Username
		}
	}
	if who == "" {
		who, _ = os.Hostname()
	}
	if i := strings.LastIndexAny(who, `\/`); i >= 0 { // DOMAIN\user
		who = who[i+1:]
	}
	return truncate("benchpod ci: "+firstNonEmptyStr(who, "unknown"), 40)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// leaseState is the shared part of both leases: the id, the renew loop and the loss.
type leaseState struct {
	id        string
	supported bool
	mu        sync.Mutex
	lost      string
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

func (l *leaseState) ID() string      { return l.id }
func (l *leaseState) Supported() bool { return l.supported }

func (l *leaseState) Lost() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

func (l *leaseState) MarkLost(why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost == "" {
		l.lost = why
	}
}

// loop calls renew every interval until stop; a renew that reports the lease gone marks it lost
// and ends the loop. Transient failures are retried until a whole TTL passed without a renew.
func (l *leaseState) loop(interval time.Duration, renew func() (gone string, err error)) {
	l.stop, l.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(l.done)
		lastOK := time.Now()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
			}
			gone, err := renew()
			switch {
			case gone != "":
				l.MarkLost(gone)
				return
			case err == nil:
				lastOK = time.Now()
			case time.Since(lastOK) > ciLeaseTTL*time.Second:
				l.MarkLost(fmt.Sprintf("the lease could not be renewed for %d s (%v)", ciLeaseTTL, err))
				return
			}
		}
	}()
}

func (l *leaseState) stopLoop() {
	l.once.Do(func() {
		if l.stop != nil {
			close(l.stop)
			<-l.done
		}
	})
}

// ── the pod's own lease (LAN, USB network) ──────────────────────────────────

type localLease struct {
	leaseState
	cmd  podCommander // a client that does not join (NoLeaseJoin)
	join *tcpclient.LeaseJoin
}

// acquireLocalLease takes the pod's lease, waiting up to wait for another holder to let go. A pod
// without lease_acquire gets one warning on warn and an unsupported lease.
func acquireLocalLease(ctx context.Context, cmd podCommander, join *tcpclient.LeaseJoin, wait, interval time.Duration, warn io.Writer) (*localLease, error) {
	l := &localLease{leaseState: leaseState{id: join.ID}, cmd: cmd, join: join}
	deadline := time.Now().Add(wait)
	warned := false
	for {
		_, err := cmd.Command(ctx, tcpclient.LeaseAcquireRequest(join))
		if err == nil {
			l.supported = true
			break
		}
		if tcpclient.IsUnknownCommand(err) {
			fmt.Fprintln(warn, "Warning: this pod's firmware has no lease (lease_acquire): the run goes on without one, so the web app or another session could use the pod meanwhile. Update the firmware with `benchpod flash-self`.")
			return l, nil
		}
		pe, ok := tcpclient.AsPodError(err)
		if !ok || !isLeaseBusy(pe) {
			return nil, fmt.Errorf("take the pod lease: %w", err)
		}
		if time.Now().After(deadline) {
			msg := "the pod is in use: " + busyText(pe)
			if wait > 0 {
				msg += fmt.Sprintf(" (waited %s)", wait.Round(time.Second))
			} else {
				msg += "; pass --wait 10m to wait for it"
			}
			return nil, &ciBusyError{msg: msg}
		}
		if !warned {
			fmt.Fprintf(warn, "The pod is busy (%s); waiting up to %s for it.\n", pe.Reason, wait.Round(time.Second))
			warned = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ciLeasePoll):
		}
	}
	tcpclient.SetLeaseJoin(join)
	l.loop(interval, func() (string, error) {
		rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := l.cmd.Command(rctx, map[string]any{"cmd": "lease_renew", "id": l.id, "ttl_s": l.join.TTL})
		if pe, ok := tcpclient.AsPodError(err); ok {
			return "the pod lease was lost (" + strings.TrimSpace(pe.Reason) + "): the pod rebooted, or someone stopped the run's lease", nil
		}
		return "", err
	})
	return l, nil
}

func (l *localLease) Release(ctx context.Context) {
	l.stopLoop()
	tcpclient.SetLeaseJoin(nil)
	if !l.supported {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, _ = l.cmd.Command(rctx, map[string]any{"cmd": "lease_release", "id": l.id})
}

// Lease refusals of the pod (benchpod-firmware errors/catalog.toml).
const (
	codeLeaseBusy    = "BP0025" // another id holds it; args holder, purpose, left_s
	codeLeaseLost    = "BP0026" // stopped from the web app; args by
	codeLeaseNotHeld = "BP0027" // renew after expiry
)

func isLeaseBusy(pe *tcpclient.PodError) bool {
	return pe.Code == codeLeaseBusy || pe.Kind() == tcpclient.Busy
}

// busyText is "<holder> holds this pod (<purpose>, N s left)" from a lease_busy refusal.
func busyText(pe *tcpclient.PodError) string {
	if holder, _ := pe.Args["holder"].(string); holder != "" {
		s := holder + " holds this pod"
		purpose, _ := pe.Args["purpose"].(string)
		left, hasLeft := pe.Args["left_s"].(float64)
		switch {
		case purpose != "" && hasLeft:
			s += fmt.Sprintf(" (%s, %.0f s left)", purpose, left)
		case purpose != "":
			s += " (" + purpose + ")"
		case hasLeft:
			s += fmt.Sprintf(" (%.0f s left)", left)
		}
		return s
	}
	return strings.TrimPrefix(strings.TrimSpace(pe.Reason), "busy: ")
}

// isLeaseLost is a command refused because the run's lease was stopped (BP0026) or expired (BP0027).
func isLeaseLost(pe *tcpclient.PodError) bool {
	r := strings.TrimSpace(pe.Reason)
	return pe.Code == codeLeaseLost || pe.Code == codeLeaseNotHeld || strings.HasPrefix(r, "lease lost")
}

// ── the server's lease (embeddedci.com) ─────────────────────────────────────

type cloudLease struct {
	leaseState
	api        *serverapi.Client
	credential string // the user's
	device     string // the pod's name on embeddedci.com
	req        serverapi.LeaseRequest

	sessMu  sync.Mutex
	session string
	expires time.Time
}

// sessionToken is a cloud session token, minted again when it is near its end.
func (l *cloudLease) sessionToken(ctx context.Context) (string, error) {
	l.sessMu.Lock()
	defer l.sessMu.Unlock()
	if l.session != "" && time.Until(l.expires) > 5*time.Minute {
		return l.session, nil
	}
	s, err := l.api.MintCloudSession(ctx, l.credential, 3600)
	if err != nil {
		return "", err
	}
	l.session = s.Token
	l.expires = time.Now().Add(55 * time.Minute)
	if s.ExpiresIn > 0 {
		l.expires = time.Now().Add(time.Duration(s.ExpiresIn) * time.Second)
	}
	return l.session, nil
}

// acquireCloudLease takes the server's lease on device, waiting up to wait.
func acquireCloudLease(ctx context.Context, api *serverapi.Client, credential, device, id string, wait, interval time.Duration, warn io.Writer) (*cloudLease, error) {
	l := &cloudLease{leaseState: leaseState{id: id}, api: api, credential: credential, device: device,
		req: serverapi.LeaseRequest{LeaseID: id, RunLabel: leaseHolder(), TTLSeconds: ciLeaseTTL}}
	deadline := time.Now().Add(wait)
	warned := false
	for {
		tok, err := l.sessionToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("take the pod lease on embeddedci.com: %w", err)
		}
		busy, err := api.CloudLease(ctx, tok, "", device, l.req)
		var apiErr *serverapi.APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusMethodNotAllowed || apiErr.Status == http.StatusNotImplemented ||
			apiErr.Status == http.StatusNotFound && apiErr.Message == "") {
			fmt.Fprintln(warn, "Warning: embeddedci.com has no device lease here: the run goes on without one.")
			return l, nil
		}
		if err != nil {
			return nil, fmt.Errorf("take the pod lease on embeddedci.com: %w", err)
		}
		if busy == nil {
			l.supported = true
			break
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("the pod is in use: %s holds %s on embeddedci.com", busy.Holder, device)
			if busy.ExpiresAt != "" {
				msg += " (its lease runs until " + busy.ExpiresAt + " unless renewed)"
			}
			if wait == 0 {
				msg += "; pass --wait 10m to wait for it"
			}
			return nil, &ciBusyError{msg: msg}
		}
		if !warned {
			fmt.Fprintf(warn, "The pod is busy (%s holds it); waiting up to %s for it.\n", busy.Holder, wait.Round(time.Second))
			warned = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(ciLeasePoll):
		}
	}
	l.loop(interval, func() (string, error) {
		rctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		tok, err := l.sessionToken(rctx)
		if err != nil {
			return "", err
		}
		busy, err := api.CloudLease(rctx, tok, "renew", device, l.req)
		var apiErr *serverapi.APIError
		switch {
		case busy != nil:
			return "the lease was lost on embeddedci.com: " + busy.Holder + " holds the pod now", nil
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusGone:
			return "the lease expired on embeddedci.com", nil
		}
		return "", err
	})
	return l, nil
}

func (l *cloudLease) Release(ctx context.Context) {
	l.stopLoop()
	if !l.supported {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if tok, err := l.sessionToken(rctx); err == nil {
		_, _ = l.api.CloudLease(rctx, tok, "release", l.device, l.req)
	}
}
