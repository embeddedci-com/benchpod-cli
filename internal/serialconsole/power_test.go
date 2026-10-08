package serialconsole

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// powerPod answers `power` like the firmware's console (console.c).
type powerPod struct {
	old    bool
	on     [3]bool
	writes []string
}

func (p *powerPod) console() *Console {
	return newConsole(&fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		p.writes = append(p.writes, line)
		out.WriteString(line + "\r\n[net] dhcp renew\r\n")
		f := strings.Fields(line)
		switch {
		case p.old || f[0] != "power" || len(f) < 3:
			fmt.Fprintf(out, "  unknown command '%s' (try 'help')\r\n", f[0])
		case f[1] != "1" && f[1] != "2":
			out.WriteString("  bad eFuse index\r\n")
		default:
			var e int
			fmt.Sscan(f[1], &e)
			p.on[e] = f[2] == "on"
			state := "OFF"
			if p.on[e] {
				state = "ON"
			}
			fmt.Fprintf(out, "  eFuse%d %s\r\n", e, state)
		}
		out.WriteString("> ")
	}})
}

func TestTargetPowerOnOff(t *testing.T) {
	pod := &powerPod{}
	c := pod.console()
	if err := c.TargetPower(testContext(t), 1, true); err != nil || !pod.on[1] {
		t.Fatalf("on: %v (on=%v)", err, pod.on[1])
	}
	if err := c.TargetPower(testContext(t), 1, false); err != nil || pod.on[1] {
		t.Fatalf("off: %v (on=%v)", err, pod.on[1])
	}
	if got := strings.Join(pod.writes, "|"); got != "power 1 on|power 1 off" {
		t.Fatalf("writes = %s", got)
	}
}

func TestTargetPowerRefusals(t *testing.T) {
	err := (&powerPod{}).console().TargetPower(testContext(t), 3, true)
	if pe, ok := tcpclient.AsPodError(err); !ok || pe.Cmd != "power" || pe.Reason != "bad eFuse index" {
		t.Fatalf("bad index: %#v", err)
	}
	// A console without the command must fail, not pass for a powered target.
	err = (&powerPod{old: true}).console().TargetPower(testContext(t), 1, true)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("old firmware: %v", err)
	}
}
