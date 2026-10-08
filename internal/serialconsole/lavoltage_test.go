package serialconsole

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// laVoltagePod answers la-voltage like the firmware's console (console.c).
type laVoltagePod struct {
	mv     int
	v2     bool   // a v2 board: no 1.8 V
	inUse  string // pins in use: a change is refused
	old    bool
	writes []string
}

func (p *laVoltagePod) console() *Console {
	return newConsole(&fakeConsole{onWrite: func(line string, out *bytes.Buffer) {
		p.writes = append(p.writes, line)
		out.WriteString(line + "\r\n[net] dhcp renew\r\n")
		f := strings.Fields(line)
		switch {
		case p.old:
			fmt.Fprintf(out, "  unknown command '%s' (try 'help')\r\n", f[0])
		case len(f) == 1 && p.mv == 0:
			out.WriteString("  LA VCCIO = UNSET (st=0)\r\n")
		case len(f) == 1:
			fmt.Fprintf(out, "  LA VCCIO = %d mV (st=1)\r\n", p.mv)
		case p.inUse != "" && f[1] != fmt.Sprint(p.mv):
			fmt.Fprintf(out, "  la voltage can't change while pins are in use: %s; stop them first\r\n", p.inUse)
		case f[1] == "1800" && p.v2:
			out.WriteString("  1.8 V needs a v3 pod; this board is v2 (its TPS2116 has no 1.8 V setting)\r\n")
		case f[1] == "1800" || f[1] == "3300":
			fmt.Sscan(f[1], &p.mv)
			fmt.Fprintf(out, "  LA VCCIO = %d mV (st=1)\r\n", p.mv)
		default:
			out.WriteString("  usage: la-voltage <1800|3300>\r\n")
		}
		out.WriteString("> ")
	}})
}

func TestLAVoltageShowAndSet(t *testing.T) {
	pod := &laVoltagePod{}
	c := pod.console()
	if mv, err := c.LAVoltage(testContext(t), 0); err != nil || mv != 0 {
		t.Fatalf("unset = %d, %v", mv, err)
	}
	if mv, err := c.LAVoltage(testContext(t), 3300); err != nil || mv != 3300 || pod.mv != 3300 {
		t.Fatalf("set = %d, %v", mv, err)
	}
	if mv, err := c.LAVoltage(testContext(t), 0); err != nil || mv != 3300 {
		t.Fatalf("show = %d, %v", mv, err)
	}
	if got := strings.Join(pod.writes, "|"); got != "la-voltage|la-voltage 3300|la-voltage" {
		t.Fatalf("writes = %s", got)
	}
}

func TestLAVoltageRefusals(t *testing.T) {
	for _, pod := range []*laVoltagePod{
		{mv: 3300, v2: true},
		{mv: 3300, inUse: "LA3 (uart_rx), LA4 (uart_tx)"},
	} {
		_, err := pod.console().LAVoltage(testContext(t), 1800)
		pe, ok := tcpclient.AsPodError(err)
		if !ok || pe.Cmd != "la-voltage" || pe.Reason == "" || strings.HasPrefix(pe.Reason, " ") {
			t.Fatalf("err = %#v", err)
		}
	}
	_, err := (&laVoltagePod{old: true}).console().LAVoltage(testContext(t), 0)
	if !errors.Is(err, ErrUnknownCommand) {
		t.Fatalf("old firmware: %v", err)
	}
}
