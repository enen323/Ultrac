package builtin

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"ultrac/internal/contract/tool"
	"ultrac/internal/shell"
)

// BashInput mirrors BashTool's schema.
type BashInput struct {
	Command               string `json:"command"`
	Timeout               *int   `json:"timeout,omitempty"`
	Description           string `json:"description,omitempty"`
	RunInBackground       *bool  `json:"run_in_background,omitempty"`
	DangerouslyDisableSan *bool  `json:"dangerouslyDisableSandbox,omitempty"`
}

const bashSchema = `{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "The command to execute"
    },
    "timeout": {
      "type": "number",
      "description": "Optional timeout in milliseconds (max 600000)"
    },
    "description": {
      "type": "string",
      "description": "Clear, concise description of what this command does in active voice. Never use words like \"complex\" or \"risk\" in the description - just describe what it does.\n\nFor simple commands (git, npm, standard CLI tools), keep it brief (5-10 words):\n- ls -> \"List files in current directory\"\n- git status -> \"Show working tree status\"\n- npm install -> \"Install package dependencies\"\n\nFor commands that are harder to parse at a glance (piped commands, obscure flags, etc.), add enough context to clarify what it does:\n- find . -name \"*.tmp\" -exec rm {} \\; -> \"Find and delete all .tmp files recursively\"\n- git reset --hard origin/main -> \"Discard all local changes and match remote main\"\n- curl -s url | jq '.data[]' -> \"Fetch JSON from URL and extract data array elements\""
    },
    "run_in_background": {
      "type": "boolean",
      "description": "Set to true to run this command in the background. The command is started detached and its output is not captured; this engine reports the PID instead."
    },
    "dangerouslyDisableSandbox": {
      "type": "boolean",
      "description": "Set this to true to dangerously override sandbox mode and run commands without sandboxing."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`

const bashDescription = `Executes a given bash command and returns its output.

The working directory persists between commands, but shell state does not. The shell environment is initialized from the user's profile.

IMPORTANT: Avoid using this tool to run ` + "`find`" + `, ` + "`grep`" + `, ` + "`cat`" + `, ` + "`head`" + `, ` + "`tail`" + `, ` + "`sed`" + `, ` + "`awk`" + `, or ` + "`echo`" + ` commands, unless explicitly instructed or after you have verified that a dedicated tool cannot accomplish your task. Instead, use the appropriate dedicated tool as this will provide a much better experience for the user:

- Read files: Use Read (NOT cat/head/tail)
- Edit files: Use Edit (NOT sed/awk)
- Write files: Use Write (NOT echo >/cat <<EOF)
- Communication: Output text directly (NOT echo/printf)
- File search: Use Glob (NOT find or ls)
- Content search: Use Grep (NOT grep or rg)

While the Bash tool can do similar things, it is better to use the built-in tools as they provide a better user experience and make it easier to review tool calls and give permission.

# Instructions
- If your command will create new directories or files, first use this tool to run ` + "`ls`" + ` to verify the parent directory exists and is the correct location.
- Always quote file paths that contain spaces with double quotes in your command (e.g., cd "path with spaces/file.txt")
- Try to maintain your current working directory throughout the session by using absolute paths and avoiding usage of ` + "`cd`" + `. You may use ` + "`cd`" + ` if the User explicitly requests it.
- You may specify an optional timeout in milliseconds (up to 600000ms / 10 minutes). By default, your command will timeout after 120000ms (2 minutes).
- You can use the ` + "`run_in_background`" + ` parameter to run the command in the background. Note that this engine does not capture a background command's output.
- When issuing multiple commands:
  - If the commands are independent and can run in parallel, make multiple Bash tool calls in a single message.
  - If the commands depend on each other and must run sequentially, use a single Bash call with '&&' to chain them together.
  - Use ';' only when you need to run commands sequentially but don't care if earlier commands fail
  - DO NOT use newlines to separate commands (newlines are ok in quoted strings)`

const (
	bashDefaultTimeoutMs = 120_000
	bashMaxTimeoutMs     = 600_000
	bashOutputDefault    = 30_000
	bashOutputUpperLimit = 150_000
)

// shellPath resolves the shell used to run commands.
//
// On Windows there is usually no bash on PATH, and the TypeScript side reaches
// for the same variable. If nothing is found the tool fails with a message
// naming the variable rather than with a bare ENOENT, because "no such file or
// directory" for a command the user typed as `ls` is the least helpful error
// this program can produce.
func shellPath() (string, error) {
	if p := os.Getenv("CLAUDE_CODE_GIT_BASH_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, candidate := range []string{"bash", "sh"} {
		if p, err := exec.LookPath(candidate); err == nil {
			return p, nil
		}
	}
	if runtime.GOOS == "windows" {
		return "", errors.New(
			"no bash or sh found on PATH. Set CLAUDE_CODE_GIT_BASH_PATH to the bash executable (for example " +
				`C:\Program Files\Git\bin\bash.exe)`)
	}
	return "", errors.New("no shell found on PATH")
}

type bashTool struct {
	spec[BashInput]
}

// IsReadOnly and IsDestructive are input-dependent, so they override the
// static defaults: `ls` and `rm -rf` are the same tool.
func (bashTool) IsReadOnly(in BashInput) bool {
	return shell.Analyze(in.Command).ReadOnly()
}

func (bashTool) IsDestructive(in BashInput) bool {
	return shell.Analyze(in.Command).Destructive()
}

func (bashTool) Call(ctx context.Context, in BashInput, uc *tool.UseContext, _ tool.CanUseFunc, onProgress tool.ProgressFunc) (*tool.Result, error) {
	command := strings.TrimSpace(in.Command)
	if command == "" {
		return errResult("bash: command is required"), nil
	}

	if in.RunInBackground != nil && *in.RunInBackground {
		return bashBackground(command, uc)
	}

	shell, err := shellPath()
	if err != nil {
		return errResult("bash: %v", err), nil
	}

	timeout := bashTimeout(in.Timeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, shell, "-c", command)
	cmd.Dir = workDir(uc)
	// Stdin is left nil so the child reads from the null device: a command that
	// prompts fails fast instead of hanging until the timeout with no
	// explanation.
	cmd.Stdin = nil

	// Output goes to temp files rather than to buffers, and that is not an
	// optimisation — it is what makes the timeout work.
	//
	// When Stdout is not an *os.File, os/exec creates an OS pipe and a copying
	// goroutine, and Wait blocks until every writer of that pipe is closed.
	// Killing the shell does not close the pipe if the shell spawned a
	// grandchild (`bash -c "sleep 30"`), so Wait hangs for as long as the
	// grandchild lives, and a 300ms timeout takes 30 seconds. An *os.File is
	// handed to the child directly, so there is no pipe and no copier:
	// Wait returns as soon as the shell process is gone.
	outFile, err := os.CreateTemp("", "ultrac-bash-out-*")
	if err != nil {
		return errResult("bash: cannot create a temp file for output: %v", err), nil
	}
	defer func() {
		outFile.Close()
		os.Remove(outFile.Name())
	}()
	errFile, err := os.CreateTemp("", "ultrac-bash-err-*")
	if err != nil {
		return errResult("bash: cannot create a temp file for output: %v", err), nil
	}
	defer func() {
		errFile.Close()
		os.Remove(errFile.Name())
	}()

	cmd.Stdout = outFile
	cmd.Stderr = errFile

	if onProgress != nil {
		onProgress(tool.Progress{Message: "running: " + firstLineOf(command)})
	}

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	cancelled := errors.Is(ctx.Err(), context.Canceled)

	exitCode := 0
	switch {
	case timedOut:
		exitCode = -1
	case runErr != nil:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else if cancelled {
			exitCode = -1
		} else {
			return errResult("bash: could not start %s: %v", shell, runErr), nil
		}
	}

	stdoutBytes := readAndTruncate(outFile)
	stderrBytes := readAndTruncate(errFile)

	limit := bashOutputLimit()
	out := renderBashOutput(stdoutBytes, stderrBytes, limit)

	meta := map[string]any{
		"exitCode":   exitCode,
		"durationMs": elapsed.Milliseconds(),
		"timedOut":   timedOut,
		"cancelled":  cancelled,
		"stdoutLen":  len(stdoutBytes),
		"stderrLen":  len(stderrBytes),
	}
	if timedOut {
		out = "command timed out after " + itoa(int(timeout/time.Millisecond)) + "ms\n" + out
	}
	if cancelled {
		out = "command was interrupted\n" + out
	}

	res := &tool.Result{Content: truncateBytes(out, maxOutputBytes), Metadata: meta}
	// A non-zero exit is a normal outcome the model is expected to read and
	// react to, so it is IsError on the Result rather than a Go error.
	res.IsError = exitCode != 0
	return res, nil
}

// readAndTruncate reads a command's captured output.
//
// It reads from the start of the file, so the file must not have been seeked:
// os/exec writes through the descriptor and leaves the file offset wherever
// the child left it, which is why the read seeks explicitly.
func readAndTruncate(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	// Cap the read at the tool's output ceiling plus a margin. A command that
	// wrote a gigabyte has already done so; slurping it into memory is how the
	// agent dies of a runaway log.
	data, err := io.ReadAll(io.LimitReader(f, int64(bashOutputUpperLimit)+1))
	if err != nil {
		return ""
	}
	return string(data)
}

// renderBashOutput assembles what the model sees.
func renderBashOutput(stdout, stderr string, limit int) string {
	var b strings.Builder
	if strings.TrimSpace(stdout) == "" && strings.TrimSpace(stderr) == "" {
		b.WriteString("(no output)\n")
	}
	if stdout != "" {
		b.WriteString(clipOutput(stdout, limit))
		if !strings.HasSuffix(stdout, "\n") {
			b.WriteByte('\n')
		}
	}
	if stderr != "" {
		b.WriteString("\n[stderr]\n")
		b.WriteString(clipOutput(stderr, limit))
		if !strings.HasSuffix(stderr, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// clipOutput keeps both ends of an oversized output.
//
// Keeping only the head throws away the error at the bottom of a build log,
// and keeping only the tail hides the command that was echoed at the top.
// Middle-out truncation keeps the two parts that carry information.
func clipOutput(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := limit * 2 / 3
	tail := limit - head
	return s[:head] +
		"\n...[" + itoa(len(s)-limit) + " bytes omitted]...\n" +
		s[len(s)-tail:]
}

func bashTimeout(override *int) time.Duration {
	ms := bashDefaultTimeoutMs
	if v := envInt("BASH_DEFAULT_TIMEOUT_MS", 0); v > 0 {
		ms = v
	}
	max := bashMaxTimeoutMs
	if v := envInt("BASH_MAX_TIMEOUT_MS", 0); v > 0 {
		max = v
	}
	if max < ms {
		max = ms
	}
	if override != nil && *override > 0 {
		ms = *override
	}
	if ms > max {
		ms = max
	}
	return time.Duration(ms) * time.Millisecond
}

func bashOutputLimit() int {
	v := envInt("BASH_MAX_OUTPUT_LENGTH", bashOutputDefault)
	if v <= 0 {
		v = bashOutputDefault
	}
	if v > bashOutputUpperLimit {
		v = bashOutputUpperLimit
	}
	return v
}

func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// bashBackground starts a detached process.
//
// It is honest about the limitation: this engine has no background task
// registry yet (that is T2), so the output is not captured and the caller gets
// a PID. Pretending to have the output later would be worse.
func bashBackground(command string, uc *tool.UseContext) (*tool.Result, error) {
	shell, err := shellPath()
	if err != nil {
		return errResult("bash: %v", err), nil
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = workDir(uc)
	devNull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if devNull != nil {
		defer devNull.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	}
	if err := cmd.Start(); err != nil {
		return errResult("bash: could not start background command: %v", err), nil
	}
	pid := cmd.Process.Pid
	// Release the child so it is not killed when this process exits.
	_ = cmd.Process.Release()
	return okResult(
		"Started in the background with PID "+itoa(pid)+". "+
			"This engine does not capture a background command's output; "+
			"redirect it to a file in the command if you need to read it.",
		map[string]any{"background": true, "pid": pid}), nil
}

func init() {
	tool.RegisterBuiltin(tool.Adapt[BashInput](bashTool{spec: spec[BashInput]{
		Name_: "Bash",
		Hint:  "run a shell command",
		Desc:  bashDescription,
		// Not concurrency-safe: commands can depend on each other's effects,
		// and the TypeScript side serialises them for the same reason.
		ConcurrencySafe: false,
		// OpenWorld: a shell command can reach the network, which is what makes
		// it a sandbox concern.
		OpenWorld:  true,
		Schema:     bashSchema,
		SearchRead: tool.SearchRead{},
	}}))
}
