package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// LAN pairing over the console (firmware 4.0.1+, docs/usb-serial-interface.md):
//
//	lan-clients                         -> lan-clients <n> of <max>, pairing <on|off>
//	                                       then "  <name> <spki sha256>" per computer
//	lan-clients add <name> <key>        -> lan-clients added|renamed|same, then the list
//	lan-clients remove <name|key>       -> the list | lan-clients error <why>
//	lan-tls                             -> lan-tls port 8443 https-only <on|off> sha256 <hex> ...
//
// The console pairs a computer even while the pod's USB network adapter is off (built-in Wi-Fi
// turns it off). Older firmware answers "unknown command": ErrUnknownCommand.

// LanClient is one paired computer.
type LanClient struct {
	Name   string
	SHA256 string // of the computer's TLS client key (SubjectPublicKeyInfo)
}

// LanClients is the pod's list of paired computers.
type LanClients struct {
	Outcome string // after add: "added", "renamed" or "same"
	Count   int
	Max     int
	Pairing bool // whether LAN computers must be paired
	Clients []LanClient
}

// Has reports whether key (64 hex digits) is paired.
func (l LanClients) Has(key string) bool {
	for _, c := range l.Clients {
		if strings.EqualFold(c.SHA256, key) {
			return true
		}
	}
	return false
}

// LanClientsList runs `lan-clients`.
func (c *Console) LanClientsList(ctx context.Context) (LanClients, error) {
	return c.lanClients(ctx, "lan-clients")
}

// LanClientsAdd pairs the computer with key (64 hex digits) under name (no spaces).
func (c *Console) LanClientsAdd(ctx context.Context, name, key string) (LanClients, error) {
	if name == "" || strings.ContainsAny(name, " \t\r\n\"") {
		return LanClients{}, fmt.Errorf("lan-clients add: bad name %q", name)
	}
	if len(key) != 64 || strings.Trim(strings.ToLower(key), "0123456789abcdef") != "" {
		return LanClients{}, fmt.Errorf("lan-clients add: the key must be 64 hex digits")
	}
	return c.lanClients(ctx, "lan-clients add "+name+" "+strings.ToLower(key))
}

// LanClientsRemove removes the computers named what, or the one key starting with what.
func (c *Console) LanClientsRemove(ctx context.Context, what string) (LanClients, error) {
	if what == "" || strings.ContainsAny(what, " \t\r\n") {
		return LanClients{}, fmt.Errorf("lan-clients remove: bad name or key %q", what)
	}
	return c.lanClients(ctx, "lan-clients remove "+what)
}

// LanClientsClear forgets every paired computer.
func (c *Console) LanClientsClear(ctx context.Context) (LanClients, error) {
	return c.lanClients(ctx, "lan-clients clear")
}

func (c *Console) lanClients(ctx context.Context, line string) (LanClients, error) {
	complete := func(acc string) bool {
		l, err := parseLanClients(acc, line)
		return err == nil && len(l.Clients) >= l.Count || (err != nil && !errors.Is(err, errNoLanClients))
	}
	out, err := c.sendCommandUntil(ctx, line, line, "", complete)
	l, perr := parseLanClients(out, line)
	if perr == nil || !errors.Is(perr, errNoLanClients) {
		return l, perr
	}
	if err != nil {
		return LanClients{}, fmt.Errorf("lan-clients: %w", err)
	}
	return LanClients{}, fmt.Errorf("lan-clients: no reply from the pod (got: %q)", tail([]byte(out), 200))
}

var errNoLanClients = errors.New("no lan-clients reply yet")

// parseLanClients reads the reply to line (the first copy of the echoed command is skipped).
func parseLanClients(raw, line string) (LanClients, error) {
	var l LanClients
	echoSeen, header := false, false
	for _, ln := range consoleLines(raw) {
		if strings.Contains(ln, "unknown command") {
			return LanClients{}, ErrUnknownCommand
		}
		if !echoSeen && ln == line {
			echoSeen = true
			continue
		}
		rest, ok := strings.CutPrefix(ln, "lan-clients ")
		if ok {
			if why, isErr := replyError(rest); isErr {
				return LanClients{}, &CommandError{Cmd: "lan-clients", Reason: why}
			}
			switch rest {
			case "added", "renamed", "same":
				l.Outcome = rest
				continue
			}
			// "<n> of <max>, pairing <on|off>"
			f := strings.Fields(strings.ReplaceAll(rest, ",", " "))
			if len(f) >= 3 && f[1] == "of" {
				n, err1 := strconv.Atoi(f[0])
				m, err2 := strconv.Atoi(f[2])
				if err1 == nil && err2 == nil {
					l.Count, l.Max, header = n, m, true
					l.Pairing = len(f) >= 5 && f[3] == "pairing" && f[4] == "on"
					l.Clients = nil
				}
			}
			continue
		}
		if header {
			f := strings.Fields(ln)
			if len(f) == 2 && len(f[1]) == 64 {
				l.Clients = append(l.Clients, LanClient{Name: f[0], SHA256: f[1]})
			}
		}
	}
	if !header {
		return LanClients{}, errNoLanClients
	}
	return l, nil
}

// LanTLSFingerprint is the SHA-256 of the pod's LAN TLS certificate, from `lan-tls`; "" when the
// pod has none.
func (c *Console) LanTLSFingerprint(ctx context.Context) (string, error) {
	line := "lan-tls"
	complete := func(acc string) bool { _, ok := lastReply(acc, "lan-tls", line); return ok }
	out, err := c.sendCommandUntil(ctx, line, line, "", complete)
	if strings.Contains(out, "unknown command") {
		return "", ErrUnknownCommand
	}
	rest, ok := lastReply(out, "lan-tls", line)
	if !ok {
		if err != nil {
			return "", fmt.Errorf("lan-tls: %w", err)
		}
		return "", fmt.Errorf("lan-tls: no reply from the pod (got: %q)", tail([]byte(out), 200))
	}
	if why, isErr := replyError(rest); isErr {
		return "", &CommandError{Cmd: "lan-tls", Reason: why}
	}
	f := strings.Fields(rest)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "sha256" && len(f[i+1]) == 64 {
			return strings.ToLower(f[i+1]), nil
		}
	}
	return "", nil
}
