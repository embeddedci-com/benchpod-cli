// Package tcpclient is a thin client for the bench pod firmware's TCP/JSON API.
//
// The firmware speaks newline-delimited JSON over a TCP socket (default port
// 8080, or 8443 with TLS: tls.go), one client at a time. Each request is a flat JSON object
// ({"cmd":"<command>", ...top-level params...}) and each response line is a JSON
// object of the form {"status":"ok","data":<value>} or
// {"status":"error","message":"..."}. Commands that return large arrays
// (capture/stream/measure/test) chunk their data across multiple lines: the
// first packet uses "status":"ok", later packets use "status":"chunk", and every
// packet carries a "more" boolean — the client reads until "more":false.
//
// Samples come as "b64" (unpadded base64url of little-endian uint16), firmware 4.0's default,
// which reads back several times faster than the decimal "data" array; a request with
// "enc":"dec" gets "data", so both are decoded.
package tcpclient

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// DefaultDialTimeout is the default total connect budget (across retries) used
// when Client.DialTimeout is zero.
const DefaultDialTimeout = 10 * time.Second

// Dial-retry tuning. The single-client ESP32-AT server is often slow/flaky to
// accept a fresh connection right after the previous one tears down, so a lone
// connect() waits out the kernel's sparse SYN-retransmit (RTO) schedule (~1s,
// ~3s). We instead re-probe on a much finer cadence to catch the accept-ready
// window sooner. dialAttemptTimeout stays well above a healthy LAN connect
// (<50ms) so we never abandon a connect that is about to complete.
const (
	dialAttemptTimeout = 500 * time.Millisecond
	dialRetryBackoff   = 100 * time.Millisecond
)

// Client issues commands to a bench pod firmware over TCP. Each call opens a
// short-lived connection (the firmware serves a single client at a time) and
// closes it once the response is fully read.
type Client struct {
	Addr        string        // host:port; callers should port-normalise first
	DialTimeout time.Duration // total connect budget across retries; defaults to DefaultDialTimeout when zero
	TLS         TLSMode       // TLSDefault: DefaultTLSMode (tls.go)
	// NoLeaseJoin: this client's commands do not carry the lease set with SetLeaseJoin (the lease
	// commands themselves, and read-only probes of other pods).
	NoLeaseJoin bool

	lastTLS bool // the last connection used TLS
}

// reply is one decoded response line from the firmware.
type reply struct {
	Status  string          `json:"status"` // "ok" | "error" | "chunk"
	Data    json.RawMessage `json:"data"`
	B64     *string         `json:"b64"` // the samples (the default); Data with "enc":"dec"
	Message string          `json:"message"`
	More    bool            `json:"more"`
	Code    string          `json:"code"` // the stable error code of an error reply (newer firmware)
	Args    map[string]any  `json:"args"`
}

// Command sends req as a single JSON line and reads exactly one response line.
// On "status":"ok" it returns the raw data field; on "status":"error" it returns
// an error carrying the firmware's message. It is used by commands whose result
// fits in a single packet (ping, status, generate, la).
func (c *Client) Command(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	conn, reader, err := c.dialAndSend(ctx, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchContext(ctx, conn)()

	r, err := readReply(reader)
	if err != nil {
		return nil, readErr(ctx, err)
	}
	switch r.Status {
	case "ok":
		if err := checkReply(req, r.Data); err != nil {
			return nil, err
		}
		return r.Data, nil
	case "error":
		return nil, replyError(r)
	default:
		return nil, fmt.Errorf("unexpected response status %q", r.Status)
	}
}

// Samples sends req and reassembles a (possibly chunked) sample array response
// into a single []int, reading packets until one reports "more":false. It is
// used by capture, stream, measure, and test. Samples are stored as []int rather
// than []byte to avoid encoding/json's base64 special-casing of byte slices.
func (c *Client) Samples(ctx context.Context, req map[string]any) ([]int, error) {
	conn, reader, err := c.dialAndSend(ctx, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchContext(ctx, conn)()

	var out []int
	for {
		r, err := readReply(reader)
		if err != nil {
			return nil, readErr(ctx, err)
		}
		switch r.Status {
		case "ok", "chunk":
			if r.B64 != nil {
				chunk, err := decodeB64Samples(*r.B64)
				if err != nil {
					return nil, fmt.Errorf("parse sample data: %w", err)
				}
				out = append(out, chunk...)
			} else if len(r.Data) > 0 && string(r.Data) != "null" {
				var chunk []int
				if err := json.Unmarshal(r.Data, &chunk); err != nil {
					return nil, fmt.Errorf("parse sample data: %w", err)
				}
				out = append(out, chunk...)
			}
			if !r.More {
				return out, nil
			}
		case "error":
			return nil, replyError(r)
		default:
			return nil, fmt.Errorf("unexpected response status %q", r.Status)
		}
	}
}

// decodeB64Samples decodes one "b64" chunk: base64url (padding optional) of little-endian
// uint16 samples.
func decodeB64Samples(s string) ([]int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, err
	}
	if len(raw)%2 != 0 {
		return nil, fmt.Errorf("odd byte count %d in b64 samples", len(raw))
	}
	out := make([]int, len(raw)/2)
	for i := range out {
		out[i] = int(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return out, nil
}

// Chunks sends req and returns every reply line of a chunked reply as it came, reading until a
// line has "more" false (or no "more"). It is for chunked replies whose fields are not a sample
// array (power_profile's t_us/current_ua/stats). An error line is the pod's refusal.
func (c *Client) Chunks(ctx context.Context, req map[string]any) ([]json.RawMessage, error) {
	conn, reader, err := c.dialAndSend(ctx, req)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	defer watchContext(ctx, conn)()

	var out []json.RawMessage
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil && len(strings.TrimSpace(string(line))) == 0 {
			return nil, readErr(ctx, err)
		}
		var head reply
		if jerr := json.Unmarshal(line, &head); jerr != nil {
			return nil, fmt.Errorf("parse reply: %w", jerr)
		}
		switch head.Status {
		case "error":
			return nil, replyError(&head)
		case "ok", "chunk":
		default:
			return nil, fmt.Errorf("unexpected response status %q", head.Status)
		}
		out = append(out, json.RawMessage(append([]byte(nil), line...)))
		if !head.More {
			return out, nil
		}
		if err != nil {
			return nil, readErr(ctx, err)
		}
	}
}

// dialAndSend opens the TCP connection, applies the context deadline, writes req
// as one newline-terminated JSON line, and returns the connection plus a buffered
// reader positioned to read response lines. The caller owns closing the connection.
func (c *Client) dialAndSend(ctx context.Context, req map[string]any) (net.Conn, *bufio.Reader, error) {
	addr := strings.TrimSpace(c.Addr)
	if addr == "" {
		return nil, nil, fmt.Errorf("pod address is empty; run `benchpod set-connection <addr>` first")
	}

	if err := c.probePodAPI(ctx, req); err != nil {
		return nil, nil, err
	}
	budget := c.DialTimeout
	if budget <= 0 {
		budget = DefaultDialTimeout
	}
	conn, err := c.dial(ctx, budget)
	if err != nil {
		return nil, nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	line, err := json.Marshal(c.tagRequest(req))
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("send request: %w", err)
	}

	return conn, bufio.NewReader(conn), nil
}

// dialWithRetry repeatedly attempts to connect to addr, giving each attempt at
// most perAttempt, until a connection succeeds, the total budget is spent, or
// ctx is cancelled. Re-probing on a fine cadence (perAttempt + backoff) catches
// the firmware's accept-ready window sooner than the kernel's sparse SYN-retransmit
// schedule. It retries on any dial error (a not-yet-listening server yields
// "connection refused"; dropped SYNs yield a per-attempt timeout); errors that
// occur after a connection is established are the caller's concern and are not
// retried here.
func dialWithRetry(ctx context.Context, network, addr string, total, perAttempt, backoff time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(total)
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, perAttempt)
		conn, err := (&net.Dialer{}).DialContext(attemptCtx, network, addr)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err

		// Stop if the caller cancelled or we've spent the whole budget.
		if ctx.Err() != nil {
			return nil, &dialError{addr: addr, err: ctx.Err()}
		}
		if !time.Now().Add(backoff).Before(deadline) {
			return nil, &dialError{addr: addr, err: lastErr}
		}

		select {
		case <-ctx.Done():
			return nil, &dialError{addr: addr, err: ctx.Err()}
		case <-time.After(backoff):
		}
	}
}

// watchContext unblocks a blocked Read/Write on conn the moment ctx is done.
// A net.Conn honours its SetDeadline timer but not context cancellation, so a
// read that is waiting for a reply that never comes would otherwise block until
// the absolute deadline even after the user hits Ctrl+C. Pushing the deadline
// into the past makes the in-flight I/O return immediately; the caller then
// reports ctx.Err() (see readErr). The returned func must be called (defer) once
// I/O is done to release the watcher goroutine.
func watchContext(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()
	return func() { close(done) }
}

// readErr prefers the context's cancellation/deadline cause over the raw socket
// error, so an interrupted command reports "context canceled" rather than a
// confusing "i/o timeout" produced by the deadline watchContext set.
func readErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// readReply reads the next newline-terminated line and decodes it into a reply.
// It uses bufio.Reader.ReadBytes rather than bufio.Scanner: a scanner's default
// token cap is 64 KB and would fail with bufio.ErrTooLong on a larger reply line
// (chunked capture/measure packets can exceed that), while ReadBytes grows the
// buffer to whatever the line needs.
func readReply(reader *bufio.Reader) (*reply, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		// EOF (with or without a partial, newline-less line buffered) means the pod
		// closed before sending a complete response line.
		if errors.Is(err, io.EOF) {
			return nil, errors.New("pod closed connection without a complete response")
		}
		return nil, fmt.Errorf("read response: %w", err)
	}
	var r reply
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &r, nil
}
