package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/embeddedci-com/benchpod-cli/internal/features"
	"github.com/embeddedci-com/benchpod-cli/internal/tcpclient"
)

// pullHW is one LA pin's pull resistor on a board: the directions it can pull (the first is the
// one `la pullup PIN on` engages) and its value.
type pullHW struct {
	dirs []string
	ohms string
}

// pullTableV3 is today's boards (every v3 board): LA1-LA6 pull up, LA7/LA8 down, LA9-LA14 none.
// The firmware has the same table (la_pins.c) and reports it per pin (dirs); the CLI only uses this
// one for firmware without the la_pull_dir feature, which takes pullup on/off only.
var pullTableV3 = map[int]pullHW{
	1: {[]string{"up"}, "4.7k"}, 2: {[]string{"up"}, "4.7k"},
	3: {[]string{"up"}, "2.2k"}, 4: {[]string{"up"}, "2.2k"},
	5: {[]string{"up"}, "10k"}, 6: {[]string{"up"}, "10k"},
	7: {[]string{"down"}, "10k"}, 8: {[]string{"down"}, "10k"},
}

// pullRefusal is the pod's own BP0023 / BP0024, for what the CLI refuses before asking older
// firmware.
func pullRefusal(la int, want string, hw pullHW, ok bool) error {
	if !ok {
		return &tcpclient.PodError{Cmd: "la", Reason: fmt.Sprintf("LA%d has no pull resistor on this board", la),
			Code: "BP0023", Args: map[string]any{"la": la}}
	}
	have := hw.dirs[0]
	return &tcpclient.PodError{Cmd: "la", Reason: fmt.Sprintf("LA%d supports pull-%s only on this board", la, have),
		Code: "BP0024", Args: map[string]any{"la": la, "want": want, "have": have}}
}

// setLAPull pulls LA la "up", "down" or "off". Firmware with la_pull_dir gets {"pull":...} and
// decides; older firmware gets {"pullup":"on"|"off"}, which engages the pin's one direction, so the
// direction is checked here first ("up" must never engage LA7's pull-down).
func setLAPull(ctx context.Context, c podCommander, la int, want string) (json.RawMessage, error) {
	st, err := c.Command(ctx, features.StatusRequest())
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	byDir, err := features.Has(st, "la_pull_dir")
	if err != nil {
		return nil, err
	}
	if byDir {
		return c.Command(ctx, map[string]any{"cmd": "la", "la": la, "pull": want})
	}
	hw, ok := pullTableV3[la]
	if want != "off" && (!ok || hw.dirs[0] != want) {
		return nil, pullRefusal(la, want, hw, ok)
	}
	if !ok { // off on a pin without a resistor: nothing to switch (older firmware refuses the query)
		return json.Marshal(map[string]any{"la": la, "pullup": 0, "ohms": "", "pull": "none", "state": "off",
			"dirs": []string{}})
	}
	state := "off"
	if want != "off" {
		state = "on"
	}
	return c.Command(ctx, map[string]any{"cmd": "la", "la": la, "pullup": state})
}
