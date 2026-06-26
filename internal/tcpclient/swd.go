package tcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"
)

// TargetPower enables (on) or disables a target power eFuse over the TCP/JSON
// API (firmware cmd "target_power"). efuse is 1 (internal 5V) or 2 (external).
// It returns the firmware's error when the request is rejected.
func (c *Client) TargetPower(ctx context.Context, efuse int, on bool) error {
	state := 0
	if on {
		state = 1
	}
	_, err := c.Command(ctx, map[string]any{"cmd": "target_power", "efuse": efuse, "state": state})
	return err
}

// startRawMode performs the "arm the probe, then flip the connection to the
// length-framed CMSIS-DAP protocol" handshake used by DAPStart (cmd "dap_start").
// The pod acks {"status":"ok","data":"dap ready"} while still in JSON mode and
// then speaks framed CMSIS-DAP on the same socket, so — unlike Command/Samples —
// this does NOT use a bufio.Scanner (which could read past the ack newline and
// swallow probe bytes): it reads exactly the one ack line off the wire, then
// returns the untouched connection for the caller to bridge to OpenOCD. The
// caller owns closing it; closing returns the pod to a safe JSON state.
func (c *Client) startRawMode(ctx context.Context, cmd string, swclk, swdio int, nreset *int) (net.Conn, error) {
	addr := strings.TrimSpace(c.Addr)
	if addr == "" {
		return nil, fmt.Errorf("bench pod address is empty; run `benchpod set-connection <addr>` first")
	}

	budget := c.DialTimeout
	if budget <= 0 {
		budget = DefaultDialTimeout
	}
	conn, err := dialWithRetry(ctx, "tcp", addr, budget, dialAttemptTimeout, dialRetryBackoff)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	req := map[string]any{"cmd": cmd, "swclk": swclk, "swdio": swdio}
	if nreset != nil {
		req["nreset"] = *nreset
	}
	line, err := json.Marshal(req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send request: %w", err)
	}

	ackLine, err := readLine(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read %s ack: %w", cmd, err)
	}
	var r reply
	if err := json.Unmarshal(ackLine, &r); err != nil {
		conn.Close()
		return nil, fmt.Errorf("parse %s ack: %w", cmd, err)
	}
	switch r.Status {
	case "ok":
		// The connection is now in its raw protocol for the whole flash session,
		// which can outlast the command deadline — clear it so the bridge manages
		// the connection's lifetime itself.
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	case "error":
		conn.Close()
		return nil, fmt.Errorf("%s", firmwareMessage(r.Message))
	default:
		conn.Close()
		return nil, fmt.Errorf("unexpected response status %q", r.Status)
	}
}

// readLine reads a single newline-terminated line from conn one byte at a time,
// so no bytes past the newline are consumed (any buffered reader would swallow
// the framed CMSIS-DAP stream that follows the ack). The trailing newline is
// stripped from the returned slice.
func readLine(conn net.Conn) ([]byte, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		n, err := conn.Read(b)
		if n > 0 {
			if b[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, b[0])
		}
		if err != nil {
			return nil, err
		}
	}
}
