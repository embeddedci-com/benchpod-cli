package tcpclient

import (
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

// IsUnknownCommand says the pod does not know the command it was sent (BP0002: older firmware).
func IsUnknownCommand(err error) bool { return HasCode(err, "BP0002") }

// tagRequest is req as this client sends it: with the client fields and, unless NoLeaseJoin, the
// lease (clientapi.go).
func (c *Client) tagRequest(req map[string]any) map[string]any { return tagRequest(req, c.NoLeaseJoin) }
