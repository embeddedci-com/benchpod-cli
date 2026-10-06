package serialconsole

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// policyPod answers lan-policy and sig-policy like the firmware's console: the line is echoed,
// then the reply, then the prompt. old makes it answer like firmware without the commands.
type policyPod struct {
	lan, sig string
	keys     int
	old      bool
	logLine  string // an async log line written between the echo and the reply
}

func (p *policyPod) console() (*Console, *fakeConsole) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString(line + "\r\n")
		if p.logLine != "" {
			out.WriteString(p.logLine + "\r\n")
		}
		f := strings.Fields(line)
		switch {
		case p.old:
			fmt.Fprintf(out, "  unknown command '%s' (try 'help')\r\n", f[0])
		case f[0] == "lan-policy" && len(f) == 1:
			fmt.Fprintf(out, "lan-policy %s\r\n", p.lan)
		case f[0] == "lan-policy":
			switch f[1] {
			case "open", "locked", "off":
				p.lan = f[1]
				fmt.Fprintf(out, "lan-policy %s\r\n", p.lan)
			default:
				out.WriteString("lan-policy error usage: lan-policy [open|locked|off]\r\n")
			}
		case f[0] == "sig-policy" && len(f) > 1:
			p.sig = f[1]
			fallthrough
		case f[0] == "sig-policy":
			fmt.Fprintf(out, "sig-policy %s %d\r\n", p.sig, p.keys)
		}
		out.WriteString("> ")
	}}
	return newConsole(fc), fc
}

func TestPolicyShowAndSet(t *testing.T) {
	pod := &policyPod{lan: "open", sig: "audit", keys: 3, logLine: "[net] dhcp renew"}
	c, fc := pod.console()

	rep, err := c.Policy(testContext(t), "lan-policy", "")
	if err != nil || rep.Policy != "open" {
		t.Fatalf("show = %+v, %v", rep, err)
	}
	rep, err = c.Policy(testContext(t), "lan-policy", "locked")
	if err != nil || rep.Policy != "locked" || pod.lan != "locked" {
		t.Fatalf("set = %+v, %v (pod %s)", rep, err, pod.lan)
	}
	rep, err = c.Policy(testContext(t), "sig-policy", "required")
	if err != nil || rep.Policy != "required" || rep.Keys != 3 {
		t.Fatalf("sig set = %+v, %v", rep, err)
	}
	if got := fc.written.String(); got != "lan-policy\nlan-policy locked\nsig-policy required\n" {
		t.Fatalf("written = %q", got)
	}
}

func TestPolicyRefusalPassesThrough(t *testing.T) {
	pod := &policyPod{lan: "open"}
	c, _ := pod.console()
	_, err := c.Policy(testContext(t), "lan-policy", "closed")
	var pe *PolicyError
	if !errors.As(err, &pe) || pe.Reason != "usage: lan-policy [open|locked|off]" {
		t.Fatalf("err = %v", err)
	}
	if pod.lan != "open" {
		t.Fatalf("pod policy changed to %s", pod.lan)
	}
}

func TestPolicyOldFirmware(t *testing.T) {
	c, _ := (&policyPod{old: true}).console()
	if _, err := c.Policy(testContext(t), "sig-policy", ""); !errors.Is(err, ErrPolicyUnsupported) {
		t.Fatalf("err = %v, want ErrPolicyUnsupported", err)
	}
}

// TestParsePolicyReplyNeedsMoreThanTheEcho: for a set, the echo reads like a success reply, so
// output that has only the echo so far is not an answer.
func TestParsePolicyReplyNeedsMoreThanTheEcho(t *testing.T) {
	if _, err := parsePolicyReply("lan-policy off\r\n", "lan-policy", "lan-policy off"); !errors.Is(err, errNoPolicyReply) {
		t.Fatalf("err = %v, want errNoPolicyReply", err)
	}
	rep, err := parsePolicyReply("lan-policy off\r\nlan-policy off\r\n> ", "lan-policy", "lan-policy off")
	if err != nil || rep.Policy != "off" {
		t.Fatalf("rep = %+v, %v", rep, err)
	}
}
