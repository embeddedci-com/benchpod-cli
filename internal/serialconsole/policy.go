package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Pod policies over the console (firmware docs/design/policy-commands.md):
//
//	lan-policy                          -> lan-policy <open|locked|off>
//	lan-policy <open|locked|off>        -> lan-policy <policy> | lan-policy error <why>
//	sig-policy                          -> sig-policy <audit|permissive|required> <keys>
//	sig-policy <audit|permissive|...>   -> sig-policy <policy> <keys> | sig-policy error <why>
//
// The USB console may set either policy to anything: it is the way back from the cloud's ratchet
// and from a LAN that is switched off. Older firmware answers "unknown command".

// ErrPolicyUnsupported is older firmware's answer to lan-policy or sig-policy.
var ErrPolicyUnsupported = errors.New("the firmware does not know this command")

// PolicyReply is the pod's answer to lan-policy or sig-policy.
type PolicyReply struct {
	Policy string
	Keys   int // sig-policy only: the public keys the firmware trusts (-1 when not reported)
}

// PolicyError is the pod refusing a value ("<cmd> error <why>").
type PolicyError struct {
	Cmd    string
	Reason string
}

func (e *PolicyError) Error() string { return e.Cmd + ": " + e.Reason }

// Policy runs the console command cmd ("lan-policy" or "sig-policy"), setting the policy to set
// when it is not empty, and returns what the pod reports afterwards.
func (c *Console) Policy(ctx context.Context, cmd, set string) (PolicyReply, error) {
	if cmd != "lan-policy" && cmd != "sig-policy" {
		return PolicyReply{}, fmt.Errorf("unknown policy command %q", cmd)
	}
	line := cmd
	if set = strings.TrimSpace(set); set != "" {
		if strings.ContainsAny(set, " \t\r\n") {
			return PolicyReply{}, fmt.Errorf("%s: bad value %q", cmd, set)
		}
		line += " " + set
	}
	complete := func(acc string) bool {
		_, err := parsePolicyReply(acc, cmd, line)
		return !errors.Is(err, errNoPolicyReply)
	}
	out, err := c.sendCommandUntil(ctx, line, line, "", complete)
	rep, perr := parsePolicyReply(out, cmd, line)
	if perr == nil || !errors.Is(perr, errNoPolicyReply) {
		return rep, perr
	}
	if err != nil {
		return PolicyReply{}, fmt.Errorf("%s: %w", cmd, err)
	}
	return PolicyReply{}, fmt.Errorf("%s: no reply from the pod (got: %q)", cmd, tail([]byte(out), 200))
}

var errNoPolicyReply = errors.New("no policy reply yet")

// parsePolicyReply finds the pod's reply to line in raw console output. The line editor echoes
// the command first, and for a set the echo ("lan-policy locked") reads exactly like the reply,
// so the first copy of the command line is skipped. Log lines interleave freely; the last reply
// line wins.
func parsePolicyReply(raw, cmd, line string) (PolicyReply, error) {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	echoSeen := false
	var reply string
	found := false
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(ln, "> \t\x08"))
		if ln == "" {
			continue
		}
		if strings.Contains(ln, "unknown command") {
			return PolicyReply{}, ErrPolicyUnsupported
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
		return PolicyReply{}, errNoPolicyReply
	}
	f := strings.Fields(reply)
	if f[0] == "error" {
		why := strings.TrimSpace(strings.TrimPrefix(reply, "error"))
		if why == "" {
			why = "refused"
		}
		return PolicyReply{}, &PolicyError{Cmd: cmd, Reason: why}
	}
	rep := PolicyReply{Policy: f[0], Keys: -1}
	if len(f) > 1 {
		if n, err := strconv.Atoi(f[1]); err == nil {
			rep.Keys = n
		}
	}
	return rep, nil
}
