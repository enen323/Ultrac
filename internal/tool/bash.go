package tool

// The bash and powershell tools, ported from pi's
// packages/coding-agent/src/core/tools/bash.ts (443 lines) and
// powershell.ts (67 lines).
//
// # The three hard-won rules this file keeps
//
//  1. Subprocess output goes to a temp file, never a bytes.Buffer or pipe
//     reader. When Stdout is not an *os.File, os/exec creates an OS pipe
//     and a copying goroutine, and Wait blocks until every writer of that
//     pipe closes — killing the shell does not close the pipe if the shell
//     spawned a grandchild, so a 1-second timeout would take as long as
//     the grandchild lives. An *os.File is handed to the child directly:
//     no pipe, no copier, Wait returns as soon as the shell is gone
//     (pre-port lesson, plan R6).
//  2. Timeouts and cancellations kill the process TREE, not just the
//     shell: the shell is a process-group leader on Unix and taskkill /T
//     on Windows (pi: utils/shell.ts:187-217).
//  3. Truncation keeps the TAIL — the error at the bottom of a build log
//     is what the model needs — and names the temp file holding the full
//     output when it truncates.
//
// # Contract deviations from the TypeScript, all deliberate
//
//   - No live output streaming (pi emits throttled snapshots through
//     onUpdate). The temp-file design has no concurrent reader, so partial
//     results would require a polling goroutine; the loop already renders
//     tool_execution_start, and the final result carries everything.
//   - No PI_* session environment variables (pi exposes session metadata
//     through its ExtensionContext; the harness port has no session
//     context in Execute — that wiring belongs to P4/P5).
//   - Timeout has no default. pi has none either ("optional, no default
//     timeout"); the pre-port engine's 120s default is a policy the
//     settings layer can reinstate later.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// BashInput mirrors pi's bashSchema (bash.ts:40-43).
type BashInput struct {
	Command string   `json:"command"`
	Timeout *float64 `json:"timeout,omitempty"`
}

const bashSchema = `{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell command to execute"
    },
    "timeout": {
      "type": "number",
      "description": "Timeout in seconds (optional, no default timeout)"
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`

// bashOutputSchema is pi's bashOutputSchema (bash.ts:56-62): the payload
// programmatic callers receive. `output` is not limited like the
// model-facing text.
const bashOutputSchema = `{
  "type": "object",
  "properties": {
    "output": { "type": "string", "description": "Combined stdout and stderr, possibly truncated" },
    "truncated": { "type": "boolean" },
    "full_output_path": { "type": "string", "description": "Full output, when truncated" },
    "exit_code": { "type": "number" },
    "wall_time_seconds": { "type": "number" }
  },
  "required": ["output", "truncated", "exit_code", "wall_time_seconds"]
}`

// BashDetails is the tool's Details payload (bash.ts:68-71).
type BashDetails struct {
	Truncation     *TruncationResult `json:"truncation,omitempty"`
	FullOutputPath string            `json:"fullOutputPath,omitempty"`
}

// shellToolConfig is pi's ShellToolConfig (bash.ts:233-241).
type shellToolConfig struct {
	name          string
	label         string
	shellName     string
	promptSnippet string
	tempPrefix    string
}

// maxTimeoutMS is pi's MAX_TIMEOUT_MS (bash.ts:22): the int32-millisecond
// ceiling, above which the timeout cannot be represented.
const maxTimeoutMS = 2_147_483_647

// maxTimeoutSeconds is the same ceiling in seconds, as the error message
// reports it (bash.ts:25 — a float division there, 2147483.647).
const maxTimeoutSeconds = float64(maxTimeoutMS) / 1000

// structuredOutputMaxBytes caps structuredContent.output (bash.ts:24).
const structuredOutputMaxBytes = 1024 * 1024

// tailMargin is extra window beyond the byte limit when reading the tail,
// so the truncation always has whole lines to work with.
const tailMargin = 256 * 1024

var bashToolConfig = shellToolConfig{
	name:          "bash",
	label:         "bash",
	shellName:     "bash",
	promptSnippet: "Execute bash commands (ls, grep, find, etc.)",
	tempPrefix:    "ultrac-bash-",
}

// NewBashTool builds the bash tool for cwd.
func NewBashTool(cwd string) *agent.AgentTool[BashInput] {
	return newShellTool(cwd, bashToolConfig, resolveBashShell)
}

// NewPowerShellTool builds the powershell tool for cwd.
func NewPowerShellTool(cwd string) *agent.AgentTool[BashInput] {
	return newShellTool(cwd, powershellToolConfig, resolvePowerShell)
}

// newShellTool is the shared constructor — pi's createShellToolDefinition
// (bash.ts:243-415).
func newShellTool(cwd string, cfg shellToolConfig, resolveShell func() (string, []string, error)) *agent.AgentTool[BashInput] {
	return &agent.AgentTool[BashInput]{
		Tool: ai.Tool{
			Name: cfg.name,
			// pi: bash.ts:255.
			Description: fmt.Sprintf(
				"Execute a %s command in the current working directory. Returns stdout and stderr. Output is truncated to last %d lines or %dKB (whichever is hit first). If truncated, full output is saved to a temp file. Optionally provide a timeout in seconds.",
				cfg.shellName, defaultMaxLines, defaultMaxBytes/1024),
			Parameters: json.RawMessage(bashSchema),
			// pi: bash.ts:260.
			ConstrainedSampling: jsonSchemaPrefer(),
		},
		Label:        cfg.label,
		OutputSchema: json.RawMessage(bashOutputSchema),
		Execute: func(ctx context.Context, _ string, in BashInput, _ func(agent.Result)) (agent.Result, error) {
			return runShellCommand(ctx, cwd, cfg, resolveShell, in)
		},
	}
}

func init() {
	Register("bash", func(cwd string) agent.Tool { return agent.Erase(NewBashTool(cwd)) })
	Register("powershell", func(cwd string) agent.Tool { return agent.Erase(NewPowerShellTool(cwd)) })
}

// ---------------------------------------------------------------------------
// Shell resolution
// ---------------------------------------------------------------------------

// resolveBashShell finds the shell used to run bash commands.
//
// Order: the explicit path from CLAUDE_CODE_GIT_BASH_PATH (kept from the
// pre-port engine — on Windows there is often no bash on PATH and a bare
// ENOENT for `ls` is the least helpful error this program can produce),
// then the known Git Bash install locations (pi's getShellConfig,
// utils/shell.ts:88-120), then bash on PATH, then sh.
//
// pi's WSL-legacy special case (C:\Windows\System32\bash.exe switching to
// stdin command transport) is not ported: this project targets Git Bash.
func resolveBashShell() (string, []string, error) {
	if p := os.Getenv("CLAUDE_CODE_GIT_BASH_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, []string{"-c"}, nil
		}
	}
	if windowsOS {
		var candidates []string
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			candidates = append(candidates, filepath.Join(pf, "Git", "bin", "bash.exe"))
		}
		if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
			candidates = append(candidates, filepath.Join(pf86, "Git", "bin", "bash.exe"))
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, []string{"-c"}, nil
			}
		}
	}
	for _, candidate := range []string{"bash", "sh"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p, []string{"-c"}, nil
		}
	}
	if windowsOS {
		return "", nil, errors.New(
			"no bash or sh found on PATH. Set CLAUDE_CODE_GIT_BASH_PATH to the bash executable (for example " +
				`C:\Program Files\Git\bin\bash.exe)`)
	}
	return "", nil, errors.New("no shell found on PATH")
}

var powershellToolConfig = shellToolConfig{
	name:          "powershell",
	label:         "powershell",
	shellName:     "PowerShell",
	promptSnippet: "Execute PowerShell commands",
	tempPrefix:    "ultrac-powershell-",
}

// powershellArgs is pi's POWERSHELL_ARGS (utils/shell.ts:122).
var powershellArgs = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}

// utf8OutputPrefix forces UTF-8 console output so captured text is not
// mojibake (powershell.ts:16).
const utf8OutputPrefix = "try { [Console]::OutputEncoding=[System.Text.Encoding]::UTF8 } catch {}\n"

// resolvePowerShell finds PowerShell, preferring PowerShell 7
// (pi: utils/shell.ts:125-136). Windows only.
func resolvePowerShell() (string, []string, error) {
	if !windowsOS {
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return "", nil, errors.New("The powershell tool is only available on Windows.")
	}
	for _, candidate := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p, powershellArgs, nil
		}
	}
	//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
	return "", nil, errors.New("No PowerShell executable found. Install PowerShell or add powershell.exe/pwsh.exe to PATH.")
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// runShellCommand executes one command and renders its result
// (pi: bash.ts:261-412).
func runShellCommand(
	ctx context.Context,
	cwd string,
	cfg shellToolConfig,
	resolveShell func() (string, []string, error),
	in BashInput,
) (agent.Result, error) {
	command := in.Command
	if strings.TrimSpace(command) == "" {
		return agent.Result{}, errors.New("command is required")
	}

	// Timeout validation happens before anything else is started
	// (pi's resolveTimeoutMs, bash.ts:27-38): the message names the
	// model's own value so it can correct the call.
	var timeout time.Duration
	hasTimeout := false
	if in.Timeout != nil {
		t := *in.Timeout
		if math.IsNaN(t) || math.IsInf(t, 0) || t <= 0 {
			//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
			return agent.Result{}, errors.New("Invalid timeout: must be a finite number of seconds")
		}
		if t*1000 > maxTimeoutMS {
			//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
			return agent.Result{}, fmt.Errorf("Invalid timeout: maximum is %s seconds", strconv.FormatFloat(maxTimeoutSeconds, 'f', -1, 64))
		}
		timeout = time.Duration(t * float64(time.Second))
		hasTimeout = true
	}

	if err := ctx.Err(); err != nil {
		return agent.Result{}, errors.New("aborted")
	}

	shell, shellArgs, err := resolveShell()
	if err != nil {
		return agent.Result{}, err
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if _, err := os.Stat(cwd); err != nil {
		// pi: bash.ts:106-108.
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return agent.Result{}, fmt.Errorf("Working directory does not exist: %s\nCannot execute %s commands.", cwd, cfg.shellName)
	}

	// The UTF-8 prefix is PowerShell-only (powershell.ts:35).
	finalCommand := command
	if cfg.name == "powershell" {
		finalCommand = utf8OutputPrefix + command
	}

	// Output goes to a temp file, shared by stdout and stderr so the two
	// streams interleave in arrival order (Go's os/exec points the child's
	// fd 1 and 2 at the same descriptor when Stdout and Stderr are the
	// same *os.File).
	outFile, err := os.CreateTemp("", cfg.tempPrefix+"*.log")
	if err != nil {
		return agent.Result{}, fmt.Errorf("cannot create a temp file for output: %v", err)
	}
	outPath := outFile.Name()

	cmd := exec.Command(shell, append(shellArgs, finalCommand)...)
	cmd.Dir = cwd
	// Stdin is left nil so the child reads from the null device: a command
	// that prompts fails fast instead of hanging until the timeout with
	// no explanation (kept from the pre-port bash tool).
	cmd.Stdin = nil
	cmd.Stdout = outFile
	cmd.Stderr = outFile
	newProcessGroup(cmd)

	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		outFile.Close()
		os.Remove(outPath)
		//lint:ignore ST1005 pi-verbatim contract: this exact string is what pi sends the model.
		return agent.Result{}, fmt.Errorf("Failed to start %s: %v", cfg.shellName, err)
	}

	// Wait runs on its own goroutine so the watchdog can kill the tree on
	// timeout or cancellation. Wait returns as soon as the shell is gone
	// even if grandchildren hold the output file — that is the whole point
	// of redirecting to an *os.File instead of a pipe.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var (
		waitErr   error
		timedOut  bool
		cancelled bool
	)
	var timer *time.Timer
	var timerC <-chan time.Time
	if hasTimeout {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}
	select {
	case waitErr = <-done:
	case <-timerC:
		timedOut = true
		killProcessTree(cmd.Process.Pid)
		waitErr = <-done
	case <-ctx.Done():
		cancelled = true
		killProcessTree(cmd.Process.Pid)
		waitErr = <-done
	}

	// Release the parent's handle before any cleanup: on Windows, deleting
	// a file that still has an open handle fails (no FILE_SHARE_DELETE),
	// so the not-truncated temp file would leak. The child wrote through
	// its own descriptor, so nothing else needs this one.
	outFile.Close()

	// Read the captured output before deciding what to report. On the
	// error paths the partial output still belongs in the message — a
	// timed-out build log is exactly what the model needs to see.
	tail, tr, totalLines, _, lastLineBytes := readOutputTail(outPath, defaultMaxLines, defaultMaxBytes)

	render := func(emptyText string) string {
		text := tail
		if text == "" {
			text = emptyText
		}
		return text
	}

	if cancelled {
		if tr.Truncated {
			text := render("")
			text += truncationNotice(tr, totalLines, lastLineBytes, outPath)
			return agent.Result{}, errors.New(appendStatus(text, "Command aborted"))
		}
		os.Remove(outPath)
		return agent.Result{}, errors.New(appendStatus(render(""), "Command aborted"))
	}
	if timedOut {
		text := render("")
		if tr.Truncated {
			text += truncationNotice(tr, totalLines, lastLineBytes, outPath)
		} else {
			os.Remove(outPath)
		}
		secs := strconv.FormatFloat(*in.Timeout, 'f', -1, 64)
		return agent.Result{}, errors.New(appendStatus(text, "Command timed out after "+secs+" seconds"))
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if !errors.As(waitErr, &ee) {
			os.Remove(outPath)
			return agent.Result{}, waitErr
		}
	}

	exitCode := 0
	if waitErr != nil {
		exitCode = exitCodeOf(waitErr)
	}
	wallSeconds := math.Round(time.Since(startedAt).Seconds()*10) / 10

	// The full output is read while the temp file still exists — pi gets it
	// from its in-memory chunks, this port from the file. Removing it
	// first would leave structuredContent.output empty for short runs.
	full, fullTruncated := readFullOutput(outPath, structuredOutputMaxBytes)

	outputText := render("(no output)")
	var details BashDetails
	hasDetails := false
	if tr.Truncated {
		outputText += truncationNotice(tr, totalLines, lastLineBytes, outPath)
		details = BashDetails{Truncation: &tr, FullOutputPath: outPath}
		hasDetails = true
	} else {
		// Nothing truncated: no reason to keep the file (the parent's
		// handle is already closed, so the remove succeeds on Windows).
		os.Remove(outPath)
	}

	if exitCode != 0 {
		res := errResult(appendStatus(outputText, fmt.Sprintf("Command exited with code %d", exitCode)))
		if hasDetails {
			res.Details = details
		}
		res.StructuredContent = bashStructuredContent(full, fullTruncated, outPath, exitCode, wallSeconds, tr.Truncated)
		return res, nil
	}

	res := textResult(outputText)
	if hasDetails {
		res.Details = details
	}
	res.StructuredContent = bashStructuredContent(full, fullTruncated, outPath, exitCode, wallSeconds, tr.Truncated)
	return res, nil
}

// bashStructuredContent builds the programmatic payload
// (pi: bash.ts:390-399).
func bashStructuredContent(output string, truncated bool, fullOutputPath string, exitCode int, wallSeconds float64, outputTruncated bool) map[string]any {
	out := map[string]any{
		"output":            output,
		"truncated":         truncated,
		"exit_code":         exitCode,
		"wall_time_seconds": wallSeconds,
	}
	if truncated && outputTruncated && fullOutputPath != "" {
		out["full_output_path"] = fullOutputPath
	}
	return out
}

// appendStatus appends a status line after a blank line, or the status
// alone when there is no output (pi's appendStatus, bash.ts:358).
func appendStatus(text, status string) string {
	if text == "" {
		return status
	}
	return text + "\n\n" + status
}

// truncationNotice renders the tail-truncation notice (pi's formatOutput,
// bash.ts:338-356).
func truncationNotice(tr TruncationResult, totalLines, lastLineBytes int, fullOutputPath string) string {
	startLine := totalLines - tr.OutputLines + 1
	endLine := totalLines
	switch {
	case tr.LastLinePartial:
		return fmt.Sprintf("\n\n[Showing last %s of line %d (line is %s). Full output: %s]",
			formatSize(tr.OutputBytes), endLine, formatSize(lastLineBytes), fullOutputPath)
	case tr.TruncatedBy == "lines":
		return fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Full output: %s]",
			startLine, endLine, totalLines, fullOutputPath)
	default:
		return fmt.Sprintf("\n\n[Showing lines %d-%d of %d (%s limit). Full output: %s]",
			startLine, endLine, totalLines, formatSize(defaultMaxBytes), fullOutputPath)
	}
}

// exitCodeOf extracts the exit code from a Wait error, mapping a
// signal-killed shell to the conventional 128+signal (pi: bash.ts:157-158).
func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		if sc, ok := signalExitCode(ee.ProcessState); ok {
			return sc
		}
	}
	return 1
}

// ---------------------------------------------------------------------------
// Output reading
// ---------------------------------------------------------------------------

// readOutputTail reads the tail of the captured output file with bounded
// memory: one forward pass counts lines and locates the last newline, one
// bounded window read from the end feeds truncateTail.
//
// The returned TruncationResult describes the WINDOW; its TotalLines and
// TotalBytes are the window's, so callers must use the separately counted
// totals for notices (the same split pi's OutputAccumulator makes between
// its rolling tail and its running counters).
func readOutputTail(path string, maxLines, maxBytes int) (content string, tr TruncationResult, totalLines, totalBytes, lastLineBytes int) {
	f, err := os.Open(path)
	if err != nil {
		return "", TruncationResult{}, 0, 0, 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return "", TruncationResult{}, 0, 0, 0
	}
	size := int(st.Size())
	totalBytes = size

	// Forward pass: count newlines and remember the last one's offset.
	newlines, lastNewline := 0, -1
	buf := make([]byte, 256*1024)
	off := 0
	for {
		n, rerr := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				newlines++
				lastNewline = off + i
			}
		}
		off += n
		if rerr != nil {
			break
		}
	}
	totalLines = newlines
	if lastNewline != size-1 {
		totalLines++ // a final line without a newline still counts
		lastLineBytes = size - 1 - lastNewline
	}

	// Bounded window from the end, dropped to a line boundary.
	window := maxBytes + tailMargin
	if window > size {
		window = size
	}
	start := size - window
	wbuf := make([]byte, window)
	if _, err := f.ReadAt(wbuf, int64(start)); err != nil && len(wbuf) == 0 {
		return "", TruncationResult{}, totalLines, totalBytes, lastLineBytes
	}
	s := string(wbuf)
	if start > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:] // the window's first line is partial; drop it
		}
	}
	s = strings.ToValidUTF8(s, "\uFFFD")
	tr = truncateTail(s, TruncationOptions{MaxLines: maxLines, MaxBytes: maxBytes})
	return tr.Content, tr, totalLines, totalBytes, lastLineBytes
}

// readFullOutput reads the complete output for structuredContent, keeping
// the first and last halves around an omission marker when it exceeds the
// cap (pi's OutputAccumulator.readFullOutput, output-accumulator.ts:149-176).
func readFullOutput(path string, maxBytes int) (content string, truncated bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", false
	}
	size := int(st.Size())
	if size <= maxBytes {
		// Small enough to hand over whole.
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		return strings.ToValidUTF8(string(raw), "\uFFFD"), false
	}

	headBytes := maxBytes / 2
	tailBytes := maxBytes - headBytes
	head := make([]byte, headBytes)
	tail := make([]byte, tailBytes)
	_, _ = f.ReadAt(head, 0)
	_, _ = f.ReadAt(tail, int64(size-tailBytes))

	headText := strings.ToValidUTF8(string(head), "\uFFFD")
	// The head may end mid-rune; the streaming decoder in pi holds the
	// incomplete sequence back, so trim it here.
	for len(headText) > 0 && !utf8Complete(headText) {
		headText = headText[:len(headText)-1]
	}
	// The tail may start mid-rune; skip the leading continuation bytes.
	ts := 0
	for ts < len(tail) && tail[ts]&0xC0 == 0x80 {
		ts++
	}
	tailText := strings.ToValidUTF8(string(tail[ts:]), "\uFFFD")
	omitted := size - headBytes - tailBytes
	return fmt.Sprintf("%s\n\n[... %d bytes omitted ...]\n\n%s", headText, omitted, tailText), true
}

// utf8Complete reports whether s ends on a character boundary.
func utf8Complete(s string) bool {
	if s == "" {
		return true
	}
	b := s[len(s)-1]
	return b&0xC0 != 0x80
}
