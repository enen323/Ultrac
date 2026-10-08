package logging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Error identity
// ---------------------------------------------------------------------------

func TestIsAbortDistinguishesUserCancelFromShutdown(t *testing.T) {
	// The distinction matters because the reactions differ: a cancelled
	// context is shutdown and should stay quiet, an AbortError is the user
	// pressing escape and should end the turn with a message.
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"interrupt", Interrupt(), true},
		{"interrupt-with-reason", Interruptf("stream idle"), true},
		{"wrapped interrupt", fmt.Errorf("stream: %w", Interrupt()), true},
		{"context cancelled", context.Canceled, true},
		{"context deadline", context.DeadlineExceeded, false},
		{"plain error", errors.New("boom"), false},
		{"api error", &APIError{StatusCode: 500}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAbort(tc.err); got != tc.want {
				t.Errorf("IsAbort(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAPIErrorsCarryIdentityNotWording(t *testing.T) {
	// The reason APIError is typed: both the retry loop and the error surface
	// branch on these, and both must keep working when the wording changes.
	overloaded := &APIError{Message: `{"type":"overloaded_error"}`}
	if !overloaded.IsOverloaded() {
		t.Error("an overloaded_error body must be recognised even with no status")
	}
	if (&APIError{StatusCode: 529}).IsOverloaded() != true {
		t.Error("529 must be recognised as overloaded")
	}
	if (&APIError{StatusCode: 429}).IsOverloaded() {
		t.Error("429 is a rate limit, not an overload; the backoff differs")
	}

	if !(&APIError{StatusCode: 429}).IsRateLimit() {
		t.Error("429 must be a rate limit")
	}
	if !(&APIError{StatusCode: 403}).IsAuth() {
		t.Error("403 must be an auth failure")
	}
}

func TestTemporaryExcludesClientErrors(t *testing.T) {
	// A 400 is wrong the same way next time. Retrying burns quota to
	// reproduce the same failure.
	for _, status := range []int{400, 404, 422} {
		if (&APIError{StatusCode: status}).Temporary() {
			t.Errorf("%d must not be retried", status)
		}
	}
	for _, status := range []int{408, 409, 429, 500, 502, 529} {
		if !(&APIError{StatusCode: status}).Temporary() {
			t.Errorf("%d must be retried", status)
		}
	}
}

func TestConnectionErrorIsTemporaryWithoutStatus(t *testing.T) {
	// A refused connection has no status code, but it is the textbook
	// retryable case.
	conn := &APIError{Err: errors.New("connection refused")}
	if !conn.Temporary() {
		t.Error("a transport failure with no status must still be retryable")
	}
}

func TestAsAPIErrorUnwrapsChain(t *testing.T) {
	inner := &APIError{StatusCode: 500, Message: "boom"}
	wrapped := fmt.Errorf("calling provider: %w", inner)

	got, ok := AsAPIError(wrapped)
	if !ok || got.StatusCode != 500 {
		t.Fatalf("AsAPIError did not unwrap: %v %v", got, ok)
	}
	if _, ok := AsAPIError(errors.New("plain")); ok {
		t.Error("a plain error must not look like an APIError")
	}
}

func TestErrnoCodesArePortable(t *testing.T) {
	// Callers compare against the POSIX names to match the TypeScript
	// behaviour, so the Windows spellings must not leak through.
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not exist", fs.ErrNotExist, "ENOENT"},
		{"permission", fs.ErrPermission, "EACCES"},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		if got := ErrnoCode(tc.err); got != tc.want {
			t.Errorf("ErrnoCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}

	if !IsInaccessible(&fs.PathError{Op: "open", Err: syscall.ENOTDIR}) {
		t.Error("ENOTDIR must count as inaccessible: a file where a directory belongs")
	}
	if !IsNotExist(fs.ErrNotExist) {
		t.Error("IsNotExist(ENOENT) must be true")
	}
}

// ---------------------------------------------------------------------------
// Model-facing formatting
// ---------------------------------------------------------------------------

func TestFormatTruncatesMiddleKeepingBothEnds(t *testing.T) {
	// Truncating the tail would drop the recent stdout; truncating the head
	// would drop the cause. The model needs both.
	err := &ShellError{
		Code:   1,
		Stderr: "HEAD-MARKER" + strings.Repeat("s", 6000) + "more-stderr",
		Stdout: strings.Repeat("o", 6000) + "TAIL-MARKER",
	}
	out := Format(err)

	if len(out) > maxModelErrorLen+100 {
		t.Errorf("output is %d chars, over the %d budget", len(out), maxModelErrorLen)
	}
	if !strings.Contains(out, "HEAD-MARKER") {
		t.Error("truncation dropped the head, where the cause lives")
	}
	if !strings.Contains(out, "TAIL-MARKER") {
		t.Error("truncation dropped the tail, where the recent output lives")
	}
	if !strings.Contains(out, "characters truncated") {
		t.Error("the truncation must be visible to the model")
	}
	if !strings.HasPrefix(out, "Exit code 1") {
		t.Errorf("the exit code must lead: %.40q", out)
	}
}

func TestFormatSmallErrorUnchanged(t *testing.T) {
	err := &ShellError{Code: 2, Stderr: "file not found", Stdout: "listing"}
	out := Format(err)
	if strings.Contains(out, "truncated") {
		t.Errorf("a short error must not be truncated: %q", out)
	}
	if !strings.Contains(out, "file not found") || !strings.Contains(out, "listing") {
		t.Errorf("both streams must be present: %q", out)
	}
}

func TestFormatInterrupt(t *testing.T) {
	if got := Format(Interrupt()); got != InterruptMessage {
		t.Errorf("Format(Interrupt()) = %q", got)
	}
	if got := Format(Interruptf("idle timeout")); got != "idle timeout" {
		t.Errorf("an abort reason should be shown verbatim, got %q", got)
	}
}

func TestFormatEmptyShellErrorStillCarriesTheExitCode(t *testing.T) {
	// Even with both streams empty, "Exit code 1" is the one fact the model
	// can act on. Dropping it for an empty string would read as "nothing
	// happened".
	got := Format(&ShellError{Code: 1})
	if got != "Exit code 1" {
		t.Errorf("Format(empty ShellError) = %q, want \"Exit code 1\"", got)
	}
}

func TestFormatEmptyErrorStillSaysSomething(t *testing.T) {
	if got := Format(errors.New("")); got != "Command failed with no output" {
		t.Errorf("Format(empty error) = %q", got)
	}
}

func TestFormatShowsCapturedStderr(t *testing.T) {
	// exec.Cmd only fills ExitError.Stderr when the output was captured with
	// Output(); assigning cmd.Stderr leaves it empty. So this exercises the
	// real path a Bash tool takes.
	cmd := exec.Command("sh", "-c", "echo boom >&2; exit 3")
	_, err := cmd.Output()
	if err == nil {
		t.Fatal("expected the command to fail")
	}

	out := Format(err)
	if !strings.Contains(out, "boom") {
		t.Errorf("captured stderr missing from model-facing text: %q", out)
	}
}

func TestFormatIssuesNamesTheField(t *testing.T) {
	// The TypeScript side found that a vague "invalid input" makes models
	// repeat the same wrong call. Naming the field is what fixes the retry.
	msg := FormatIssues("TodoWrite", []ValidationIssue{
		{Path: "todos[0].activeForm", Kind: IssueMissing},
		{Key: "colour", Kind: IssueUnexpected},
		{Path: "todos", Kind: IssueWrongType, Expected: "array", Got: "object"},
	})
	for _, want := range []string{
		"The required parameter `todos[0].activeForm` is missing",
		"An unexpected parameter `colour` was provided",
		"expected as `array` but provided as `object`",
		// The wording is a verbatim port of formatZodValidationError. It
		// carries no issue count, and changing that would make a transcript
		// read differently under the two implementations.
		"TodoWrite failed due to the following issues:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestFormatIssuesSingular(t *testing.T) {
	msg := FormatIssues("Read", []ValidationIssue{{Path: "file_path", Kind: IssueMissing}})
	if !strings.Contains(msg, "Read failed due to the following issue:") {
		t.Errorf("singular form wrong: %q", msg)
	}
	if strings.Contains(msg, "issues") {
		t.Errorf("one issue must not be pluralised: %q", msg)
	}
}

func TestValidationPathUsesBracketsForIndices(t *testing.T) {
	// A dot would be ambiguous with a literal key that contains one.
	got := ValidationPath([]any{"todos", 0, "activeForm"})
	if got != "todos[0].activeForm" {
		t.Errorf("ValidationPath = %q", got)
	}
	if got := ValidationPath([]any{"file_path"}); got != "file_path" {
		t.Errorf("single segment = %q", got)
	}
	if got := ValidationPath(nil); got != "" {
		t.Errorf("empty path = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Logger
// ---------------------------------------------------------------------------

func newCaptureLogger(t *testing.T) (*Logger, *os.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })

	l := New(Options{Out: f, Level: LevelDebug, Component: "test", SessionID: "sess-1"})
	return l, f, path
}

func TestLoggerRespectsLevel(t *testing.T) {
	l, f, path := newCaptureLogger(t)
	l.SetLevel(LevelWarn)

	l.Debug("dropped-debug")
	l.Info("dropped-info")
	l.Warn("kept-warn")
	l.ErrorAt("kept-error")

	l.SetOutput(f)
	_ = f.Sync()

	data, _ := os.ReadFile(path)
	out := string(data)
	if strings.Contains(out, "dropped-debug") || strings.Contains(out, "dropped-info") {
		t.Errorf("lines below the threshold were written:\n%s", out)
	}
	if !strings.Contains(out, "kept-warn") || !strings.Contains(out, "kept-error") {
		t.Errorf("lines at or above the threshold were dropped:\n%s", out)
	}
}

func TestLoggerDisabledSuppressesEverything(t *testing.T) {
	// Set by DISABLE_ERROR_REPORTING and by the cloud providers, which must
	// not send anything anywhere.
	f, err := os.Create(filepath.Join(t.TempDir(), "log.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	l := New(Options{Out: f, Level: LevelDebug, Disabled: true})
	l.Error(errors.New("secret"))
	l.ErrorAt("also secret")

	if l.Enabled(LevelError) {
		t.Error("a disabled logger must report every level as disabled")
	}
}

func TestLoggerRecordsErrorsForBugReports(t *testing.T) {
	l, _, _ := newCaptureLogger(t)
	l.Error(errors.New("first"))
	l.Error(errors.New("second"))

	got := l.RecentErrors()
	if len(got) != 2 {
		t.Fatalf("recorded %d errors, want 2", len(got))
	}
	if got[0].Message != "first" || got[1].Message != "second" {
		t.Errorf("order or content wrong: %+v", got)
	}
	if got[0].Timestamp.IsZero() {
		t.Error("a record without a timestamp cannot be correlated with a log")
	}

	l.ClearRecentErrors()
	if len(l.RecentErrors()) != 0 {
		t.Error("ClearRecentErrors did not empty the ring")
	}
}

func TestRecentErrorsRingIsBounded(t *testing.T) {
	// A thousand-line history helps nobody and costs memory over a long
	// session. The newest 100 are what a bug report needs.
	l, _, _ := newCaptureLogger(t)
	for i := range maxRecentErrors + 50 {
		l.Error(fmt.Errorf("error %d", i))
	}
	got := l.RecentErrors()
	if len(got) != maxRecentErrors {
		t.Fatalf("ring holds %d, want %d", len(got), maxRecentErrors)
	}
	if got[len(got)-1].Message != fmt.Sprintf("error %d", maxRecentErrors+49) {
		t.Errorf("newest error not retained: %s", got[len(got)-1].Message)
	}
}

type countingSink struct{ errs []error }

func (s *countingSink) LogError(err error)         { s.errs = append(s.errs, err) }
func (s *countingSink) LogMCPError(string, error)  {}
func (s *countingSink) LogMCPDebug(string, string) {}

func TestErrorsBeforeSinkAttachedAreNotLost(t *testing.T) {
	// Startup errors arrive before any sink exists and are exactly the ones
	// worth having.
	l, _, _ := newCaptureLogger(t)
	l.Error(errors.New("queued-1"))
	l.Error(errors.New("queued-2"))

	sink := &countingSink{}
	l.AttachSink(sink)

	if len(sink.errs) != 2 {
		t.Fatalf("sink received %d queued errors, want 2", len(sink.errs))
	}
	if sink.errs[0].Error() != "queued-1" {
		t.Errorf("queue drained out of order: %v", sink.errs)
	}
}

func TestAttachSinkIsIdempotent(t *testing.T) {
	// Both the subcommand pre-action hook and setup() attach the sink, and
	// neither knows about the other.
	l, _, _ := newCaptureLogger(t)
	first := &countingSink{}
	second := &countingSink{}

	l.AttachSink(first)
	l.AttachSink(second)
	l.Error(errors.New("after"))

	if len(second.errs) != 0 {
		t.Error("a second AttachSink must be a no-op")
	}
	if len(first.errs) != 1 {
		t.Errorf("the first sink should have received the error, got %d", len(first.errs))
	}
}

func TestLogLineCarriesSessionAndComponent(t *testing.T) {
	l, f, path := newCaptureLogger(t)
	l.Info("hello", F("model", "test-model"))
	_ = f.Sync()

	data, _ := os.ReadFile(path)
	out := string(data)
	for _, want := range []string{"[test]", "sess-1", "model=\"test-model\"", "INFO"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestFileSinkWritesDatedJSONL(t *testing.T) {
	dir := t.TempDir()
	sink := NewFileSink(dir)
	t.Cleanup(func() { sink.Close() })

	sink.LogError(errors.New("disk went away"))
	sink.LogMCPError("github", errors.New("connection refused"))
	sink.LogMCPDebug("github", "reconnecting")
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	errData, err := os.ReadFile(sink.ErrorsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(errData), `"kind":"error"`) {
		t.Errorf("errors file is not JSONL as expected:\n%s", errData)
	}
	if !strings.HasSuffix(sink.ErrorsPath(), time.Now().UTC().Format("2006-01-02")+".jsonl") {
		t.Errorf("error log is not dated: %s", sink.ErrorsPath())
	}

	mcpData, err := os.ReadFile(sink.MCPPath("github"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mcpData), "connection refused") {
		t.Errorf("MCP error not recorded:\n%s", mcpData)
	}
}

func TestMCPPathSanitizesServerName(t *testing.T) {
	// Windows reserves colons for drive letters, and an MCP server name may
	// contain one. A colon in a filename is an "invalid path" error there.
	sink := NewFileSink(t.TempDir())
	path := sink.MCPPath("plugin:github:server")
	if strings.ContainsAny(filepath.Base(filepath.Dir(path)), `:\/`) {
		t.Errorf("MCP log directory was not sanitized: %s", path)
	}
}

func TestParseLevelDefaultsToInfo(t *testing.T) {
	// A typo in a debug flag should not stop the program from starting.
	if got := ParseLevel("verbose"); got != LevelInfo {
		t.Errorf("ParseLevel(\"verbose\") = %v, want info", got)
	}
	if got := ParseLevel("DEBUG"); got != LevelDebug {
		t.Errorf("ParseLevel should be case-insensitive, got %v", got)
	}
	if got := ParseLevel("off"); got != LevelOff {
		t.Errorf("ParseLevel(\"off\") = %v", got)
	}
}

func TestNoRetryStopsTheLoop(t *testing.T) {
	// Used for a 529 on a background classification, where a retry is pure
	// gateway amplification.
	base := &APIError{StatusCode: 529}
	wrapped := NoRetry(base)
	if wrapped.Error() != base.Error() {
		t.Errorf("NoRetry changed the message: %q", wrapped.Error())
	}
	_, ok := AsAPIError(wrapped)
	if !ok {
		t.Error("NoRetry must stay unwrappable to *APIError")
	}
	if NoRetry(nil) != nil {
		t.Error("NoRetry(nil) must be nil, not a non-nil wrapper")
	}
}
