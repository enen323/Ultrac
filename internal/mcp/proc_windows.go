//go:build windows

package mcp

// Process-tree primitives for Windows
// (docs/开发计划/新功能计划/mcp-plan.md R7).
//
// There are no process groups on Windows, so the tree kill shells out to the
// trusted System32 taskkill. Copied from internal/tool/tree_windows.go rather
// than exported from it, because that package's killProcessTree is private to
// the eight coding tools and exporting it would make internal/mcp depend on
// the tool registry the plan keeps it parallel to.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// envKey normalizes an environment key for comparison. Windows environment
// variables are case-insensitive, so PATH, Path and path are one variable and
// an overlay that treats them as three would duplicate it.
func envKey(k string) string { return strings.ToUpper(k) }

// prepareProcess hides the child's console window.
//
// A GUI launch (the desktop shell) would otherwise flash a console for every
// spawned server, and a server that is a GUI app itself would open one. The
// constant is CREATE_NO_WINDOW, spelled out so this file needs no
// golang.org/x/sys dependency.
const createNoWindow = 0x08000000

func prepareProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// killProcessTree kills pid and all its descendants with taskkill /F /T.
//
// SystemRoot is used rather than PATH so cleanup never depends on an
// environment a subprocess could change; every error is swallowed, because a
// kill that fails since the process is already gone is the common case, not
// an incident.
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
