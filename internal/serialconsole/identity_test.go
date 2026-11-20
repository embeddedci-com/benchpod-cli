package serialconsole

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// identityPod answers identity and identity-wipe like the firmware's console.
type identityPod struct {
	id, key, problem string
	newID            string
	old              bool
	logLine          string
}

func (p *identityPod) console() (*Console, *fakeConsole) {
	fc := &fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		out.WriteString(line + "\r\n")
		if p.logLine != "" {
			out.WriteString(p.logLine + "\r\n")
		}
		f := strings.Fields(line)
		switch {
		case p.old:
			fmt.Fprintf(out, "  unknown command '%s' (try 'help')\r\n", f[0])
		case f[0] == "identity" && p.id != "":
			fmt.Fprintf(out, "identity %s %s\r\n", p.id, p.key)
		case f[0] == "identity":
			fmt.Fprintf(out, "identity unknown %s\r\n", p.problem)
		case f[0] == "identity-wipe":
			want := p.id
			if want == "" {
				want = "unknown"
			}
			if len(f) < 2 || f[1] != want {
				fmt.Fprintf(out, "identity-wipe error confirm with the current id: identity-wipe %s\r\n", want)
				break
			}
			out.WriteString("[id] identity WIPED on the USB console: old x, new y\r\n")
			p.id, p.problem = p.newID, ""
			fmt.Fprintf(out, "identity-wipe ok %s\r\n", p.id)
		}
		out.WriteString("> ")
	}}
	return newConsole(fc), fc
}

func TestIdentityShow(t *testing.T) {
	pod := &identityPod{id: "a1b2c3", key: "KEYKEY", logLine: "[cloud] backoff: sign failed"}
	c, _ := pod.console()
	st, err := c.Identity(testContext(t))
	if err != nil || !st.Known() || st.Name() != "benchpod-a1b2c3" || st.PublicKey != "KEYKEY" || st.ConfirmToken() != "a1b2c3" {
		t.Fatalf("state = %+v, %v", st, err)
	}

	pod = &identityPod{problem: "unknown identity record (magic 0xc0ffee02 v2), not replaced"}
	c, _ = pod.console()
	st, err = c.Identity(testContext(t))
	if err != nil || st.Known() || st.ConfirmToken() != "unknown" || !strings.HasPrefix(st.Problem, "unknown identity record") {
		t.Fatalf("unknown state = %+v, %v", st, err)
	}
}

func TestIdentityOldFirmware(t *testing.T) {
	c, _ := (&identityPod{old: true}).console()
	if _, err := c.Identity(testContext(t)); !errors.Is(err, ErrIdentityUnsupported) {
		t.Fatalf("identity err = %v", err)
	}
}
