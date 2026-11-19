//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// childInGroup runs the child in its own process group, so a timeout or Ctrl-C stops everything it
// started (pytest's own children, a shell's commands), not just the first process.
func childInGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalGroup(cmd, syscall.SIGINT) }
}

// killChildGroup kills whatever is left of the child's group.
func killChildGroup(cmd *exec.Cmd) { _ = signalGroup(cmd, syscall.SIGKILL) }

func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}

// shellCommand is how a `run` step's command line runs.
func shellCommand(line string) []string { return []string{"/bin/sh", "-c", line} }
