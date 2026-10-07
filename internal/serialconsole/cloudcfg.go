package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The pod's company CA certificate and HTTP proxy over the console (firmware
// docs/design/cloud-hardening.md, section 3):
//
//	ca                               -> ca <subject> <sha256hex>   (one line per certificate) | ca none
//	ca-clear                         -> ca-clear ok | ca-clear error <why>
//	proxy                            -> proxy <host:port> auth|noauth | proxy none
//	proxy-set <host:port> [user pw]  -> proxy <host:port> auth|noauth | proxy error <why>
//	proxy-clear                      -> proxy none
//
// Any command may also answer "<cmd> error <why>". The CA itself is uploaded with the regular
// upload path (Upload with target "ca"). Older firmware answers "unknown command".

// ErrUnknownCommand is older firmware's answer to a console command it does not have. It is the
// same value as ErrPolicyUnsupported, so either name matches with errors.Is.
var ErrUnknownCommand = ErrPolicyUnsupported

// ConsoleLineMax is the longest command line the firmware's line editor takes (CONSOLE_LINE_MAX
// is 128 including the terminating NUL).
const ConsoleLineMax = 127

// CommandError is the pod refusing a console command ("<cmd> error <why>").
type CommandError struct {
	Cmd    string
	Reason string
}

func (e *CommandError) Error() string { return e.Cmd + ": " + e.Reason }

// CACert is one certificate of the pod's company CA, as the pod reports it.
type CACert struct {
	Subject string `json:"subject"`
	SHA256  string `json:"sha256"` // hex SHA-256 of the DER certificate
}

// ProxyConfig is the pod's HTTP proxy. Addr is "" when no proxy is set.
type ProxyConfig struct {
	Addr string // host:port
	Auth bool   // a user and password are stored (the pod never reports the password)
}

// CloudCA lists the company CA certificates the pod holds (none: an empty list).
func (c *Console) CloudCA(ctx context.Context) ([]CACert, error) {
	out, err := c.cfgCommand(ctx, "ca", "ca", "", parseCAComplete)
	if err != nil {
		return nil, err
	}
	certs, err := parseCA(out)
	if errors.Is(err, errNoCAReply) {
		return nil, fmt.Errorf("ca: no reply from the pod (got: %q)", tail([]byte(out), 200))
	}
	return certs, err
}

// CloudCAClear removes the pod's company CA.
func (c *Console) CloudCAClear(ctx context.Context) error {
	out, err := c.cfgCommand(ctx, "ca-clear", "ca-clear", "", func(s string) bool {
		_, ok := lastReply(s, "ca-clear", "ca-clear")
		return ok
	})
	if err != nil {
		return err
	}
	rest, ok := lastReply(out, "ca-clear", "ca-clear")
	if !ok {
		return fmt.Errorf("ca-clear: no reply from the pod (got: %q)", tail([]byte(out), 200))
	}
	if why, isErr := replyError(rest); isErr {
		return &CommandError{Cmd: "ca-clear", Reason: why}
	}
	return nil
}

// CloudProxy reports the pod's HTTP proxy.
func (c *Console) CloudProxy(ctx context.Context) (ProxyConfig, error) {
	return c.proxyCommand(ctx, "proxy", "proxy", "")
}

// CloudProxyClear removes the pod's HTTP proxy and returns what the pod reports afterwards.
func (c *Console) CloudProxyClear(ctx context.Context) (ProxyConfig, error) {
	return c.proxyCommand(ctx, "proxy-clear", "proxy-clear", "")
}

// CloudProxySet sets the pod's HTTP proxy to addr (host:port), with a user and password when
// user is not empty. The password never reaches a log line or an error message.
func (c *Console) CloudProxySet(ctx context.Context, addr, user, password string) (ProxyConfig, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" || strings.ContainsAny(addr, " \t\r\n") {
		return ProxyConfig{}, fmt.Errorf("proxy-set: bad address %q", addr)
	}
	if (user == "") != (password == "") {
		return ProxyConfig{}, errors.New("proxy-set: pass both a user and a password, or neither")
	}
	if strings.ContainsAny(user, " \t\r\n") || strings.ContainsAny(password, " \t\r\n") {
		return ProxyConfig{}, errors.New("proxy-set: the USB console cannot carry a user or password with spaces; set it over the LAN")
	}
	line, display := "proxy-set "+addr, "proxy-set "+addr
	if user != "" {
		line += " " + user + " " + password
		display += " " + user + " ***"
	}
	if len(line) > ConsoleLineMax {
		return ProxyConfig{}, fmt.Errorf("proxy-set: the command is %d characters, the USB console takes %d; use a shorter host, user or password, or set it over the LAN",
			len(line), ConsoleLineMax)
	}
	return c.proxyCommand(ctx, line, display, password)
}

func (c *Console) proxyCommand(ctx context.Context, line, display, secret string) (ProxyConfig, error) {
	cmd := strings.Fields(line)[0]
	done := func(s string) bool {
		_, ok := proxyReply(s, cmd, line)
		return ok
	}
	out, err := c.cfgCommand(ctx, line, display, secret, done)
	if err != nil {
		return ProxyConfig{}, err
	}
	rest, ok := proxyReply(out, cmd, line)
	if !ok {
		return ProxyConfig{}, fmt.Errorf("%s: no reply from the pod (got: %q)", cmd, maskPassword(tail([]byte(out), 200), secret))
	}
	if why, isErr := replyError(rest); isErr {
		return ProxyConfig{}, &CommandError{Cmd: cmd, Reason: maskPassword(why, secret)}
	}
	f := strings.Fields(rest)
	if f[0] == "none" {
		return ProxyConfig{}, nil
	}
	p := ProxyConfig{Addr: f[0]}
	if len(f) > 1 {
		p.Auth = f[1] == "auth"
	}
	return p, nil
}

// proxyReply is the pod's answer to a proxy command: a "proxy ..." line, or "<cmd> error ...".
func proxyReply(raw, cmd, line string) (string, bool) {
	if cmd != "proxy" {
		if rest, ok := lastReply(raw, cmd, line); ok {
			if _, isErr := replyError(rest); isErr {
				return rest, true
			}
		}
	}
	return lastReply(raw, "proxy", line)
}

// cfgCommand sends line (shown as display, with secret masked) and reads until complete reports
// the reply is in (or the prompt and a short grace passed). An "unknown command" answer is
// ErrUnknownCommand.
func (c *Console) cfgCommand(ctx context.Context, line, display, secret string, complete func(string) bool) (string, error) {
	cmd := strings.Fields(line)[0]
	unknownOrDone := func(s string) bool { return strings.Contains(s, "unknown command") || complete(s) }
	out, err := c.sendCommandUntil(ctx, line, display, secret, unknownOrDone)
	if strings.Contains(out, "unknown command") {
		return "", ErrUnknownCommand
	}
	if err != nil && !complete(out) {
		return "", fmt.Errorf("%s: %w", cmd, err)
	}
	return out, nil
}

// consoleLines splits raw console output into trimmed lines, dropping prompts and backspaces.
func consoleLines(raw string) []string {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(ln, "> \t\x08"))
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	return lines
}

// lastReply returns what follows "<word> " on the last line that starts with it, skipping the
// first copy of the echoed command line.
func lastReply(raw, word, line string) (string, bool) {
	echoSeen := false
	var rest string
	found := false
	for _, ln := range consoleLines(raw) {
		if !echoSeen && ln == line {
			echoSeen = true
			continue
		}
		if strings.HasPrefix(ln, word+" ") {
			rest, found = strings.TrimSpace(ln[len(word)+1:]), true
		}
	}
	return rest, found && rest != ""
}

// replyError reports whether a reply is "error <why>", and the why.
func replyError(rest string) (string, bool) {
	if rest != "error" && !strings.HasPrefix(rest, "error ") {
		return "", false
	}
	why := strings.TrimSpace(strings.TrimPrefix(rest, "error"))
	if why == "" {
		why = "refused"
	}
	return why, true
}

// parseCAComplete reports whether the reply to `ca` is in: "ca none", an error, or at least one
// certificate line. More certificate lines may follow; the prompt ends the read.
func parseCAComplete(raw string) bool {
	_, err := parseCA(raw)
	return err == nil || !errors.Is(err, errNoCAReply)
}

var errNoCAReply = errors.New("no ca reply yet")

// parseCA reads "ca <subject> <sha256hex>" lines (the subject may hold spaces; the hash is the
// last field), "ca none", or "ca error <why>".
func parseCA(raw string) ([]CACert, error) {
	var certs []CACert
	none := false
	for _, ln := range consoleLines(raw) {
		rest, ok := strings.CutPrefix(ln, "ca ")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if why, isErr := replyError(rest); isErr {
			return nil, &CommandError{Cmd: "ca", Reason: why}
		}
		if rest == "none" {
			none = true
			continue
		}
		i := strings.LastIndexAny(rest, " \t")
		if i < 0 {
			continue
		}
		sha := strings.ToLower(rest[i+1:])
		if !isHex64(sha) {
			continue
		}
		certs = append(certs, CACert{Subject: strings.TrimSpace(rest[:i]), SHA256: sha})
	}
	if len(certs) == 0 && !none {
		return nil, errNoCAReply
	}
	return certs, nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
