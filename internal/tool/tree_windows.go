//go:build windows

package tool

// Process-tree primitives for Windows: there are no process groups, so the
// tree kill shells out to the trusted System32 taskkill.
//
// Port of pi's killProcessTree (utils/shell.ts:188-204): use
// %SystemRoot%\System32\taskkill.exe so cleanup never depends on PATH, and
// swallow every error — a kill that fails because the process is already
// gone is the common case, not an incident.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// newProcessGroup is a no-op on Windows: taskkill /T walks the tree from
// the pid, so no group setup is needed.
func newProcessGroup(cmd *exec.Cmd) {}

// killProcessTree kills pid and all its descendants via taskkill /F /T.
func killProcessTree(pid int) {
	if pid <= 0 {
		return
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	taskkill := filepath.Join(root, "System32", "taskkill.exe")
	cmd := exec.Command(taskkill, "/F", "/T", "/PID", strconv.Itoa(pid))
	cmd.Stdout, cmd.Stderr, cmd.Stdin = nil, nil, nil
	_ = cmd.Run()
}

// signalExitCode is a no-op on Windows: the runtime reports the raw exit
// code and there is no signal numbering to map.
func signalExitCode(ps *os.ProcessState) (int, bool) { return 0, false }
