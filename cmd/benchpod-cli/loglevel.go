package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The pod's console log level (firmware newer than 3.10.12, feature log_level): how much it
// prints on its USB console. Stored on the pod; every connection may read and set it (T0).

const logLevelMissing = "this firmware has no console log levels (it prints everything); update the pod's firmware (newer than 3.10.12)"

var logLevels = []string{"error", "warn", "info", "debug"}

func newLogLevelCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "log-level [error|warn|info|debug]",
		Short: "Show or set how much the pod prints on its USB console",
		Long: "Show or set the pod's console log level (firmware newer than 3.10.12).\n\n" +
			"info is the default. debug also prints the trace of every command (`[cmd] <- ...`,\n" +
			"`[cmd] -> ok ...`) and each retry of the cloud, Wi-Fi and DHCP reconnect loops; warn and\n" +
			"error print less. Errors always print. The level is stored on the pod (it survives a\n" +
			"reboot and a firmware update) and is the same setting as the console's `log` command.\n" +
			"Any connection may read or set it: Ethernet, Wi-Fi, --connection usb or embeddedci.com.",
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: logLevels,
		RunE: func(cmd *cobra.Command, args []string) error {
			set := ""
			if len(args) == 1 {
				set = strings.ToLower(strings.TrimSpace(args[0]))
				if !validLogLevel(set) {
					return fmt.Errorf("log-level: use error, warn, info or debug (got %q)", args[0])
				}
			}
			out, closeOut, err := resolveOutput(g.outputFilename)
			if err != nil {
				return err
			}
			defer closeOut()
			return runLogLevel(g, set, out)
		},
	}
}

func validLogLevel(s string) bool {
	for _, l := range logLevels {
		if s == l {
			return true
		}
	}
	return false
}

func runLogLevel(g *globalFlags, set string, out io.Writer) error {
	ctx, cancel, t, err := g.podClient("log-level", 15*time.Second)
	if err != nil {
		return err
	}
	defer cancel()
	level, err := logLevelJSON(ctx, t.client, set)
	if err != nil {
		return fmt.Errorf("log-level: %w", err)
	}
	if set == "" {
		fmt.Fprintf(out, "Console log level of the pod at %s: %s\n", t.where(), level)
	} else {
		fmt.Fprintf(out, "Console log level of the pod at %s is now %s (stored on the pod).\n", t.where(), level)
	}
	return nil
}

// logLevelJSON reads, or with set changes, the level over the JSON API.
func logLevelJSON(ctx context.Context, c podCommander, set string) (string, error) {
	req := map[string]any{"cmd": "log_level"}
	if set != "" {
		req["set"] = set
	}
	data, err := c.Command(ctx, req)
	if err != nil {
		if isUnknownCommand(err) {
			return "", errors.New(logLevelMissing)
		}
		return "", err
	}
	var rep struct {
		Level string `json:"level"`
	}
	if err := json.Unmarshal(data, &rep); err != nil || !validLogLevel(rep.Level) {
		return "", fmt.Errorf("unexpected reply %s", strings.TrimSpace(string(data)))
	}
	return rep.Level, nil
}
