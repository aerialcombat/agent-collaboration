//go:build unix

// spawn_unix.go — POSIX process-group helpers for the daemon's spawn
// path. Puts every spawned CLI into its own process group so that
// ack-timeout cancellation propagates to grandchildren (e.g. a bash
// wrapper that forked `sleep 30`). Without this, cmd.Wait would block
// on the grandchild's inherited stdout pipe even after SIGKILL hit
// the direct child.
//
// Windows is intentionally out of scope — agent-collab's hot path
// runs on Unix-flavored dev hosts (macOS + Linux).

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroup requests that the child be started in a fresh
// process group (pgid == its own pid). Called before cmd.Start / Run.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGTERM to the whole process group
// identified by pgid (which equals the leader's pid when Setpgid was
// set at fork time). Negative pid syntax is the POSIX convention for
// "signal this process group."
func killProcessGroup(pid int) {
	// Ignore errors — the process may already have exited between
	// the timeout and our cancel callback firing, which is a race
	// we can't prevent and don't need to report.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}
