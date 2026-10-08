package logging

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// maxModelErrorLen is how much error text a model is shown before truncation.
//
// The budget exists because tool results go into the context window. A Go
// stack trace is 500-2000 characters of frames that say nothing about what went
// wrong, and spending those tokens on every failed tool call measurably
// shortens the conversation.
const maxModelErrorLen = 10000

// halfModelErrorLen is how much of each end survives truncation.
const halfModelErrorLen = 5000

// Format renders an error for a tool_result.
//
// The output goes to the model, so it is trimmed, both streams included and
// truncated in the middle. Truncating the middle rather than the end keeps
// both the cause (usually at the top, in the message) and the recent output
// (usually at the bottom, in stdout) — the two things a model actually reads.
func Format(err error) string {
	if err == nil {
		return ""
	}

	var abort *AbortError
	if ok := errors.As(err, &abort); ok {
		if abort.Reason != "" {
			return abort.Reason
		}
		return InterruptMessage
	}

	var shellErr *ShellError
	if ok := errors.As(err, &shellErr); ok {
		full := joinNonEmpty([]string{
			fmt.Sprintf("Exit code %d", shellErr.Code),
			interruptedNote(shellErr),
			shellErr.Stderr,
			shellErr.Stdout,
		})
		return truncateMiddle(full)
	}

	full := joinNonEmpty(errParts(err))
	if full == "" {
		return "Command failed with no output"
	}
	return truncateMiddle(full)
}

// errParts extracts the parts of an error worth showing a model.
func errParts(err error) []string {
	parts := []string{err.Error()}

	// A captured command fills ExitError.Stderr; a bare Start/Wait does not.
	// Showing it when present is the difference between "exit status 1" and
	// something the model can act on.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		parts = append(parts, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return parts
}

func interruptedNote(e *ShellError) string {
	if e.Interrupted {
		return InterruptMessage
	}
	return ""
}

func joinNonEmpty(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func truncateMiddle(s string) string {
	if len(s) <= maxModelErrorLen {
		return s
	}
	dropped := len(s) - maxModelErrorLen
	return fmt.Sprintf("%s\n\n... [%d characters truncated] ...\n\n%s",
		s[:halfModelErrorLen], dropped, s[len(s)-halfModelErrorLen:])
}

// ValidationIssue is one problem with a tool's input, in the shape the model
// needs to correct itself.
//
// Distinct from a Go error on purpose: this is not a failure, it is a
// description of what the model got wrong. "The parameter `todos[0].activeForm`
// is missing" tells it what to fix; a JSON parse error does not.
type ValidationIssue struct {
	Path     string
	Kind     IssueKind
	Expected string
	Got      string
	Key      string
}

// IssueKind classifies a validation problem.
type IssueKind string

const (
	// IssueMissing is a required field that was absent.
	IssueMissing IssueKind = "missing"
	// IssueUnexpected is a field the schema does not define.
	IssueUnexpected IssueKind = "unexpected"
	// IssueWrongType is a field with the wrong type.
	IssueWrongType IssueKind = "wrong_type"
)

// FormatIssues renders validation issues as one model-facing message.
//
// The phrasing is deliberately specific. The TypeScript side found that a vague
// "invalid input" makes models retry the same wrong call; naming the field and
// the expected type makes the retry correct.
func FormatIssues(toolName string, issues []ValidationIssue) string {
	if len(issues) == 0 {
		return ""
	}

	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		switch issue.Kind {
		case IssueMissing:
			parts = append(parts, fmt.Sprintf("The required parameter `%s` is missing", issue.Path))
		case IssueUnexpected:
			parts = append(parts, fmt.Sprintf("An unexpected parameter `%s` was provided", issue.Key))
		case IssueWrongType:
			got := issue.Got
			if got == "" {
				got = "unknown"
			}
			parts = append(parts, fmt.Sprintf(
				"The parameter `%s` type is expected as `%s` but provided as `%s`",
				issue.Path, issue.Expected, got))
		}
	}

	noun := "issues"
	if len(parts) == 1 {
		noun = "issue"
	}
	return fmt.Sprintf("%s failed due to the following %s:\n%s",
		toolName, noun, strings.Join(parts, "\n"))
}

// ValidationPath renders a path like ["todos", 0, "activeForm"] as
// "todos[0].activeForm".
//
// Numeric segments become brackets because a dot there would be ambiguous with
// a literal key containing a dot.
func ValidationPath(segments []any) string {
	if len(segments) == 0 {
		return ""
	}
	var b strings.Builder
	for i, seg := range segments {
		if idx, isInt := seg.(int); isInt {
			b.WriteString("[" + strconv.Itoa(idx) + "]")
			continue
		}
		// Every non-index segment after the first gets a dot, including one
		// that follows a bracket: "todos[0].activeForm". Adding no separator
		// after "]" would read as "todos[0]activeForm", which is a different
		// path.
		if i > 0 {
			b.WriteString(".")
		}
		b.WriteString(fmt.Sprint(seg))
	}
	return b.String()
}
