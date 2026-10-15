//go:build !windows

package tool

// Process-tree primitives for Unix (macOS/Linux): the shell is started as a
// process-group leader so a timeout or cancellation can kill every
// descendant, not just the shell.
//
// Port of pi's killProcessTree (utils/shell.ts:205-217): SIGKILL to the
// group, falling back to the single process if the group kill fails
// (typically because the child already exited).

import (
	"os"
	"os/exec"
	"syscall"
)

// newProcessGroup marks the command to run in its own process group.
func newProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree kills pid and all its descendants.
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	// The child is a group leader (newProcessGroup), so the negative pid
	// addresses the whole group.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		// Group kill can fail if the leader already exited but the group
		// still has members; fall back to the process itself.
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// signalExitCode maps a signal-terminated process to the shell convention
// 128+signal, so a killed command does not read as a success (pi:
// bash.ts:157-158). ok is false when the process was not signaled.
func signalExitCode(ps *os.ProcessState) (int, bool) {
	if ps == nil {
		return 0, false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return 128 + int(ws.Signal()), true
}
