package tcpclient

import (
	"strings"
	"sync/atomic"
)

// ── tagging commands with the run's pod lease ───────────────────────────────
//
// The pod keeps one lease in RAM (benchpod ci takes it for a whole run). While it is held, only
// its owners may drive the pod. Any JSON command that carries "lease":"<id>" with the held id is
// an owner's, on any connection, so while a lease is set here every command this process sends
// carries it. Tagging never extends the lease: the runner renews it with lease_renew.

// LeaseJoin is the lease the process holds.
type LeaseJoin struct {
	ID      string // "L-<uuid>"
	Holder  string // who holds it, for others' busy messages ("benchpod ci: alice")
	Purpose string // what for (the suite)
	TTL     int    // seconds
}

var leaseJoin atomic.Pointer[LeaseJoin]

// SetLeaseJoin makes every command carry j's id (nil stops it).
func SetLeaseJoin(j *LeaseJoin) { leaseJoin.Store(j) }

// LeaseAcquireRequest is the lease_acquire command for j.
func LeaseAcquireRequest(j *LeaseJoin) map[string]any {
	return map[string]any{"cmd": "lease_acquire", "id": j.ID, "holder": j.Holder, "purpose": j.Purpose, "ttl_s": j.TTL}
}

// IsUnknownCommand says the pod does not know the command it was sent (older firmware).
func IsUnknownCommand(err error) bool {
	pe, ok := AsPodError(err)
	if !ok {
		return false
	}
	r := strings.ToLower(strings.TrimSpace(pe.Reason))
	return pe.Code == "BP0002" || strings.HasPrefix(r, "unknown cmd") || strings.HasPrefix(r, "unknown command")
}

func (c *Client) tagLease(req map[string]any) map[string]any { return tagLease(req, c.NoLeaseJoin) }

// tagLease is req with "lease":<id> added while a lease is set (a copy; req is not changed). A
// request that names a lease already, or skip, is left as it is.
func tagLease(req map[string]any, skip bool) map[string]any {
	j := leaseJoin.Load()
	if j == nil || skip {
		return req
	}
	if _, has := req["lease"]; has {
		return req
	}
	out := make(map[string]any, len(req)+1)
	for k, v := range req {
		out[k] = v
	}
	out["lease"] = j.ID
	return out
}
