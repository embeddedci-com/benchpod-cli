//go:build windows

package main

import "os/exec"

// childInGroup: on Windows the child is stopped with Process.Kill (exec.CommandContext's default).
func childInGroup(*exec.Cmd) {}

func killChildGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// shellCommand is how a `run` step's command line runs.
func shellCommand(line string) []string { return []string{"cmd", "/C", line} }
