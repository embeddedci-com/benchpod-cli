package tcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// ── joining a pod lease ─────────────────────────────────────────────────────
//
// The pod keeps one lease in RAM (benchpod ci takes it for a whole run). While it is held, only
// the connections that sent lease_acquire with its id (its owners) may drive the pod. Client opens
// a fresh connection per command, so when a lease is set here every new connection sends
// lease_acquire with that id first and so becomes an owner; then the command goes on as usual.

// LeaseJoin is the lease every new connection joins.
type LeaseJoin struct {
	ID      string // "L-<uuid>"
	Holder  string // who holds it, for others' busy messages ("benchpod ci: alice")
	Purpose string // what for (the suite)
	TTL     int    // seconds; each join also renews it
}

var (
	leaseJoin        atomic.Pointer[LeaseJoin]
	leaseUnsupported atomic.Bool
)

// SetLeaseJoin makes every new connection join j (nil stops it). A pod that does not know
// lease_acquire (older firmware) is remembered, and later connections skip the join.
func SetLeaseJoin(j *LeaseJoin) {
	leaseJoin.Store(j)
	leaseUnsupported.Store(false)
}

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
	return pe.Code == "BP0002" || r == "unknown cmd" || strings.HasPrefix(r, "unknown cmd") || strings.HasPrefix(r, "unknown command")
}

// dial is dialConn, then the lease join when one is set.
func (c *Client) dial(ctx context.Context, budget time.Duration) (net.Conn, error) {
	conn, err := c.dialConn(ctx, budget)
	if err != nil {
		return nil, err
	}
	j := leaseJoin.Load()
	if j == nil || c.NoLeaseJoin || leaseUnsupported.Load() {
		return conn, nil
	}
	if err := joinLease(ctx, conn, j); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// joinLease sends lease_acquire on conn and reads its one reply line, byte by byte so nothing past
// it is consumed (a raw-mode command may follow).
func joinLease(ctx context.Context, conn net.Conn, j *LeaseJoin) error {
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})
	line, err := json.Marshal(LeaseAcquireRequest(j))
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("join the pod lease: send: %w", err)
	}
	ack, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("join the pod lease: %w", readErr(ctx, err))
	}
	var r reply
	if err := json.Unmarshal(ack, &r); err != nil {
		return fmt.Errorf("join the pod lease: parse reply: %w", err)
	}
	if r.Status == "error" {
		rerr := replyError(&r)
		if IsUnknownCommand(rerr) {
			leaseUnsupported.Store(true)
			return nil
		}
		return rerr
	}
	return nil
}
