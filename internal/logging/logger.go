package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level is a log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelOff
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	case LevelOff:
		return "off"
	default:
		return fmt.Sprintf("level(%d)", int(l))
	}
}

// ParseLevel reads a level name, case-insensitively. Unknown input yields
// LevelInfo rather than an error: a typo in a debug flag should not stop the
// program from starting.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace":
		return LevelDebug
	case "info":
		return LevelInfo
	case "warn", "warning":
		return LevelWarn
	case "error", "fatal":
		return LevelError
	case "off", "none", "silent":
		return LevelOff
	default:
		return LevelInfo
	}
}

// maxRecentErrors bounds the in-memory ring. 100 is the number the TypeScript
// side keeps, chosen because it fills a bug report without becoming the bug.
const maxRecentErrors = 100

// ErrorRecord is one remembered error.
type ErrorRecord struct {
	Message   string
	Timestamp time.Time
	Stack     string
}

// Field is one structured key/value pair.
type Field struct {
	Key   string
	Value any
}

// F builds a Field.
func F(key string, value any) Field { return Field{Key: key, Value: value} }

// Logger writes structured lines and keeps a short in-memory error history.
//
// It is safe for concurrent use: tools run in parallel and each may log.
type Logger struct {
	mu sync.Mutex

	out        *os.File
	level      Level
	component  string
	sessionID  string
	version    string
	disabled   bool
	recent     []ErrorRecord
	onError    func(error)
	errorQueue []error
	sink       Sink
}

// Options configure a Logger.
type Options struct {
	// Out receives the lines. Defaults to os.Stderr, never stdout: stdout is
	// the protocol channel for NDJSON and headless mode, and a stray log line
	// there corrupts it.
	Out *os.File
	// Level is the minimum severity to write.
	Level Level
	// Component names the subsystem, e.g. "anthropic".
	Component string
	// SessionID is attached to every line for correlation.
	SessionID string
	// Version is attached to every line.
	Version string
	// Disabled suppresses all output. Set by DISABLE_ERROR_REPORTING and by
	// the cloud providers, which must not send anything anywhere.
	Disabled bool
}

// New builds a Logger.
func New(opts Options) *Logger {
	out := opts.Out
	if out == nil {
		out = os.Stderr
	}
	level := opts.Level
	if level == LevelDebug {
		// LevelDebug is the zero value, so an unset Level means debug. That is
		// the wrong default for a shipped binary: default to info and let
		// DebugLogging() opt in explicitly.
		level = LevelInfo
	}
	return &Logger{
		out:       out,
		level:     level,
		component: opts.Component,
		sessionID: opts.SessionID,
		version:   opts.Version,
		disabled:  opts.Disabled,
	}
}

// DebugLogging is the explicit opt-in for verbose output, for tests that build
// a Logger with the zero Options.
func DebugLogging() Options { return Options{Level: LevelDebug} }

// SetLevel changes the threshold.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// Level reports the current threshold.
func (l *Logger) Level() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// SetOutput redirects output. Tests use it; production leaves stderr.
func (l *Logger) SetOutput(f *os.File) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = f
}

// SetSessionID attaches a session id to subsequent lines.
func (l *Logger) SetSessionID(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sessionID = id
}

// SetComponent renames the logger's subsystem.
func (l *Logger) SetComponent(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.component = name
}

// Enabled reports whether level would be written.
func (l *Logger) Enabled(level Level) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.disabled && l.level != LevelOff && level >= l.level
}

// Debug logs at debug level.
func (l *Logger) Debug(msg string, fields ...Field) { l.log(LevelDebug, msg, fields...) }

// Info logs at info level.
func (l *Logger) Info(msg string, fields ...Field) { l.log(LevelInfo, msg, fields...) }

// Warn logs at warn level.
func (l *Logger) Warn(msg string, fields ...Field) { l.log(LevelWarn, msg, fields...) }

// ErrorAt logs at error level without recording the error in the ring buffer.
//
// Use this for messages that are not Go errors — "provider fell back to
// standard speed" — so the bug-report history stays a history of failures.
func (l *Logger) ErrorAt(msg string, fields ...Field) { l.log(LevelError, msg, fields...) }

// Error records err, writes it, and hands it to the sink.
//
// This is the one callers should reach for. It remembers the error for
// /bug, writes it with its stack, and notifies the sink — three things that
// have to keep happening together or a bug report arrives without the line
// that explains it.
func (l *Logger) Error(err error, fields ...Field) {
	if err == nil {
		return
	}

	l.record(err)
	l.logErr(err, fields...)

	l.mu.Lock()
	sink, queued := l.sink, l.errorQueue
	l.mu.Unlock()

	if sink != nil {
		sink.LogError(err)
		return
	}
	// No sink yet. Queue rather than drop: errors logged during startup are
	// exactly the ones worth having, and they arrive before any sink exists.
	l.mu.Lock()
	l.errorQueue = append(queued, err)
	l.mu.Unlock()
}

// Sink receives errors for durable storage.
//
// Separate from the Logger so the file-writing half can be attached during
// startup without the Logger knowing where it writes.
type Sink interface {
	LogError(err error)
	LogMCPError(server string, err error)
	LogMCPDebug(server, msg string)
}

// AttachSink installs the durable sink and drains anything queued before it
// existed.
//
// Idempotent, because both the subcommand pre-action hook and setup() want to
// attach it and neither knows about the other.
func (l *Logger) AttachSink(s Sink) {
	l.mu.Lock()
	if l.sink != nil {
		l.mu.Unlock()
		return
	}
	l.sink = s
	queued := l.errorQueue
	l.errorQueue = nil
	l.mu.Unlock()

	for _, err := range queued {
		s.LogError(err)
	}
}

func (l *Logger) record(err error) {
	rec := ErrorRecord{
		Message:   err.Error(),
		Timestamp: time.Now(),
		Stack:     stackOf(err),
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent) >= maxRecentErrors {
		// Drop the oldest. The newest hundred are what a bug report needs; a
		// thousand-line history helps nobody and costs memory in a long
		// session.
		l.recent = l.recent[1:]
	}
	l.recent = append(l.recent, rec)
}

// RecentErrors returns the remembered errors, oldest first.
func (l *Logger) RecentErrors() []ErrorRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]ErrorRecord, len(l.recent))
	copy(out, l.recent)
	return out
}

// ClearRecentErrors empties the ring. Tests use it; production does not.
func (l *Logger) ClearRecentErrors() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recent = nil
}

func (l *Logger) log(level Level, msg string, fields ...Field) {
	if !l.Enabled(level) {
		return
	}
	l.write(level, msg, fields...)
}

func (l *Logger) logErr(err error, fields ...Field) {
	l.mu.Lock()
	disabled := l.disabled
	l.mu.Unlock()
	if disabled {
		return
	}
	l.write(LevelError, err.Error(), fields...)
}

func (l *Logger) write(level Level, msg string, fields ...Field) {
	l.mu.Lock()
	out, component, sessionID, version := l.out, l.component, l.sessionID, l.version
	disabled := l.disabled
	l.mu.Unlock()
	if disabled || out == nil {
		return
	}

	var b strings.Builder
	b.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteString(" ")
	b.WriteString(strings.ToUpper(level.String()))
	if component != "" {
		b.WriteString(" [")
		b.WriteString(component)
		b.WriteString("]")
	}
	b.WriteString(" ")
	b.WriteString(msg)

	fields = append(fields, F("sessionId", sessionID), F("version", version))
	b.WriteString(" {")
	first := true
	for _, f := range fields {
		if f.Value == nil || f.Value == "" {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		fmt.Fprintf(&b, "%s=%q", f.Key, fmt.Sprint(f.Value))
	}
	b.WriteString("}\n")

	// One Write call so interleaved goroutines cannot tear a line. A log line
	// split across two writes is unreadable in exactly the situation you open
	// the file to investigate.
	_, _ = out.WriteString(b.String())
}

// FileSink appends errors to dated JSONL files under a cache directory.
//
// Structured lines rather than free text because these files get grepped. The
// per-day filename is what makes them deletable: rotating on size means a
// runaway loop fills the disk.
type FileSink struct {
	mu    sync.Mutex
	dir   string
	date  string
	files map[string]*os.File
	// writeErrorFunc is injectable so a test can capture without files.
	writeErrorFunc func(err error)
}

// NewFileSink builds a sink writing under dir.
func NewFileSink(dir string) *FileSink {
	return &FileSink{
		dir:   dir,
		date:  time.Now().UTC().Format("2006-01-02"),
		files: make(map[string]*os.File),
	}
}

// ErrorsPath is the file this sink appends errors to.
func (s *FileSink) ErrorsPath() string {
	return filepath.Join(s.dir, s.date+".jsonl")
}

// MCPPath is the file this sink appends one MCP server's errors to.
func (s *FileSink) MCPPath(server string) string {
	return filepath.Join(s.dir, "mcp-logs-"+sanitizeName(server), s.date+".jsonl")
}

func (s *FileSink) append(path, kind, message string, extra Field) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.files[path]
	if !ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return
		}
		var err error
		f, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		s.files[path] = f
	}

	line := fmt.Sprintf(`{"timestamp":%q,"kind":%q,"message":%q`,
		time.Now().UTC().Format(time.RFC3339Nano), kind, message)
	if extra.Key != "" && extra.Value != nil {
		line += fmt.Sprintf(",%q:%q", extra.Key, fmt.Sprint(extra.Value))
	}
	line += "}\n"
	_, _ = f.WriteString(line)
}

// LogError appends err to the daily errors file.
func (s *FileSink) LogError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	hook := s.writeErrorFunc
	s.mu.Unlock()
	if hook != nil {
		hook(err)
		return
	}
	s.append(s.ErrorsPath(), "error", err.Error(), Field{})
}

// LogMCPError appends a server error to that server's log.
func (s *FileSink) LogMCPError(server string, err error) {
	s.append(s.MCPPath(server), "mcp_error", Message(err), F("server", server))
}

// LogMCPDebug appends a server debug line.
func (s *FileSink) LogMCPDebug(server, msg string) {
	s.append(s.MCPPath(server), "mcp_debug", msg, F("server", server))
}

// Close flushes and closes every open file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for path, f := range s.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.files, path)
	}
	return firstErr
}

// sanitizeName makes a string safe as a filename component. Windows reserves
// colons for drive letters, and an MCP server name may contain one.
func sanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// stackOf extracts a stack trace from err when it carries one.
//
// Go errors do not carry stacks by default, so this looks for the shape
// pkg/errors provides. A plain error yields "", which is correct — an invented
// stack would be worse than none.
func stackOf(err error) string {
	type stackTracer interface{ StackTrace() string }
	var st stackTracer
	if errors.As(err, &st) {
		return st.StackTrace()
	}
	return ""
}

// defaultLogger backs the package-level functions.
var defaultLogger = New(Options{Level: LevelInfo})

// Default returns the process-wide logger.
func Default() *Logger { return defaultLogger }

// SetDefault replaces the process-wide logger and returns the previous one, so
// tests can restore it.
func SetDefault(l *Logger) *Logger {
	prev := defaultLogger
	defaultLogger = l
	return prev
}

// Debug logs to the default logger.
func Debug(msg string, fields ...Field) { defaultLogger.Debug(msg, fields...) }

// Info logs to the default logger.
func Info(msg string, fields ...Field) { defaultLogger.Info(msg, fields...) }

// Warn logs to the default logger.
func Warn(msg string, fields ...Field) { defaultLogger.Warn(msg, fields...) }

// LogError records err on the default logger.
func LogError(err error, fields ...Field) { defaultLogger.Error(err, fields...) }

// RecentErrors returns the default logger's remembered errors.
func RecentErrors() []ErrorRecord { return defaultLogger.RecentErrors() }
