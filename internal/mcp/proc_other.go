//go:build !windows

package mcp

// Process-tree primitives for Unix. The build targets Windows first, but the
// stdio transport is not Windows-specific and a tree kill belongs to whichever
// platform it lands on.

import (
	"os/exec"
	"syscall"
)

// envKey normalizes an environment key for comparison. Unix environment
// variables are case-sensitive, so this is the identity function; the Windows
// build folds case instead. Both exist so transport.go's env overlay does not
// need to know which platform it is on.
func envKey(k string) string { return k }

// prepareProcess puts the child in its own process group, so the tree kill
// below can signal the whole group instead of just the child.
func prepareProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree signals the child's whole process group. A launcher script
// that spawned a daemon would otherwise keep the inherited pipes open, which
// is exactly the leak the close budget in transport.go exists to prevent.
//
// The error is swallowed: a group that is already gone is the common case.
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
