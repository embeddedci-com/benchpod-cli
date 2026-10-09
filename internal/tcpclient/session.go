package tcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Session is one TCP connection that carries many request/reply round trips.
// The firmware reassembles lines per connection and dispatches on each '\n', so
// a long run of small commands (an SPI flash write is thousands of them) does not
// need a fresh connect per command the way Client.Command does. Commands are
// sequential: one request, then exactly one reply line.
//
// After a Command fails on I/O (cancel, timeout, a dropped link) the connection
// may still owe the pod's reply, so the Session is marked broken and every later
// Command fails at once; open a new one (or use Client.Command) for cleanup.
type Session struct {
	conn   net.Conn
	reader *bufio.Reader

	mu     sync.Mutex
	broken error
}

// Open dials the pod and returns a Session. The caller must Close it.
func (c *Client) Open(ctx context.Context) (*Session, error) {
	addr := strings.TrimSpace(c.Addr)
	if addr == "" {
		return nil, fmt.Errorf("pod address is empty; run `benchpod set-connection <addr>` first")
	}
	budget := c.DialTimeout
	if budget <= 0 {
		budget = DefaultDialTimeout
	}
	conn, err := dialWithRetry(ctx, "tcp", addr, budget, dialAttemptTimeout, dialRetryBackoff)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &Session{conn: conn, reader: bufio.NewReader(conn)}, nil
}

// Command sends req and reads its one reply line, like Client.Command, on the
// session's connection. ctx bounds this round trip only.
func (s *Session) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken != nil {
		return nil, fmt.Errorf("connection unusable after an earlier error: %w", s.broken)
	}

	deadline := time.Time{}
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	_ = s.conn.SetDeadline(deadline)
	defer watchContext(ctx, s.conn)()

	line, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	line = append(line, '\n')
	if _, err := s.conn.Write(line); err != nil {
		s.broken = sessionErr(ctx, err)
		return nil, fmt.Errorf("send request: %w", s.broken)
	}
	r, err := readReply(s.reader)
	if err != nil {
		s.broken = sessionErr(ctx, err)
		return nil, s.broken
	}
	switch r.Status {
	case "ok":
		return r.Data, nil
	case "error":
		return nil, podError(r.Message)
	default:
		return nil, fmt.Errorf("unexpected response status %q", r.Status)
	}
}

// sessionErr is readErr plus the case where the socket deadline (set from ctx)
// fires a moment before ctx itself reports done.
func sessionErr(ctx context.Context, err error) error {
	if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) && ctx.Err() == nil {
		return context.DeadlineExceeded
	}
	return readErr(ctx, err)
}

// Broken reports whether an earlier I/O failure made the session unusable.
func (s *Session) Broken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken != nil
}

// Close closes the connection.
func (s *Session) Close() error { return s.conn.Close() }
