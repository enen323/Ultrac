package main

// print_test.go — the P5 print-mode acceptance, end to end against a fake
// provider.
//
// The plan's P2 gate ("no real API calls in tests") carries into P5: the
// wiring under test is everything around the stream — settings resolution,
// session persistence, the event surfaces, the exit codes — and a fake
// StreamFn is what makes those deterministic. The real endpoint is
// exercised by `ultrac probe` and the manual acceptance run, not here.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ultrac/internal/ai"
	"ultrac/internal/prompt"
	"ultrac/internal/session"
)

// scriptProvider serves one preset event script per call, like the P2
// fakes. It records the transcripts it was handed so the tests can assert
// what the loop actually sent.
type scriptProvider struct {
	mu       sync.Mutex
	scripts  [][]ai.AssistantEvent
	contexts []ai.TranscriptContext
}

func (p *scriptProvider) streamFn(_ context.Context, model ai.Model, tc ai.TranscriptContext, _ ai.StreamOptions) ai.Stream {
	p.mu.Lock()
	idx := len(p.contexts)
	p.contexts = append(p.contexts, tc)
	var script []ai.AssistantEvent
	if idx < len(p.scripts) {
		script = p.scripts[idx]
	}
	p.mu.Unlock()
	if script == nil {
		s := ai.NewEventStream()
		s.Push(ai.EvError{
			Reason: ai.StopError,
			Error: ai.Message{
				Role:         ai.RoleAssistant,
				API:          string(model.API),
				Provider:     string(model.Provider),
				Model:        model.ID,
				StopReason:   ai.StopError,
				ErrorMessage: "scriptProvider: no script for this call",
			},
		})
		s.End()
		return s
	}
	return ai.StreamOf(script...)
}

func (p *scriptProvider) transcript(i int) []ai.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.contexts) {
		return nil
	}
	return p.contexts[i].Messages
}

// assistantScript builds a plain text response. Every update event carries
// the partial-so-far, the way the real adapters do: the loop reads the
// partial off the event (loop.go:393-401), so a script without one is not a
// valid script.
func assistantScript(text string, stop ai.StopReason) []ai.AssistantEvent {
	partial := func(t string) ai.Message {
		m := ai.Message{Role: ai.RoleAssistant}
		if t != "" {
			m.Content = []ai.ContentBlock{ai.TextBlock(t)}
		}
		return m
	}
	final := ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.ContentBlock{ai.TextBlock(text)},
		API:        "anthropic-messages",
		Provider:   "deepseek",
		Model:      "deepseek-chat",
		StopReason: stop,
		Usage:      &ai.Usage{Input: 10, Output: 4, TotalTokens: 14},
	}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: partial("")},
		ai.EvTextStart{ContentIndex: 0, Partial: partial("")},
		ai.EvTextDelta{ContentIndex: 0, Delta: text, Partial: partial(text)},
		ai.EvTextEnd{ContentIndex: 0, Content: text, Partial: partial(text)},
		ai.EvDone{Reason: stop, Message: final},
	}
}

// toolThenTextScript builds a tool-call turn; the tool result comes from the
// real tool registry, so the second script is the model's answer to it.
func toolThenTextScript(callID, toolName string, args map[string]any, answer string) []ai.AssistantEvent {
	call := ai.ToolCallBlock(callID, toolName, args)
	partial := ai.Message{
		Role:    ai.RoleAssistant,
		Content: []ai.ContentBlock{call},
	}
	final := ai.Message{
		Role:       ai.RoleAssistant,
		Content:    []ai.ContentBlock{call},
		API:        "anthropic-messages",
		Provider:   "deepseek",
		Model:      "deepseek-chat",
		StopReason: ai.StopToolUse,
		Usage:      &ai.Usage{Input: 10, Output: 4, TotalTokens: 14},
	}
	return []ai.AssistantEvent{
		ai.EvStart{Partial: ai.Message{Role: ai.RoleAssistant}},
		ai.EvToolCallStart{ContentIndex: 0, Partial: partial},
		ai.EvToolCallDelta{ContentIndex: 0, Delta: `{"path":`, Partial: partial},
		ai.EvToolCallEnd{ContentIndex: 0, ToolCall: call, Partial: partial},
		ai.EvDone{Reason: ai.StopToolUse, Message: final},
	}
}

// printEnv is one test's sandbox: a temp config home carrying a settings
// file, and a temp working directory with one file to read.
type printEnv struct {
	t        *testing.T
	cwd      string
	home     string
	flags    *agentFlags
	provider *scriptProvider
}

func newPrintEnv(t *testing.T) *printEnv {
	t.Helper()
	home := t.TempDir()
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "hello.txt"), []byte("hello from the sandbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The only place a key can live since settings-unify-plan.md D11: the
	// provider entry. Key-only, so the catalog supplies the baseUrl and
	// dialect, exactly as a user who only pasted a key would get.
	settings := `{
      "providers": {
        "deepseek": { "apiKey": "sk-test-token" }
      }
    }`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ULTRAC_CONFIG_DIR", home)
	env := &printEnv{
		t:        t,
		cwd:      cwd,
		home:     home,
		flags:    &agentFlags{Mode: "text", Model: "deepseek/deepseek-chat"},
		provider: &scriptProvider{},
	}
	env.flags.setPrompt("P5 prompt")
	return env
}

// run executes one print session and returns the exit code plus the captured
// streams.
func (e *printEnv) run() (int, string, string) {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	ps := &printSession{
		flags:    e.flags,
		stdout:   &stdout,
		stderr:   &stderr,
		cwd:      e.cwd,
		streamFn: e.provider.streamFn,
	}
	code := ps.run()
	return code, stdout.String(), stderr.String()
}

// sessionFiles returns the session files written for the sandbox cwd, newest
// first.
func (e *printEnv) sessionFiles() []string {
	e.t.Helper()
	dir := filepath.Join(e.home, "sessions", session.EncodeCWD(e.cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range entries {
		out = append(out, filepath.Join(dir, ent.Name()))
	}
	return out
}

func TestPrintTextModeAnswers(t *testing.T) {
	env := newPrintEnv(t)
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("P5-OK", ai.StopStop)}

	code, stdout, stderr := env.run()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stdout != "P5-OK\n" {
		t.Errorf("stdout = %q, want %q", stdout, "P5-OK\n")
	}
}

func TestPrintTextModeErrorExitCode(t *testing.T) {
	env := newPrintEnv(t)
	// A stream that opens and then fails: the assistant message ends in
	// error, which is exit 1 with the message on stderr (pi:
	// print-mode.ts:144-147).
	errMsg := ai.Message{
		Role:         ai.RoleAssistant,
		API:          "anthropic-messages",
		Provider:     "deepseek",
		Model:        "deepseek-chat",
		StopReason:   ai.StopError,
		ErrorMessage: "401 unauthorized",
		Usage:        &ai.Usage{},
	}
	env.provider.scripts = [][]ai.AssistantEvent{{
		ai.EvStart{Partial: errMsg},
		ai.EvError{Reason: ai.StopError, Error: errMsg},
	}}

	code, stdout, stderr := env.run()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "401 unauthorized") {
		t.Errorf("stderr = %q, want the error message", stderr)
	}
}

func TestPrintJSONModeEventStream(t *testing.T) {
	env := newPrintEnv(t)
	env.flags.Mode = "json"
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("P5-OK", ai.StopStop)}

	code, stdout, stderr := env.run()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}

	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("got %d lines, want at least 3 (header + events): %q", len(lines), stdout)
	}
	// The header opens the stream (pi: print-mode.ts:122-127).
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("line 0 is not JSON: %v", err)
	}
	if header.Type != "session" {
		t.Errorf("line 0 type = %q, want session", header.Type)
	}

	var types []string
	for _, line := range lines[1:] {
		var l struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("line is not JSON: %v (%q)", err, line)
		}
		types = append(types, l.Type)
	}
	want := []string{
		"agent_start",
		"turn_start",
		"message_start", "message_end", // system patch (sections)
		"message_start", "message_end", // user prompt
		"message_start",  // assistant
		"message_update", // text_start
		"message_update", // text_delta
		"message_update", // text_end
		"message_end",
		"turn_end",
		"agent_end",
		"agent_settled",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("event types =\n%s\nwant\n%s", strings.Join(types, "\n"), strings.Join(want, "\n"))
	}
}

func TestPrintJSONModeToolRoundTrip(t *testing.T) {
	env := newPrintEnv(t)
	env.flags.Mode = "json"
	env.provider.scripts = [][]ai.AssistantEvent{
		toolThenTextScript("call_1", "ls", map[string]any{"path": "."}, ""),
		assistantScript("P5-OK", ai.StopStop),
	}

	code, stdout, stderr := env.run()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, `"type":"tool_execution_start"`) {
		t.Error("json stream is missing tool_execution_start")
	}
	if !strings.Contains(stdout, `"type":"tool_execution_end"`) {
		t.Error("json stream is missing tool_execution_end")
	}
	if !strings.Contains(stdout, "hello.txt") {
		t.Error("the tool result (the ls listing) never reached the stream")
	}
	// The second request must carry the tool result, or the loop is not
	// feeding results back.
	transcript := env.provider.transcript(1)
	var sawToolResult bool
	for _, m := range transcript {
		if m.Role == ai.RoleToolResult && m.ToolCallID == "call_1" {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Error("the second request did not carry the tool result message")
	}
}

func TestPrintWritesSessionFile(t *testing.T) {
	env := newPrintEnv(t)
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("P5-OK", ai.StopStop)}

	if code, _, _ := env.run(); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	files := env.sessionFiles()
	if len(files) != 1 {
		t.Fatalf("got %d session files, want 1", len(files))
	}
	mgr, err := session.OpenSession(files[0])
	if err != nil {
		t.Fatalf("reopen session: %v", err)
	}
	entries := mgr.Entries()
	// header + model_change + thinking_level_change + system patch +
	// user + assistant (the header is not an entry).
	wantTypes := []string{
		session.TypeModelChange,
		session.TypeThinkingLevelChange,
		session.TypeMessage, // system patch
		session.TypeMessage, // user
		session.TypeMessage, // assistant
	}
	if len(entries) != len(wantTypes) {
		t.Fatalf("session has %d entries, want %d: %+v", len(entries), len(wantTypes), entries)
	}
	for i, want := range wantTypes {
		if entries[i].Type != want {
			t.Errorf("entry %d type = %q, want %q", i, entries[i].Type, want)
		}
	}
	// The system patch must carry the sections, and the user entry the
	// prompt — the two facts that make the file replayable.
	systemMsg, ok := session.EntryMessage(&entries[2])
	if !ok || len(systemMsg.Sections) == 0 {
		t.Errorf("system entry carries no sections: %+v", entries[2])
	}
	userMsg, ok := session.EntryMessage(&entries[3])
	if !ok || len(userMsg.Content) == 0 || userMsg.Content[0].Text != "P5 prompt" {
		t.Errorf("user entry = %+v", entries[3])
	}
	assistantMsg, ok := session.EntryMessage(&entries[4])
	if !ok || len(assistantMsg.Content) == 0 || assistantMsg.Content[0].Text != "P5-OK" {
		t.Errorf("assistant entry = %+v", entries[4])
	}

	// The rebuilt context must be replayable: system patch, user, assistant.
	ctx := mgr.BuildSessionContext()
	if len(ctx.Messages) != 3 {
		t.Fatalf("rebuilt context has %d messages, want 3", len(ctx.Messages))
	}
	if ctx.Messages[1].Role != ai.RoleUser {
		t.Errorf("context message 1 role = %q, want user", ctx.Messages[1].Role)
	}
}

func TestPrintContinueResumesSession(t *testing.T) {
	env := newPrintEnv(t)
	env.flags.setPrompt("first")
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("first answer", ai.StopStop)}
	if code, _, _ := env.run(); code != 0 {
		t.Fatal("first run failed")
	}

	// Second run continues the same session.
	env.flags.Continue = true
	env.flags.setPrompt("second")
	env.provider.scripts = append(env.provider.scripts, assistantScript("second answer", ai.StopStop))
	code, stdout, _ := env.run()
	if code != 0 {
		t.Fatalf("continue run exit = %d, want 0", code)
	}
	if stdout != "second answer\n" {
		t.Errorf("stdout = %q", stdout)
	}

	files := env.sessionFiles()
	if len(files) != 1 {
		t.Fatalf("continue created %d session files, want 1 (the same file appended to)", len(files))
	}
	// The second request must have seen the first exchange.
	transcript := env.provider.transcript(1)
	var sawFirstUser, sawFirstAnswer bool
	for _, m := range transcript {
		if m.Role == ai.RoleUser && len(m.Content) > 0 && m.Content[0].Text == "first" {
			sawFirstUser = true
		}
		if m.Role == ai.RoleAssistant && len(m.Content) > 0 && m.Content[0].Text == "first answer" {
			sawFirstAnswer = true
		}
	}
	if !sawFirstUser || !sawFirstAnswer {
		t.Error("the continued request did not carry the earlier exchange")
	}
	// After the first continue the tool declarations are re-announced once:
	// the sections patch never carried toolsAdded, so the resumed transcript
	// declares nothing and the loop inserts the declaration (pi's
	// declareToolChanges, agent-loop.ts:333-363). A third run must add
	// nothing — the declaration is now part of the transcript.
	env.provider.scripts = append(env.provider.scripts, assistantScript("third answer", ai.StopStop))
	env.flags.setPrompt("third")
	if code, _, _ := env.run(); code != 0 {
		t.Fatal("third run failed")
	}

	// Re-open: the third run appended to the same file.
	mgr, err := session.OpenSession(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var systemEntries, toolsDeclarations int
	for _, e := range mgr.Entries() {
		if e.Type != session.TypeMessage {
			continue
		}
		m, ok := session.EntryMessage(&e)
		if !ok || m.Role != ai.RoleSystem {
			continue
		}
		systemEntries++
		if len(m.ToolsAdded) > 0 {
			toolsDeclarations++
		}
	}
	if systemEntries != 2 {
		t.Errorf("session carries %d system messages, want 2 (sections patch + tool declaration)", systemEntries)
	}
	if toolsDeclarations != 1 {
		t.Errorf("session carries %d tool declarations, want 1", toolsDeclarations)
	}
}

func TestPrintNoCredential(t *testing.T) {
	env := newPrintEnv(t)
	// Strip the credential from the settings file.
	settings := `{"env": {"ANTHROPIC_MODEL": "deepseek-chat"}}`
	if err := os.WriteFile(filepath.Join(env.home, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("never", ai.StopStop)}

	code, _, stderr := env.run()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no credential") {
		t.Errorf("stderr = %q, want the credential guidance", stderr)
	}
	if len(env.provider.contexts) != 0 {
		t.Error("no request should have been made without a credential")
	}
}

func TestPrintNoModel(t *testing.T) {
	env := newPrintEnv(t)
	settings := `{"env": {"ANTHROPIC_AUTH_TOKEN": "sk-test"}}`
	if err := os.WriteFile(filepath.Join(env.home, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	env.flags.Model = ""
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("never", ai.StopStop)}

	code, _, stderr := env.run()
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (usage error)", code)
	}
	if !strings.Contains(stderr, "no model") {
		t.Errorf("stderr = %q, want the model guidance", stderr)
	}
}

func TestPrintCwdFlagPicksTheDirectory(t *testing.T) {
	env := newPrintEnv(t)
	// The tools resolve against the --cwd directory, not the process cwd:
	// the file to read lives in the sandbox cwd, which is NOT where the
	// test process runs.
	env.flags.CWD = env.cwd
	env.provider.scripts = [][]ai.AssistantEvent{
		toolThenTextScript("call_1", "read", map[string]any{"path": "hello.txt"}, ""),
		assistantScript("P5-OK", ai.StopStop),
	}

	code, stdout, stderr := env.run()
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if stdout != "P5-OK\n" {
		t.Errorf("stdout = %q", stdout)
	}
	// The session must be filed under the --cwd directory, not the real
	// process cwd: two directories must not share one session folder.
	dir := filepath.Join(env.home, "sessions", session.EncodeCWD(env.cwd))
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("no session directory for the --cwd directory: %v", err)
	}
}

func TestPrintCwdFlagRejectsNonDirectory(t *testing.T) {
	env := newPrintEnv(t)
	env.flags.CWD = filepath.Join(env.cwd, "hello.txt") // a file
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("never", ai.StopStop)}

	var stdout, stderr bytes.Buffer
	// runPrint is the layer that resolves --cwd; call it directly. It fails
	// before any session or provider work, so the real registry is never
	// reached.
	code := runPrint(env.flags, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a non-directory --cwd", code)
	}
	if !strings.Contains(stderr.String(), "not a directory") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if len(env.provider.contexts) != 0 {
		t.Error("no request should have been made")
	}
}

func TestPrintNoTranscriptSkipsSessionFile(t *testing.T) {
	env := newPrintEnv(t)
	env.flags.NoTranscript = true
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("P5-OK", ai.StopStop)}

	code, stdout, _ := env.run()
	if code != 0 || stdout != "P5-OK\n" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if files := env.sessionFiles(); len(files) != 0 {
		t.Errorf("session files written despite --no-transcript: %v", files)
	}
}

func TestPrintPromptCarriesSystemSections(t *testing.T) {
	env := newPrintEnv(t)
	env.provider.scripts = [][]ai.AssistantEvent{assistantScript("P5-OK", ai.StopStop)}
	if code, _, _ := env.run(); code != 0 {
		t.Fatal("run failed")
	}
	// The first request's transcript must open with the system messages:
	// the tool-declaring seed and the sections patch, and the sections must
	// name every registered tool.
	transcript := env.provider.transcript(0)
	if len(transcript) < 2 || transcript[0].Role != ai.RoleSystem || transcript[1].Role != ai.RoleSystem {
		t.Fatalf("transcript does not open with two system messages: %+v", transcript)
	}
	if len(transcript[1].Sections) == 0 {
		t.Fatal("the second system message carries no sections")
	}
	if _, ok := transcript[1].Sections[prompt.SectionTools]; !ok {
		t.Errorf("sections = %v, want a tools section", transcript[1].Sections)
	}
}
