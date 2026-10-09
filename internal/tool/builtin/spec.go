// Package builtin holds the tools the engine ships with.
//
// # Wiring
//
// Every tool self-registers from its init(), so cmd/ultrac only needs one
// blank import to make the set available — the same pattern the provider
// registry uses. Nothing here is referenced by the engine directly; it sees a
// tool.Registry like it would for a plugin-provided tool.
//
// # Schemas are written out, not generated
//
// Each tool's input schema is a JSON string constant. Reflection over struct
// tags would be less typing and more magic, and it cannot express the parts
// that matter: which fields are mutually exclusive, which are required
// together, and the prose descriptions the model reads to decide how to call
// the tool. Those descriptions are the tool's interface as much as the types
// are, so they are versioned here next to the code that honours them.
//
// # Layering
//
// Part of the capability layer (L2). It imports the tool contract, never the
// permission package: the engine injects a CanUseFunc, which is how the
// TypeScript implementation kept `call(arg, ctx, canUseTool)` decoupled too.
package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"ultrac/internal/contract/tool"
)

// spec carries the static half of a tool definition.
//
// It is generic in the tool's input type so its default methods land in the
// method set with the right signature. An embedded non-generic base would
// expose `IsReadOnly(any) bool`, which does not satisfy `IsReadOnly(In) bool`
// and would silently fail to compile as a Tool — a lifetime of confusion for
// one type parameter.
type spec[In any] struct {
	Name_    string
	Aliases_ []string
	Hint     string
	Desc     string
	Schema   string
	OutSchem string

	// ReadOnly, ConcurrencySafe, Destructive and OpenWorld are the defaults.
	// A tool whose answer depends on the input overrides the method instead.
	ReadOnly        bool
	ConcurrencySafe bool
	Destructive     bool
	OpenWorld       bool
	SearchRead      tool.SearchRead
	Interrupt       tool.InterruptBehavior

	// NeedsInteraction gates the tool out of headless runs that cannot
	// prompt. Only AskUserQuestion sets it.
	NeedsInteraction bool
}

func (s spec[In]) Name() string       { return s.Name_ }
func (s spec[In]) Aliases() []string  { return s.Aliases_ }
func (s spec[In]) SearchHint() string { return s.Hint }

func (s spec[In]) Description(context.Context, In, tool.DescriptionOptions) (string, error) {
	return s.Desc, nil
}

func (s spec[In]) InputSchema() json.RawMessage {
	return json.RawMessage(s.Schema)
}

func (s spec[In]) OutputSchema() json.RawMessage {
	if s.OutSchem == "" {
		return nil
	}
	return json.RawMessage(s.OutSchem)
}

func (s spec[In]) IsConcurrencySafe(In) bool { return s.ConcurrencySafe }
func (s spec[In]) IsReadOnly(In) bool        { return s.ReadOnly }
func (s spec[In]) IsDestructive(In) bool     { return s.Destructive }
func (s spec[In]) IsOpenWorld(In) bool       { return s.OpenWorld }
func (s spec[In]) IsSearchOrRead(In) tool.SearchRead {
	return s.SearchRead
}

func (s spec[In]) InterruptBehavior() tool.InterruptBehavior {
	if s.Interrupt == "" {
		return tool.InterruptBlock
	}
	return s.Interrupt
}

func (s spec[In]) RequiresUserInteraction() bool { return s.NeedsInteraction }

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// maxOutputBytes caps one tool result.
//
// The number is the TypeScript side's MAX_OUTPUT_SIZE. Exceeding it is not a
// failure — the result is truncated with a marker, because a tool that
// returned nothing at all would leave the model with no idea what happened.
const maxOutputBytes = 256 * 1024

// maxLinesDefault is the Read tool's default window.
const maxLinesDefault = 2000

func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// Cut on a rune boundary so a truncated result is still valid UTF-8.
	cut := limit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	if cut <= 0 {
		cut = limit
	}
	return s[:cut] + "\n\n[output truncated: " + itoa(len(s)-cut) + " more bytes]"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// itoa is a dependency-free int-to-string, kept local so the tool package
// imports nothing it does not need.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// lineCount counts lines the way cat -n does: a trailing newline does not
// start a new line.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// splitLines splits without dropping the information about a trailing newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// numberLines renders content in `cat -n` form, starting at startLine.
func numberLines(content string, startLine int) string {
	lines := splitLines(content)
	var b strings.Builder
	b.Grow(len(content) + len(lines)*8)
	width := len(itoa(startLine + len(lines)))
	if width < 6 {
		width = 6
	}
	for i, line := range lines {
		num := itoa(startLine + i)
		for pad := width - len(num); pad > 0; pad-- {
			b.WriteByte(' ')
		}
		b.WriteString(num)
		b.WriteByte('\t')
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// errResult builds a tool-level failure.
//
// Tool failures travel as a Result with IsError set, not as a Go error: the
// model is supposed to read them and adjust. A Go error would abort the turn
// instead, which is a different and much worse outcome for something as
// ordinary as "file not found".
func errResult(format string, args ...any) *tool.Result {
	return &tool.Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// okResult builds a success result with metadata.
func okResult(content string, meta map[string]any) *tool.Result {
	return &tool.Result{Content: truncateBytes(content, maxOutputBytes), Metadata: meta}
}

// base64Encode is encoding/base64 in one place, so the tools that need it do
// not each grow their own import.
func base64Encode(raw []byte) string {
	return base64.StdEncoding.EncodeToString(raw)
}
