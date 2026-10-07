package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The pod's device identity over the console (firmware src/device_identity.h):
//
//	identity                     -> identity <short id> <public key> | identity unknown <why>
//	identity-wipe <id|unknown>   -> identity-wipe ok <new short id> | identity-wipe error <why>
//
// The short id is the hex of the pod's name "benchpod-a1b2c3" (the first three bytes of its
// Ed25519 public key). identity-wipe erases the key and makes a new one; it runs on the USB console
// only and needs the current short id (or "unknown" when the pod has none) as confirmation.

// ErrIdentityUnsupported is older firmware's answer to identity or identity-wipe.
var ErrIdentityUnsupported = errors.New("the firmware does not know this command")

// IdentityState is what the pod reports about its device key.
type IdentityState struct {
	ShortID   string // "a1b2c3"; "" when the pod has no identity
	PublicKey string // base64url Ed25519 public key; "" when none
	Problem   string // why there is no identity (only when ShortID is "")
}

// Known reports whether the pod has a working identity.
func (s IdentityState) Known() bool { return s.ShortID != "" }

// Name is the pod's own name for itself ("benchpod-a1b2c3"), or "" without an identity.
func (s IdentityState) Name() string {
	if s.ShortID == "" {
		return ""
	}
	return "benchpod-" + s.ShortID
}

// ConfirmToken is the token identity-wipe needs for this state.
func (s IdentityState) ConfirmToken() string {
	if s.ShortID == "" {
		return "unknown"
	}
	return s.ShortID
}

// IdentityError is the pod refusing identity-wipe ("identity-wipe error <why>").
type IdentityError struct{ Reason string }

func (e *IdentityError) Error() string { return "identity-wipe: " + e.Reason }

// Identity asks the pod for its device identity.
func (c *Console) Identity(ctx context.Context) (IdentityState, error) {
	const line = "identity"
	complete := func(acc string) bool {
		_, _, err := parseConsoleReply(acc, "identity", line)
		return !errors.Is(err, errNoConsoleReply)
	}
	out, err := c.sendCommandUntil(ctx, line, line, "", complete)
	reply, _, perr := parseConsoleReply(out, "identity", line)
	if perr != nil && !errors.Is(perr, errNoConsoleReply) {
		return IdentityState{}, perr
	}
	if errors.Is(perr, errNoConsoleReply) {
		if err != nil {
			return IdentityState{}, fmt.Errorf("identity: %w", err)
		}
		return IdentityState{}, fmt.Errorf("identity: no reply from the pod (got: %q)", tail([]byte(out), 200))
	}
	return parseIdentityState(reply)
}

func parseIdentityState(reply string) (IdentityState, error) {
	f := strings.Fields(reply)
	if len(f) == 0 {
		return IdentityState{}, errors.New("identity: empty reply from the pod")
	}
	if f[0] == "unknown" {
		why := strings.TrimSpace(strings.TrimPrefix(reply, "unknown"))
		if why == "" {
			why = "no identity"
		}
		return IdentityState{Problem: why}, nil
	}
	if len(f) < 2 || len(f[0]) != 6 {
		return IdentityState{}, fmt.Errorf("identity: unexpected reply %q", reply)
	}
	return IdentityState{ShortID: f[0], PublicKey: f[1]}, nil
}

// IdentityWipe erases the pod's device key and makes a new one, confirming with token (the
// current short id, or "unknown"). It returns the new short id.
func (c *Console) IdentityWipe(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("identity-wipe: bad confirmation token %q", token)
	}
	line := "identity-wipe " + token
	complete := func(acc string) bool {
		_, _, err := parseConsoleReply(acc, "identity-wipe", line)
		return !errors.Is(err, errNoConsoleReply)
	}
	out, err := c.sendCommandUntil(ctx, line, line, "", complete)
	reply, refused, perr := parseConsoleReply(out, "identity-wipe", line)
	switch {
	case perr == nil && refused:
		return "", &IdentityError{Reason: reply}
	case perr == nil:
		f := strings.Fields(reply)
		if len(f) < 2 || f[0] != "ok" {
			return "", fmt.Errorf("identity-wipe: unexpected reply %q", reply)
		}
		return f[1], nil
	case !errors.Is(perr, errNoConsoleReply):
		return "", perr
	case err != nil:
		return "", fmt.Errorf("identity-wipe: %w", err)
	default:
		return "", fmt.Errorf("identity-wipe: no reply from the pod (got: %q)", tail([]byte(out), 200))
	}
}

var errNoConsoleReply = errors.New("no reply yet")

// parseConsoleReply finds "<cmd> <reply>" in raw console output, skipping the line editor's echo
// of line (the first exact copy) and interleaved log lines; the last reply line wins. refused is
// true for "<cmd> error <why>", with reply then holding the reason.
func parseConsoleReply(raw, cmd, line string) (reply string, refused bool, err error) {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	echoSeen := false
	found := false
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(ln, "> \t\x08"))
		if ln == "" {
			continue
		}
		if strings.Contains(ln, "unknown command") {
			return "", false, ErrIdentityUnsupported
		}
		if !echoSeen && ln == line {
			echoSeen = true
			continue
		}
		if strings.HasPrefix(ln, cmd+" ") {
			reply, found = strings.TrimSpace(ln[len(cmd)+1:]), true
		}
	}
	if !found {
		return "", false, errNoConsoleReply
	}
	if f := strings.Fields(reply); len(f) > 0 && f[0] == "error" {
		why := strings.TrimSpace(strings.TrimPrefix(reply, "error"))
		if why == "" {
			why = "refused"
		}
		return why, true, nil
	}
	return reply, false, nil
}
