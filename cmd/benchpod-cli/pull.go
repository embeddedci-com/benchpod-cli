package main

import (
	"context"
	"encoding/json"
)

// setLAPull pulls LA la "up", "down" or "off" (la_pull with pull; the pod checks the direction
// against the pin's resistor: BP0023, BP0024).
func setLAPull(ctx context.Context, c podCommander, la int, want string) (json.RawMessage, error) {
	return c.Command(ctx, map[string]any{"cmd": "la_pull", "la": la, "pull": want})
}
