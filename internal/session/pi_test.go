package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ultrac/internal/ai"
	"ultrac/internal/config"
)

// setupHomeDir points ULTRAC_CONFIG_DIR at a temp root so no test touches the
// real ~/.ultrac. It returns the home directory (settings and sessions live
// under it).
func setupHomeDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, ".ultrac")
	t.Setenv(config.EnvConfigDir, home)
	return home
}

// setupAgentDir is the pre-2026-10-16 name; kept for the tests that only care
// about the sessions root.
func setupAgentDir(t *testing.T) string { return setupHomeDir(t) }

// ---------------------------------------------------------------------------
// Encoding: the directory algorithm is the interop surface
// ---------------------------------------------------------------------------

func TestEncodeCWDMatchesPi(t *testing.T) {
	// pi: getDefaultSessionDirPath, session-manager.ts:589-594 —
	// strip ONE leading separator, then each of / \ : becomes '-', wrapped
	// in --. Note that a Windows drive path yields two hyphens after the
	// drive letter: "D:\repo" has both ':' and '\' between D and repo.
	//
	// The input is resolved to the host platform's absolute form first (pi's
	// resolvePath does the same), so the expectations below are written per
	// platform.
	var table []struct{ in, want string }
	if runtime.GOOS == "windows" {
		table = []struct{ in, want string }{
			{`D:\develop\Project\Ultrac`, `--D--develop-Project-Ultrac--`},
			{`D:\repo`, `--D--repo--`},
			{`D:\`, `--D----`},
			{`C:\a b\c`, `--C--a b-c--`},
		}
	} else {
		table = []struct{ in, want string }{
			{`/home/me/repo`, `--home-me-repo--`},
			{`/repo/`, `--repo---`},
			{`/`, `----`},
		}
	}
	for _, c := range table {
		if got := EncodeCWD(c.in); got != c.want {
			t.Errorf("EncodeCWD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSessionDirForSitsUnderSessionsDir(t *testing.T) {
	setupAgentDir(t)
	dir := SessionDirFor(`D:\repo`)
	if !strings.HasPrefix(dir, config.SessionsDir()) {
		t.Fatalf("SessionDirFor = %q, not under %q", dir, config.SessionsDir())
	}
	if filepath.Base(dir) != `--D--repo--` {
		t.Errorf("encoded component = %q", filepath.Base(dir))
	}
}

func TestSessionFileName(t *testing.T) {
	// pi: timestamp.replace(/[:.]/g, "-") + "_" + id + ".jsonl".
	got := SessionFileName("2026-10-15T08:40:50.123Z", "0197abc")
	want := "2026-10-15T08-40-50-123Z_0197abc.jsonl"
	if got != want {
		t.Fatalf("SessionFileName = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

func TestNewSessionIDIsOrderedUUIDv7(t *testing.T) {
	var prev int64
	for i := 0; i < 200; i++ {
		id := NewSessionID()
		if !ValidSessionID(id) {
			t.Fatalf("id %q fails pi's assertValidSessionId regex", id)
		}
		compact := strings.ReplaceAll(id, "-", "")
		if len(compact) != 32 {
			t.Fatalf("id %q is not a 128-bit uuid", id)
		}
		// Version nibble (13th hex char) must be 7; variant (17th) must be
		// 8/9/a/b (RFC 9562).
		if compact[12] != '7' {
			t.Errorf("id %q is not version 7", id)
		}
		if compact[16] < '8' || compact[16] > 'b' {
			t.Errorf("id %q has variant %c", id, compact[16])
		}
		var ms int64
		for j := 0; j < 12; j++ {
			ms = ms<<4 | int64(hexVal(compact[j]))
		}
		if ms < prev {
			t.Fatalf("uuidv7 clock went backwards: %d after %d", ms, prev)
		}
		prev = ms
	}
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func TestNewEntryIDIsShortHexAndUnique(t *testing.T) {
	taken := map[string]bool{}
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id := newEntryID(taken)
		if len(id) != 8 {
			t.Fatalf("entry id %q is not 8 hex chars", id)
		}
		for j := 0; j < len(id); j++ {
			if !isHexByte(id[j]) {
				t.Fatalf("entry id %q is not hex", id)
			}
		}
		if seen[id] {
			t.Fatalf("entry id %q repeated", id)
		}
		seen[id] = true
		taken[id] = true
	}
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// ---------------------------------------------------------------------------
// Line format
// ---------------------------------------------------------------------------

func TestMarshalAndParseRoundTrip(t *testing.T) {
	parent := "aa11bb22"
	e := &SessionEntry{
		Type:      TypeMessage,
		ID:        "cc33dd44",
		ParentID:  &parent,
		Timestamp: "2026-10-15T08:40:50.123Z",
		Message:   json.RawMessage(`{"role":"user","content":"hi","timestamp":1}`),
		Extra: map[string]json.RawMessage{
			"customField": json.RawMessage(`{"nested":[1,2]}`),
		},
	}
	line, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parsed.(*SessionEntry)
	if !ok {
		t.Fatalf("parsed %T, want *SessionEntry", parsed)
	}
	if got.Type != TypeMessage || got.ID != e.ID || got.ParentID == nil || *got.ParentID != parent {
		t.Errorf("core fields lost: %+v", got)
	}
	if string(got.Message) != string(e.Message) {
		t.Errorf("message raw changed: %s", got.Message)
	}
	raw, ok := got.Extra["customField"]
	if !ok || string(raw) != `{"nested":[1,2]}` {
		t.Errorf("Extra not preserved: %v", got.Extra)
	}
	// A second marshal of the parsed entry must be byte-identical.
	again, err := MarshalEntry(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(line) {
		t.Errorf("re-marshal differs:\n%s\n%s", line, again)
	}
}

func TestRootEntryParentIdIsNull(t *testing.T) {
	e := &SessionEntry{Type: TypeMessage, ID: "01234567", Timestamp: "2026-10-15T08:40:50.123Z"}
	line, _ := MarshalEntry(e)
	if !strings.Contains(string(line), `"parentId":null`) {
		t.Errorf("root entry must carry parentId null (pi's SessionEntryBase): %s", line)
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := NewHeader("", `D:\repo`, "")
	line, err := MarshalHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(line), `{"type":"session","version":3,"id":"`) {
		t.Errorf("header shape: %s", line)
	}
	parsed, err := ParseLine(line)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parsed.(*Header)
	if !ok {
		t.Fatalf("parsed %T, want *Header", parsed)
	}
	if got.Version != CurrentSessionVersion || got.CWD != `D:\repo` || got.ID != h.ID {
		t.Errorf("header = %+v", got)
	}
	if strings.Contains(string(line), "parentId") {
		t.Errorf("header must not carry entry fields: %s", line)
	}
}

func TestParseLineTolerance(t *testing.T) {
	for _, line := range []string{"", "   ", "not json", "[1,2,3]", `{"type":"session"}`} {
		got, err := ParseLine([]byte(line))
		if err != nil {
			t.Errorf("ParseLine(%q) errored: %v", line, err)
		}
		if got != nil {
			t.Errorf("ParseLine(%q) = %v, want nil", line, got)
		}
	}
}

// ---------------------------------------------------------------------------
// SessionManager: lazy flush, append-only, rebuild
// ---------------------------------------------------------------------------

func userMsg(text string) ai.Message {
	return ai.NewTextMessage(ai.RoleUser, text)
}

func assistantMsg(text string) ai.Message {
	m := ai.NewTextMessage(ai.RoleAssistant, text)
	m.API = "anthropic-messages"
	m.Provider = "anthropic"
	m.Model = "claude-test"
	m.StopReason = ai.StopStop
	usage := ai.Usage{Input: 1, Output: 2, TotalTokens: 3}
	m.Usage = &usage
	return m
}

func TestLazyFlushLeavesNoFileWithoutConversation(t *testing.T) {
	agent := setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendModelChange("anthropic", "claude-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendThinkingLevelChange("off"); err != nil {
		t.Fatal(err)
	}
	// Setup entries alone stay in memory: opening and closing without
	// chatting leaves no file behind (pi's _persist rule, #10000).
	if _, err := os.Stat(m.SessionFile()); !os.IsNotExist(err) {
		t.Fatalf("session file exists after setup-only appends: %v", err)
	}
	if m.SessionID() == "" {
		t.Fatal("session id empty")
	}
	if !strings.HasPrefix(m.SessionDir(), agent) {
		t.Errorf("session dir %q not under agent dir %q", m.SessionDir(), agent)
	}
	if filepath.Base(m.SessionDir()) != `--D--repo--` {
		t.Errorf("session dir = %q", m.SessionDir())
	}

	// The first user message flushes the header and every entry so far.
	if _, err := m.AppendMessage(userMsg("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.SessionFile())
	if err != nil {
		t.Fatalf("session file not created on first user message: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("file has %d lines, want header + model_change + thinking + user", len(lines))
	}
	var header Header
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatal(err)
	}
	if header.Type != "session" || header.Version != 3 || header.CWD != `D:\repo` {
		t.Errorf("header = %+v", header)
	}

	// After the flush, each append is exactly one more line (append-only).
	before, _ := os.ReadFile(m.SessionFile())
	if _, err := m.AppendMessage(assistantMsg("hi")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(m.SessionFile())
	if !strings.HasPrefix(string(after), string(before)) {
		t.Error("append rewrote earlier bytes; the file must be append-only")
	}
	if got, want := strings.Count(string(after), "\n"), strings.Count(string(before), "\n")+1; got != want {
		t.Errorf("line count %d, want %d", got, want)
	}
}

func TestRebuildContextFromOwnFile(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendModelChange("anthropic", "claude-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("q1")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(assistantMsg("a1")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendSessionInfo("my session"); err != nil {
		t.Fatal(err)
	}

	opened, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	if opened.SessionID() != m.SessionID() {
		t.Errorf("session id %q != %q", opened.SessionID(), m.SessionID())
	}
	if got := opened.SessionName(); got != "my session" {
		t.Errorf("session name = %q", got)
	}
	ctx := opened.BuildSessionContext()
	if len(ctx.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (session_info is state, not context)", len(ctx.Messages))
	}
	if ctx.Messages[0].Role != ai.RoleUser || ai.MessageText(ctx.Messages[0]) != "q1" {
		t.Errorf("message[0] = %+v", ctx.Messages[0])
	}
	if ctx.Messages[1].Role != ai.RoleAssistant || ctx.Messages[1].Provider != "anthropic" {
		t.Errorf("message[1] = %+v", ctx.Messages[1])
	}
	if ctx.ThinkingLevel != "off" {
		t.Errorf("thinking level = %q", ctx.ThinkingLevel)
	}
	if ctx.Model == nil || ctx.Model.ModelID != "claude-test" {
		t.Errorf("model = %+v", ctx.Model)
	}
}

func TestInMemorySessionNeverTouchesDisk(t *testing.T) {
	setupAgentDir(t)
	m := InMemorySession(`D:\repo`)
	if _, err := m.AppendMessage(userMsg("only in ram")); err != nil {
		t.Fatal(err)
	}
	if f := m.SessionFile(); f != "" {
		t.Errorf("in-memory session has a file: %q", f)
	}
	ctx := m.BuildSessionContext()
	if len(ctx.Messages) != 1 {
		t.Fatalf("messages = %d", len(ctx.Messages))
	}
}

// ---------------------------------------------------------------------------
// Reading pi's files: the other half of the interop DoD
// ---------------------------------------------------------------------------

// piSessionFixture is a session file transcribed from pi's writer semantics
// (session-manager.ts): header, a leading system message carrying the prompt
// sections, a model_change, one user/assistant pair, a session_info, and then
// a user message that branches off the assistant — the rewind shape, where
// the abandoned branch (session_info's child in file order) is NOT on the
// leaf's parent chain.
const piSessionFixture = `{"type":"session","version":3,"id":"0197a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","timestamp":"2026-10-14T09:00:00.000Z","cwd":"/repo"}
{"type":"message","id":"aa11bb22","parentId":null,"timestamp":"2026-10-14T09:00:00.100Z","message":{"role":"system","content":"","sections":{"preamble":"You are an expert coding assistant.","tools":"<tools>\n- read: Read file contents\n</tools>"},"timestamp":0}}
{"type":"model_change","id":"cc33dd44","parentId":"aa11bb22","timestamp":"2026-10-14T09:00:00.200Z","provider":"anthropic","modelId":"claude-sonnet-4-5"}
{"type":"message","id":"ee55ff66","parentId":"cc33dd44","timestamp":"2026-10-14T09:00:01.000Z","message":{"role":"user","content":"first question","timestamp":1}}
{"type":"message","id":"07778899","parentId":"ee55ff66","timestamp":"2026-10-14T09:00:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"first answer"}],"api":"anthropic-messages","provider":"anthropic","model":"claude-sonnet-4-5","usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":2}}
{"type":"session_info","id":"0a0b0c0d","parentId":"07778899","timestamp":"2026-10-14T09:00:03.000Z","name":"titled session"}
{"type":"message","id":"11223344","parentId":"07778899","timestamp":"2026-10-14T09:00:04.000Z","message":{"role":"user","content":"rewound question","timestamp":3}}
`

func TestReadsPiWrittenSessionFile(t *testing.T) {
	setupAgentDir(t)
	path := filepath.Join(t.TempDir(), "2026-10-14T09-00-00-000Z_0197a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b.jsonl")
	if err := os.WriteFile(path, []byte(piSessionFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}

	// The header drives cwd and id.
	if m.SessionID() != "0197a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" {
		t.Errorf("id = %q", m.SessionID())
	}
	if m.CWD() != "/repo" {
		t.Errorf("cwd = %q", m.CWD())
	}
	if got := m.SessionName(); got != "titled session" {
		t.Errorf("name = %q", got)
	}

	// The walk follows parentId from the leaf, not file order: session_info
	// sits between the assistant and the rewind in the file, and the rewind's
	// parent is the assistant — so the chain is system, model_change, user,
	// assistant, user(rewound), with session_info dropped from context.
	branch := m.GetBranch("")
	ids := make([]string, len(branch))
	for i, e := range branch {
		ids[i] = e.ID
	}
	want := []string{"aa11bb22", "cc33dd44", "ee55ff66", "07778899", "11223344"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("branch = %v, want %v", ids, want)
	}

	ctx := m.BuildSessionContext()
	if len(ctx.Messages) != 4 {
		t.Fatalf("messages = %d, want system+user+assistant+rewound", len(ctx.Messages))
	}
	if ctx.Messages[0].Role != ai.RoleSystem {
		t.Errorf("messages[0] = %s, want the leading system message", ctx.Messages[0].Role)
	}
	sections := ctx.Messages[0].Sections
	if sections == nil || sections["preamble"] == nil || *sections["preamble"] != "You are an expert coding assistant." {
		t.Errorf("system sections = %v", sections)
	}
	if ai.MessageText(ctx.Messages[1]) != "first question" {
		t.Errorf("messages[1] = %q", ai.MessageText(ctx.Messages[1]))
	}
	if ai.MessageText(ctx.Messages[3]) != "rewound question" {
		t.Errorf("messages[3] = %q", ai.MessageText(ctx.Messages[3]))
	}
	if ctx.Model == nil || ctx.Model.ModelID != "claude-sonnet-4-5" {
		t.Errorf("model = %+v", ctx.Model)
	}

	// Appending continues the chain from the leaf, and the file grows by one
	// line only.
	before, _ := os.ReadFile(path)
	if _, err := m.AppendMessage(userMsg("after resume")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(after), string(before)) {
		t.Error("resumed append rewrote history")
	}
	ctx = m.BuildSessionContext()
	if ai.MessageText(ctx.Messages[len(ctx.Messages)-1]) != "after resume" {
		t.Errorf("last message = %q", ai.MessageText(ctx.Messages[len(ctx.Messages)-1]))
	}
}

// piCompactionFixture exercises the compaction window: the summary entry
// carries firstKeptEntryId pointing at an entry before it, so the earlier
// user message drops out of context and the entry's captured systemMessage
// leads.
const piCompactionFixture = `{"type":"session","version":3,"id":"0197bbbb-0000-7000-8000-000000000001","timestamp":"2026-10-14T10:00:00.000Z","cwd":"/repo"}
{"type":"message","id":"00112233","parentId":null,"timestamp":"2026-10-14T10:00:00.100Z","message":{"role":"user","content":"dropped by compaction","timestamp":1}}
{"type":"message","id":"44556677","parentId":"00112233","timestamp":"2026-10-14T10:00:01.000Z","message":{"role":"user","content":"kept prefix","timestamp":2}}
{"type":"compaction","id":"8899aabb","parentId":"44556677","timestamp":"2026-10-14T10:00:02.000Z","summary":"the summary","firstKeptEntryId":"44556677","tokensBefore":4242,"systemMessage":{"role":"system","content":"","sections":{"preamble":"prompt at compaction"},"timestamp":9}}
{"type":"message","id":"ccddeeff","parentId":"8899aabb","timestamp":"2026-10-14T10:00:03.000Z","message":{"role":"user","content":"after compaction","timestamp":3}}
`

func TestReadsPiCompactedSessionFile(t *testing.T) {
	setupAgentDir(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(piCompactionFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := m.BuildSessionContext()
	if len(ctx.Messages) != 4 {
		t.Fatalf("messages = %d, want [system, summary, kept, after]", len(ctx.Messages))
	}
	if ctx.Messages[0].Role != ai.RoleSystem {
		t.Errorf("messages[0] = %s, want the compaction's captured system message", ctx.Messages[0].Role)
	}
	if v := ctx.Messages[0].Sections["preamble"]; v == nil || *v != "prompt at compaction" {
		t.Errorf("captured sections = %v", ctx.Messages[0].Sections)
	}
	if !strings.HasPrefix(ai.MessageText(ctx.Messages[1]), compactionSummaryPrefix) ||
		!strings.HasSuffix(ai.MessageText(ctx.Messages[1]), compactionSummarySuffix) {
		t.Errorf("summary framing wrong: %q", ai.MessageText(ctx.Messages[1]))
	}
	if ai.MessageText(ctx.Messages[2]) != "kept prefix" {
		t.Errorf("messages[2] = %q", ai.MessageText(ctx.Messages[2]))
	}
	if ai.MessageText(ctx.Messages[3]) != "after compaction" {
		t.Errorf("messages[3] = %q", ai.MessageText(ctx.Messages[3]))
	}
}

func TestUnknownEntryTypesRoundTripThroughExtra(t *testing.T) {
	setupAgentDir(t)
	// A label entry — read, preserved, re-written verbatim; this version
	// never appends one, but a file pi labelled must survive a round trip.
	path := filepath.Join(t.TempDir(), "s.jsonl")
	fixture := `{"type":"session","version":3,"id":"0197cccc-0000-7000-8000-000000000002","timestamp":"2026-10-14T11:00:00.000Z","cwd":"/repo"}
{"type":"message","id":"00112233","parentId":null,"timestamp":"2026-10-14T11:00:00.100Z","message":{"role":"user","content":"hi","timestamp":1}}
{"type":"label","id":"0badf00d","parentId":"00112233","timestamp":"2026-10-14T11:00:01.000Z","targetId":"00112233","label":"bookmark"}
`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := m.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	label := entries[1]
	if label.Type != TypeLabel || label.ExtraString("targetId") != "00112233" || label.ExtraString("label") != "bookmark" {
		t.Fatalf("label entry = %+v", label)
	}
	// The projection ignores it (labels are UI state).
	if len(m.BuildSessionContext().Messages) != 1 {
		t.Errorf("label must not enter context")
	}
	// Re-marshalling keeps the unknown fields.
	line, err := MarshalEntry(&label)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"targetId":"00112233"`, `"label":"bookmark"`, `"type":"label"`} {
		if !strings.Contains(string(line), want) {
			t.Errorf("re-marshal lost %s: %s", want, line)
		}
	}
}

func TestOpenRefusesNonSessionFile(t *testing.T) {
	setupAgentDir(t)
	path := filepath.Join(t.TempDir(), "not-a-session.jsonl")
	if err := os.WriteFile(path, []byte(`{"hello":"world"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSession(path); err == nil {
		t.Fatal("opening a non-session file must fail, not overwrite it")
	}
}

func TestReadToleratesMalformedMiddleLine(t *testing.T) {
	setupAgentDir(t)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	content := `{"type":"session","version":3,"id":"0197dddd-0000-7000-8000-000000000003","timestamp":"2026-10-14T12:00:00.000Z","cwd":"/repo"}
{"type":"message","id":"00112233","parentId":null,"timestamp":"2026-10-14T12:00:00.100Z","message":{"role":"user","content":"hi","timestamp":1}}
{"type":"message","id":"broken","parentId":"00112233","timestamp":"x","mess
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := OpenSession(path)
	if err != nil {
		t.Fatalf("a truncated last line must not fail the load: %v", err)
	}
	if len(m.BuildSessionContext().Messages) != 1 {
		t.Errorf("messages = %d, want 1", len(m.BuildSessionContext().Messages))
	}
}

func TestContinueRecentSessionPicksNewest(t *testing.T) {
	agent := setupAgentDir(t)
	dir := SessionDirFor(`D:\repo`)

	old := CreateSession(`D:\repo`)
	if _, err := old.AppendMessage(userMsg("old")); err != nil {
		t.Fatal(err)
	}
	// Distinguish by mtime explicitly: Windows' system clock granularity
	// makes two back-to-back sessions' mtimes unreliable.
	stamp := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old.SessionFile(), stamp, stamp); err != nil {
		t.Fatal(err)
	}

	recent := CreateSession(`D:\repo`)
	if _, err := recent.AppendMessage(userMsg("new")); err != nil {
		t.Fatal(err)
	}

	m, err := ContinueRecentSession(`D:\repo`)
	if err != nil {
		t.Fatal(err)
	}
	if m.SessionID() != recent.SessionID() {
		t.Fatalf("continued session %q, want the newer %q", m.SessionID(), recent.SessionID())
	}
	// A different cwd must not see this project's sessions.
	if _, err := os.Stat(filepath.Join(agent, "sessions")); err != nil {
		t.Fatalf("sessions dir missing: %v", err)
	}
	other, err := ContinueRecentSession(`D:\other`)
	if err != nil {
		t.Fatal(err)
	}
	if other.SessionID() == recent.SessionID() {
		t.Error("a session from another cwd was continued")
	}
	if filepath.Base(dir) != `--D--repo--` {
		t.Errorf("dir = %q", dir)
	}
}

func TestMigrateV1EntriesGetIDs(t *testing.T) {
	// A v1 file: the header has an id but no version, and entries carry no
	// id/parentId. Migration synthesises both (pi's migrateV1ToV2,
	// session-manager.ts:287-313) — in memory only, the file is untouched.
	path := filepath.Join(t.TempDir(), "s.jsonl")
	fixture := `{"type":"session","id":"0197eeee-0000-7000-8000-000000000009","timestamp":"2026-10-14T13:00:00.000Z","cwd":"/repo"}
{"type":"message","timestamp":"2026-10-14T13:00:00.100Z","message":{"role":"user","content":"hi","timestamp":1}}
{"type":"message","timestamp":"2026-10-14T13:00:01.000Z","message":{"role":"user","content":"there","timestamp":2}}
`
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	entries := m.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d", len(entries))
	}
	if entries[0].ID == "" || entries[1].ID == "" {
		t.Fatal("migration did not assign ids")
	}
	if entries[1].ParentID == nil || *entries[1].ParentID != entries[0].ID {
		t.Errorf("migration did not chain parentIds: %+v", entries[1])
	}
	if len(m.BuildSessionContext().Messages) != 2 {
		t.Errorf("messages = %d, want 2", len(m.BuildSessionContext().Messages))
	}
}

func TestGoWrittenFileMatchesPiRules(t *testing.T) {
	// The write half of the interop DoD: the bytes this package appends must
	// satisfy every rule pi's reader applies.
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	if _, err := m.AppendModelChange("anthropic", "claude-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("prompt")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(assistantMsg("answer")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d", len(lines))
	}

	// Every line is a JSON object with a string "type".
	seen := map[string]bool{}
	for i, line := range lines {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i, err)
		}
		var typ string
		if err := json.Unmarshal(obj["type"], &typ); err != nil || typ == "" {
			t.Fatalf("line %d has no type", i)
		}
		if i == 0 {
			if typ != "session" {
				t.Fatalf("first line type = %q, want session", typ)
			}
			continue
		}
		// pi's SessionEntryBase: id, parentId, timestamp always present.
		for _, key := range []string{"id", "parentId", "timestamp"} {
			if _, ok := obj[key]; !ok {
				t.Errorf("line %d (%s) is missing %q", i, typ, key)
			}
		}
		var id string
		_ = json.Unmarshal(obj["id"], &id)
		if len(id) != 8 {
			t.Errorf("entry id %q is not 8 chars", id)
		}
		if seen[id] {
			t.Errorf("entry id %q repeated", id)
		}
		seen[id] = true
		if typ == TypeMessage {
			if _, ok := obj["message"]; !ok {
				t.Errorf("message entry %q has no message", id)
			}
		}
	}

	// The parent chain: first entry is the root (null), each later entry
	// points at its predecessor in file order.
	if raw := string(mustField(t, lines[1], "parentId")); raw != "null" {
		t.Errorf("first entry parentId = %q, want null", raw)
	}
}

func mustField(t *testing.T, line, key string) json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		t.Fatal(err)
	}
	raw, ok := obj[key]
	if !ok {
		t.Fatalf("line has no %q: %s", key, line)
	}
	return raw
}

func TestEntryMessageDecodesAiShape(t *testing.T) {
	e := &SessionEntry{
		Type:    TypeMessage,
		Message: json.RawMessage(`{"role":"toolResult","toolCallId":"t1","toolName":"read","content":[{"type":"text","text":"out"}],"isError":true,"timestamp":7}`),
	}
	msg, ok := EntryMessage(e)
	if !ok {
		t.Fatal("EntryMessage failed")
	}
	if msg.Role != ai.RoleToolResult || msg.ToolCallID != "t1" || !msg.IsError {
		t.Errorf("msg = %+v", msg)
	}
	if msg.TimestampMS != 7 {
		t.Errorf("timestamp = %d", msg.TimestampMS)
	}
	// A garbage message body is dropped, not half-decoded.
	bad := &SessionEntry{Type: TypeMessage, Message: json.RawMessage(`{"role":"nonsense"}`)}
	if _, ok := EntryMessage(bad); ok {
		t.Error("unknown role must not decode")
	}
}

func TestAppendMessageRoundTripsAssistantFields(t *testing.T) {
	setupAgentDir(t)
	m := CreateSession(`D:\repo`)
	thinking := ai.Message{
		Role: ai.RoleAssistant,
		Content: []ai.ContentBlock{
			ai.ThinkingBlock("deliberation", "sig-123", false),
			ai.ToolCallBlock("call-1", "read", map[string]any{"path": "go.mod"}),
		},
		API: "anthropic-messages", Provider: "anthropic", Model: "claude-test",
		StopReason: ai.StopToolUse,
		Usage:      &ai.Usage{Input: 3, Output: 4, TotalTokens: 7},
	}
	if _, err := m.AppendMessage(thinking); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AppendMessage(userMsg("u")); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenSession(m.SessionFile())
	if err != nil {
		t.Fatal(err)
	}
	msgs := opened.BuildSessionContext().Messages
	if len(msgs) != 2 {
		t.Fatalf("messages = %d", len(msgs))
	}
	got := msgs[0]
	if len(got.Content) != 2 {
		t.Fatalf("blocks = %d", len(got.Content))
	}
	if got.Content[0].Type != ai.BlockThinking || got.Content[0].ThinkingSignature != "sig-123" {
		t.Errorf("thinking block lost its signature: %+v", got.Content[0])
	}
	if got.Content[1].Type != ai.BlockToolCall || got.Content[1].Name != "read" {
		t.Errorf("toolcall block = %+v", got.Content[1])
	}
	if got.StopReason != ai.StopToolUse || got.Usage == nil || got.Usage.Input != 3 {
		t.Errorf("assistant fields = %+v usage=%+v", got.StopReason, got.Usage)
	}
}

func TestBuildContextFromBareEntries(t *testing.T) {
	// The package-level projection is what a resumed file or a test uses
	// without a manager. The chain matters: model_change is the root, the
	// thinking change hangs off it, the message off that.
	setupAgentDir(t)
	entries := []*SessionEntry{
		{Type: TypeModelChange, ID: "00112233", Provider: "p", ModelID: "m"},
		{Type: TypeThinkingLevelChange, ID: "44556677", ParentID: strPtr("00112233"), ThinkingLevel: "high"},
		{Type: TypeMessage, ID: "778899aa", ParentID: strPtr("44556677"),
			Message: json.RawMessage(`{"role":"user","content":"hi","timestamp":1}`)},
	}
	ctx := buildContext(entries, "778899aa", indexEntries(entries))
	if len(ctx.Messages) != 1 || ctx.ThinkingLevel != "high" {
		t.Fatalf("ctx = %+v", ctx)
	}
	if ctx.Model == nil || ctx.Model.Provider != "p" {
		t.Errorf("model = %+v", ctx.Model)
	}
}

func indexEntries(entries []*SessionEntry) map[string]*SessionEntry {
	out := make(map[string]*SessionEntry, len(entries))
	for _, e := range entries {
		out[e.ID] = e
	}
	return out
}

func strPtr(s string) *string { return &s }

func TestCreateSessionWithParentRecordsParent(t *testing.T) {
	setupAgentDir(t)
	parent := CreateSession(`D:/repo`)
	if _, err := parent.AppendMessage(userMsg("parent turn")); err != nil {
		t.Fatal(err)
	}

	// P6's RPC new_session command: a child session whose header names the
	// parent it grew from (pi: agent-session-runtime.ts:239-243).
	child := CreateSessionWithParent(`D:/repo`, parent.SessionID())
	if child.SessionID() == parent.SessionID() {
		t.Fatal("the child must be its own session")
	}
	if child.Header().ParentSession != parent.SessionID() {
		t.Errorf("ParentSession = %q, want %q", child.Header().ParentSession, parent.SessionID())
	}

	// The parent survives the child's creation untouched, and the two are
	// distinct files.
	if parent.Header().ParentSession != "" {
		t.Errorf("the parent's header must not gain a parent: %q", parent.Header().ParentSession)
	}
	if child.SessionFile() == parent.SessionFile() {
		t.Error("parent and child must not share a file")
	}

	// An empty parent behaves exactly like CreateSession (no field on the
	// wire, not an empty string).
	plain := CreateSessionWithParent(`D:/repo`, "")
	if plain.Header().ParentSession != "" {
		t.Errorf("ParentSession = %q, want empty", plain.Header().ParentSession)
	}
}
