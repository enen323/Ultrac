// Package logging holds the error model and the structured logger.
//
// # Why an error model at all
//
// Go's error handling is already good at propagating. What it is bad at is
// identity: `errors.Is` and `errors.As` can distinguish a typed error, but
// only if someone made it typed. The TypeScript tree grew eleven error classes
// over its lifetime, each added because a caller needed to tell two failures
// apart. This package ports the ones callers branch on, as sentinels and
// types, so the branches survive the rewrite as Go type switches instead of
// disappearing into string matching.
//
// # Layering
//
// Part of the infrastructure layer. It imports nothing from ultrac.
//
// # Two audiences, one error
//
// An error usually has two readers: the user, who needs to act, and the log,
// which needs to be diagnosable. They want different text. Truncating a stack
// for the log and then feeding that to the model wastes context tokens
// (500-2000 chars of irrelevant frames); feeding a bare message to the log
// makes it useless. So Format is for the model, Error for the log.
package logging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
)

// InterruptMessage is what the model is told when a tool was interrupted.
// The TypeScript side uses the same wording, so a transcript read by either
// implementation reads the same way.
const InterruptMessage = "[Request interrupted by user]"

// AbortError reports that the operation was cancelled by the user or by a
// cancelled context.
//
// It is a distinct type rather than context.Canceled because the two demand
// opposite reactions: a cancelled context usually means shutdown and should
// not be reported, while an AbortError means the user pressed escape and the
// turn should end cleanly with a message. Collapsing them produces either
// spurious errors on quit or a silent hang on interrupt.
type AbortError struct {
	Reason string
}

func (e *AbortError) Error() string {
	if e.Reason == "" {
		return InterruptMessage
	}
	return e.Reason
}

// Interrupt builds an AbortError with no specific reason.
func Interrupt() *AbortError { return &AbortError{} }

// Interruptf builds an AbortError with a formatted reason.
func Interruptf(format string, args ...any) *AbortError {
	return &AbortError{Reason: fmt.Sprintf(format, args...)}
}

// IsAbort reports whether err is an abort, including the context and exec
// cancellations that mean the same thing to a caller.
//
// exec.ExitError is included on purpose: a Bash tool killed by a signal
// surfaces as an *exec.ExitError, and treating that as a normal failure shows
// the user an error for something they themselves did with Ctrl-C.
func IsAbort(err error) bool {
	if err == nil {
		return false
	}
	var abort *AbortError
	if errors.As(err, &abort) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, exec.ErrNotFound) {
		return true
	}
	// os/exec surfaces a signal death as an ExitError whose ProcessState says
	// so; ctx.Err() covers the rest.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if state := exitErr.ProcessState; state != nil {
			if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return true
			}
		}
	}
	return false
}

// ShellError is a failed shell command, carrying both streams and the exit
// code.
//
// The streams are fields rather than being folded into the message because
// the model needs them and the log does not: a tool_result that says only
// "exit code 1" leaves the model unable to correct itself.
type ShellError struct {
	Stdout       string
	Stderr       string
	Code         int
	Interrupted  bool
	Command      string
	DurationMS   int64
	TimedOut     bool
	TimedOutCode int
}

func (e *ShellError) Error() string {
	return fmt.Sprintf("shell command failed (exit code %d)", e.Code)
}

// ExitCode reports the process exit code, or -1 when the command never ran.
func (e *ShellError) ExitCode() int { return e.Code }

// ConfigParseError reports a malformed configuration file.
//
// DefaultValue exists because "your settings file is broken, here is what we
// used instead" is the only useful thing to say. Reporting the error without
// the fallback leaves the user with a broken tool and no idea what the agent is
// currently using.
type ConfigParseError struct {
	Path         string
	Err          error
	DefaultValue string
}

func (e *ConfigParseError) Error() string {
	return fmt.Sprintf("invalid configuration in %s: %v", e.Path, e.Err)
}

func (e *ConfigParseError) Unwrap() error { return e.Err }

// ValidationError describes one problem in a file that was otherwise read.
//
// One bad permission rule should not reject the whole settings file — that is
// the behaviour the TypeScript side deliberately moved away from. Errors are
// per-field so the loader can accept the rest.
type ValidationError struct {
	File    string
	Path    string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("%s: %s", e.File, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.File, e.Path, e.Message)
}

// APIError is an HTTP failure from a model provider.
//
// It is a typed struct rather than a formatted error because the retry policy
// and the error surface both branch on the status code, and both must keep
// working when the message wording changes.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
	RequestID  string
	// RetryAfterSeconds is parsed from the Retry-After header when present.
	// A server that tells you when to come back should be believed over a
	// computed backoff.
	RetryAfterSeconds int
	// Err is the underlying transport failure for connection errors, where
	// there is no status code.
	Err error
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("api error")
	if e.StatusCode != 0 {
		fmt.Fprintf(&b, " %d", e.StatusCode)
	}
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.RequestID != "" {
		b.WriteString(" (request " + e.RequestID + ")")
	}
	if e.Err != nil {
		b.WriteString(": " + e.Err.Error())
	}
	return b.String()
}

func (e *APIError) Unwrap() error { return e.Err }

// Temporary reports whether retrying could plausibly succeed.
//
// The split is narrower than "5xx and 429". A 400 means the request is wrong
// and will be equally wrong next time; retrying it burns the user's quota to
// produce the same error. 401 is retryable in exactly one case — the caller
// refreshed the credential — which is why the check is a method the caller
// reads after acting, not a decision baked in here.
func (e *APIError) Temporary() bool {
	switch e.StatusCode {
	case 408, 409, 425, 429:
		return true
	}
	if e.StatusCode >= 500 {
		return true
	}
	// Connection-level failures have no status code.
	return e.StatusCode == 0 && e.Err != nil
}

// IsOverloaded reports the 529 capacity error.
//
// Three signals are checked because each covers a case the others miss.
// Matching only the status fails during a stream, where the error arrives as
// an event payload and the 529 never reaches us. Matching only the body fails
// for a mid-stream error event, where the type is a structured field rather
// than embedded text. Both together would still miss a plain HTTP 529 with an
// unhelpful body — which is the most common case of all.
func (e *APIError) IsOverloaded() bool {
	return e.StatusCode == 529 ||
		e.Code == "overloaded_error" ||
		strings.Contains(e.Message, `"type":"overloaded_error"`)
}

// IsAuth reports an authentication or authorisation failure.
func (e *APIError) IsAuth() bool { return e.StatusCode == 401 || e.StatusCode == 403 }

// IsRateLimit reports a rate limit that is not an overload.
//
// Kept separate from IsOverloaded because the two want different backoff:
// 429 has a Retry-After to honour, 529 does not.
func (e *APIError) IsRateLimit() bool { return e.StatusCode == 429 }

// AsAPIError extracts an *APIError from an error chain.
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	ok := errors.As(err, &apiErr)
	return apiErr, ok
}

// NoRetryError wraps a failure that must not be retried.
//
// It exists so the retry loop can say "stop" about an error that is otherwise
// retryable — a 529 on a background classification request, for instance, where
// a retry is pure gateway amplification and the user never sees the result.
type NoRetryError struct{ Err error }

func (e *NoRetryError) Error() string { return e.Err.Error() }
func (e *NoRetryError) Unwrap() error { return e.Err }

// NoRetry marks err as not worth retrying.
func NoRetry(err error) error {
	if err == nil {
		return nil
	}
	return &NoRetryError{Err: err}
}

// ErrnoCode returns the portable errno name for err, e.g. "ENOENT".
//
// The names are normalised rather than returned raw because the Go and
// Windows spellings differ ("ERROR_FILE_NOT_FOUND" vs "ENOENT"), and callers
// here compare against the portable names to match the TypeScript behaviour.
func ErrnoCode(err error) string {
	if err == nil {
		return ""
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errnoName(errno)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return "ENOENT"
	}
	if errors.Is(err, fs.ErrPermission) {
		return "EACCES"
	}
	return ""
}

// errnoName maps the errnos this codebase actually branches on. The full table
// is not ported: a switch over values nobody checks is a switch nobody reads.
func errnoName(e syscall.Errno) string {
	switch e {
	case syscall.ENOENT:
		return "ENOENT"
	case syscall.EACCES:
		return "EACCES"
	case syscall.EPERM:
		return "EPERM"
	case syscall.ENOTDIR:
		return "ENOTDIR"
	case syscall.ELOOP:
		return "ELOOP"
	case syscall.EISDIR:
		return "EISDIR"
	case syscall.ENOTEMPTY:
		return "ENOTEMPTY"
	case syscall.EXDEV:
		return "EXDEV"
	default:
		return e.Error()
	}
}

// IsNotExist reports a missing file or directory.
func IsNotExist(err error) bool { return ErrnoCode(err) == "ENOENT" }

// IsInaccessible reports that a path is missing, unreadable, or structurally
// unreachable.
//
// Use it after any filesystem operation to separate "nothing there / no access"
// — expected, and not worth reporting — from a real error. ENOTDIR belongs here
// because it is the case where a *file* named .ultrac sits where a directory
// should be: nothing can work, but nothing is broken either.
func IsInaccessible(err error) bool {
	switch ErrnoCode(err) {
	case "ENOENT", "EACCES", "EPERM", "ENOTDIR", "ELOOP":
		return true
	}
	return false
}

// Message returns the human-readable text for any error value.
func Message(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
