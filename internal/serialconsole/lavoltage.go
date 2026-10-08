package serialconsole

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The LA bank voltage over the console (firmware console.c, "la-voltage"):
//
//	la-voltage            -> LA VCCIO = 3300 mV (st=1) | LA VCCIO = UNSET (st=0)
//	la-voltage 1800|3300  -> LA VCCIO = 1800 mV (st=0)
//	                         | 1.8 V needs a v3 pod; this board is ...
//	                         | la voltage can't change while pins are in use: ...
//	                         | usage: la-voltage <1800|3300>

var laVCCIOLine = regexp.MustCompile(`LA VCCIO = (UNSET|\d+ mV)`)

// laVoltageRefusals start the console's refusal lines.
var laVoltageRefusals = []string{"1.8 V needs", "la voltage ", "usage: la-voltage"}

var errNoLAVoltageReply = errors.New("no la-voltage reply yet")

// LAVoltage sets the LA bank voltage to mv (1800 or 3300), or only reads it when mv is 0, and
// returns the selection the pod reports afterwards in millivolts (0: not set yet).
func (c *Console) LAVoltage(ctx context.Context, mv int) (int, error) {
	line := "la-voltage"
	if mv != 0 {
		line += " " + strconv.Itoa(mv)
	}
	complete := func(acc string) bool {
		_, err := parseLAVoltageReply(acc)
		return !errors.Is(err, errNoLAVoltageReply)
	}
	out, err := c.sendCommandUntil(ctx, line, line, "", complete)
	got, perr := parseLAVoltageReply(out)
	if perr == nil || !errors.Is(perr, errNoLAVoltageReply) {
		return got, perr
	}
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("la-voltage: no reply from the pod (got: %q)", tail([]byte(out), 200))
}

// parseLAVoltageReply finds the reply in raw console output; log lines interleave freely.
func parseLAVoltageReply(raw string) (int, error) {
	s := strings.ReplaceAll(raw, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(ln, "> \t\x08"))
		if strings.Contains(ln, "unknown command") {
			return 0, ErrUnknownCommand
		}
		if m := laVCCIOLine.FindStringSubmatch(ln); m != nil {
			if m[1] == "UNSET" {
				return 0, nil
			}
			n, _ := strconv.Atoi(strings.TrimSuffix(m[1], " mV"))
			return n, nil
		}
		for _, p := range laVoltageRefusals {
			if strings.HasPrefix(ln, p) {
				return 0, &CommandError{Cmd: "la-voltage", Reason: ln}
			}
		}
	}
	return 0, errNoLAVoltageReply
}
