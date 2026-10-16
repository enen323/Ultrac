package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStoreAt(t.TempDir())
	t.Cleanup(func() { s.Close() })
	return s
}

// realTranscriptLine is a verbatim line from a transcript written by the
// TypeScript implementation. Fixtures like this are the only real check that
// the Go reader and the TS writer agree, and they are the reason the shape is
// dictated by the files rather than designed.
const realTranscriptLine = `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"reply with exactly: PROVIDER-OK"},"uuid":"5e9e5724-e6bf-4417-80da-75cbd770b8b9","timestamp":"2026-09-29T01:23:14.164Z","permissionMode":"default","userType":"external","entrypoint":"sdk-cli","cwd":"D:\\develop\\Project\\Ultrac","sessionId":"2dedd8c3-4a03-4143-9faf-b0b298f027bb","version":"unknown","gitBranch":"HEAD"}`

const realAssistantLine = `{"parentUuid":"5e9e5724-e6bf-4417-80da-75cbd770b8b9","isSidechain":false,"message":{"id":"5f1b77c3","type":"message","role":"assistant","model":"deepseek-flash","content":[{"type":"text","text":"PROVIDER-OK"}],"stop_reason":"end_turn","usage":{"input_tokens":22609,"output_tokens":6}},"type":"assistant","uuid":"3541c28b-1f2c-4e8e-96f8-fccda2557d4a","timestamp":"2026-09-29T01:23:19.957Z","userType":"external","entrypoint":"sdk-cli","cwd":"D:\\develop\\Project\\Ultrac","sessionId":"2dedd8c3-4a03-4143-9faf-b0b298f027bb","version":"unknown","gitBranch":"HEAD"}`

// ---------------------------------------------------------------------------
// Reading what the TypeScript implementation wrote
// ---------------------------------------------------------------------------

func TestReadsRealTypeScriptTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "2dedd8c3-4a03-4143-9faf-b0b298f027bb.jsonl")
	content := strings.Join([]string{
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-09-29T01:23:14.047Z","sessionId":"2dedd8c3-4a03-4143-9faf-b0b298f027bb","content":"reply with exactly: PROVIDER-OK"}`,
		realTranscriptLine,
		realAssistantLine,
		`{"type":"last-prompt","lastPrompt":"reply with exactly: PROVIDER-OK","sessionId":"2dedd8c3-4a03-4143-9faf-b0b298f027bb"}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t)
	entries, err := s.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("read %d entries, want 4", len(entries))
	}

	user := entries[1]
	if user.Type != EntryUser {
		t.Errorf("type = %q", user.Type)
	}
	if user.UUID != "5e9e5724-e6bf-4417-80da-75cbd770b8b9" {
		t.Errorf("uuid = %q", user.UUID)
	}
	if user.CWD != `D:\develop\Project\Ultrac` {
		t.Errorf("cwd = %q; a backslash path must survive intact", user.CWD)
	}
	if user.PermissionMode != "default" || user.Entrypoint != "sdk-cli" {
		t.Errorf("modelled fields lost: %+v", user)
	}
	if txt := user.FirstUserText(); txt != "reply with exactly: PROVIDER-OK" {
		t.Errorf("content = %q", txt)
	}
	// Modelled fields must be captured, not just preserved as opaque JSON.
	if user.GitBranch != "HEAD" {
		t.Errorf("gitBranch = %q", user.GitBranch)
	}
	// And fields this version does not model must survive, or a write would
	// strip them from the user's transcript.
	if user.Version != "unknown" {
		t.Errorf("version = %q", user.Version)
	}

	if got := entries[3].LastPrompt; got != "reply with exactly: PROVIDER-OK" {
		t.Errorf("lastPrompt = %q", got)
	}
}

func TestChainFollowsParentUUIDNotFileOrder(t *testing.T) {
	// After a rewind the file contains abandoned messages followed by re-asked
	// ones. Trusting file order would replay the abandoned branch.
	dir := t.TempDir()
	path := filepath.Join(dir, "chain.jsonl")
	lines := []string{
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-10-01T00:00:00.000Z","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2026-10-01T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"abandoned"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"u1","timestamp":"2026-10-01T00:00:02.000Z","message":{"role":"user","content":"re-asked"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"u2","timestamp":"2026-10-01T00:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"kept"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t)
	entries, err := s.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	chain := BuildChain(entries)

	got := make([]string, 0, len(chain.Entries))
	for _, e := range chain.Entries {
		got = append(got, e.UUID)
	}
	want := []string{"u1", "u2", "a2"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("chain = %v, want %v (a1 was abandoned by the rewind)", got, want)
	}
}

func TestChainExcludesProgress(t *testing.T) {
	// Progress is ephemeral UI state. Including it forks the chain and orphans
	// real messages on resume — the exact bug the format excludes it to avoid.
	lines := []string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"go"}}`,
		`{"type":"progress","uuid":"p1","parentUuid":"u1","message":{"dataType":"bash_progress"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":"done"}}`,
	}
	chain := BuildChain(mustEntries(t, lines))

	for _, e := range chain.Entries {
		if e.Type == EntryProgress {
			t.Error("a progress entry reached the conversation chain")
		}
	}
	if len(chain.Entries) != 2 {
		t.Errorf("chain has %d entries, want 2", len(chain.Entries))
	}
}

func TestChainExcludesSidechains(t *testing.T) {
	// A subagent's turns are a separate conversation. Interleaving them
	// duplicates messages on resume.
	lines := []string{
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"task"}}`,
		`{"type":"user","uuid":"s1","parentUuid":"u1","isSidechain":true,"message":{"role":"user","content":"subagent"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":"result"}}`,
	}
	chain := BuildChain(mustEntries(t, lines))
	for _, e := range chain.Entries {
		if e.IsSidechain {
			t.Errorf("sidechain entry %s reached the main chain", e.UUID)
		}
	}
	if len(chain.Entries) != 2 {
		t.Errorf("chain has %d entries, want 2", len(chain.Entries))
	}
}

func TestChainSurvivesCycle(t *testing.T) {
	// A cycle in the parent links means the file was corrupted. Stopping beats
	// looping forever inside a resume.
	lines := []string{
		`{"type":"user","uuid":"a","parentUuid":"b"}`,
		`{"type":"user","uuid":"b","parentUuid":"a"}`,
	}
	chain := BuildChain(mustEntries(t, lines))
	if len(chain.Entries) > 2 {
		t.Errorf("a cyclic chain produced %d entries", len(chain.Entries))
	}
}

func TestChainStopsAtCompactedParent(t *testing.T) {
	// After compaction the parent was summarised away. The chain starting at
	// the surviving entry is correct, not an error.
	lines := []string{
		`{"type":"user","uuid":"u9","parentUuid":"compacted-away"}`,
		`{"type":"assistant","uuid":"a9","parentUuid":"u9"}`,
	}
	chain := BuildChain(mustEntries(t, lines))
	if len(chain.Entries) != 2 || chain.Entries[0].UUID != "u9" {
		t.Errorf("chain = %v, want [u9 a9]", chainUUIDs(chain))
	}
}

func TestEmptyChainIsNotNil(t *testing.T) {
	// Callers should not need a nil check on the common "no entries yet" path.
	chain := BuildChain(nil)
	if chain == nil {
		t.Fatal("BuildChain(nil) returned nil")
	}
	if len(chain.Entries) != 0 {
		t.Error("expected an empty chain")
	}
}

// ---------------------------------------------------------------------------
// Path layout
// ---------------------------------------------------------------------------

func TestSanitizePathMatchesTypeScript(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/Users/foo/my-project", "-Users-foo-my-project"},
		{`D:\develop\Project\Ultrac`, "D--develop-Project-Ultrac"},
		{"plugin:name:server", "plugin-name-server"},
		{"simple", "simple"},
	}
	for _, tc := range cases {
		if got := SanitizePath(tc.in); got != tc.want {
			t.Errorf("SanitizePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizePathProducesTheRealProjectDirectoryName(t *testing.T) {
	// The name in ~/.ultrac/projects on this machine. If this changes, every
	// existing session becomes invisible.
	if got := SanitizePath(`D:\develop\Project\Ultrac`); got != "D--develop-Project-Ultrac" {
		t.Errorf("SanitizePath = %q", got)
	}
}

func TestLongPathGetsDistinctSuffixes(t *testing.T) {
	// Without the hash, two long paths sharing a 200-character prefix would
	// resolve to the same directory and their sessions would interleave.
	base := strings.Repeat("a", 250)
	one := SanitizePath(base + "-one")
	two := SanitizePath(base + "-two")
	if one == two {
		t.Error("two long paths collided into one directory name")
	}
	if len(one) <= maxSanitizedLength {
		t.Errorf("long path was not truncated: %q", one)
	}
	if !strings.HasPrefix(one, base[:maxSanitizedLength]) {
		t.Error("truncation should keep the leading prefix")
	}
}

func TestValidUUID(t *testing.T) {
	if !ValidUUID("2dedd8c3-4a03-4143-9faf-b0b298f027bb") {
		t.Error("a real session id was rejected")
	}
	for _, bad := range []string{"", "not-a-uuid", "2dedd8c34a0341439fafb0b298f027bb"} {
		if ValidUUID(bad) {
			t.Errorf("%q was accepted as a UUID", bad)
		}
	}
}

func TestNewUUIDIsV4AndUnique(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for range 1000 {
		id := NewUUID()
		if !ValidUUID(id) {
			t.Fatalf("generated an invalid UUID: %q", id)
		}
		if id[14] != '4' {
			t.Fatalf("version nibble is not 4: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate UUID generated: %q", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// Writing
// ---------------------------------------------------------------------------

func TestAppendThenReadRoundTrip(t *testing.T) {
	s := newTestStore(t)
	cwd := t.TempDir()
	sessionID := NewUUID()
	path := s.TranscriptPath(cwd, sessionID)

	if err := s.Append(path, NewUserEntry("do the thing", sessionID, cwd)); err != nil {
		t.Fatal(err)
	}
	assistant := NewAssistantEntry(map[string]any{
		"role":    "assistant",
		"content": []any{map[string]any{"type": "text", "text": "done"}},
	}, sessionID, cwd)
	assistant.ParentUUID = "placeholder"
	if err := s.Append(path, assistant); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(path); err != nil {
		t.Fatal(err)
	}

	// Every line must be valid JSON with a trailing newline, because that is
	// what the TypeScript reader and every shell one-liner expect.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("transcript does not end with a newline")
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var probe map[string]any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("line %d is not a JSON object: %v", i+1, err)
		}
		if _, ok := probe["uuid"]; !ok {
			t.Errorf("line %d has no uuid", i+1)
		}
	}

	entries, err := s.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("read %d entries, want 2", len(entries))
	}
	if entries[0].FirstUserText() != "do the thing" {
		t.Errorf("content = %q", entries[0].FirstUserText())
	}
}

func TestAppendCreatesParentDirectories(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(s.ProjectDir(t.TempDir()), NewUUID()+".jsonl")
	if err := s.Append(path, NewUserEntry("hi", "sid", cwdOrRoot(t))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("transcript was not created: %v", err)
	}
}

func TestUnmodelledFieldsSurviveRoundTrip(t *testing.T) {
	// A transcript is shared with the TypeScript reader. Dropping a field it
	// understands changes the conversation on resume.
	dir := t.TempDir()
	path := filepath.Join(dir, "extra.jsonl")
	line := `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hi"},"cwd":"/repo","futureField":{"nested":[1,2,3]},"anotherOne":"kept"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t)
	entries, err := s.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("read %d entries", len(entries))
	}
	if _, ok := entries[0].Extra["futureField"]; !ok {
		t.Error("an unmodelled field was dropped on read")
	}

	// Re-append and confirm the field is written back out.
	out := filepath.Join(dir, "out.jsonl")
	if err := s.Append(out, entries[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), "futureField") {
		t.Errorf("unmodelled field was dropped on write:\n%s", data)
	}
	if !strings.Contains(string(data), "anotherOne") {
		t.Errorf("a second unmodelled field was dropped:\n%s", data)
	}
}

func TestTruncatedLastLineIsSkipped(t *testing.T) {
	// A crash mid-append leaves a partial line. Refusing to read the file
	// would lose an entire session over one bad byte.
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.jsonl")
	content := `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"complete"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","parentU`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t)
	entries, err := s.Read(path)
	if err != nil {
		t.Fatalf("a truncated line must not fail the read: %v", err)
	}
	if len(entries) != 1 || entries[0].UUID != "u1" {
		t.Errorf("recovered %v, want the one complete entry", chainUUIDs(BuildChain(entries)))
	}
}

func TestMissingFileReadsEmptyNotError(t *testing.T) {
	s := newTestStore(t)
	entries, err := s.Read(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Errorf("a missing transcript must not be an error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries", len(entries))
	}
}

func TestRemoveByUUIDLeavesTombstone(t *testing.T) {
	// An append-only file cannot have a line deleted, and simply omitting the
	// entry would break every parentUuid pointing at it.
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "remove.jsonl")
	entries := []*Entry{
		{Type: EntryUser, UUID: "u1", ParentUUID: "", Timestamp: NowISO(), Message: userMessage{Role: "user", Content: "one"}},
		{Type: EntryAssistant, UUID: "a1", ParentUUID: "u1", Timestamp: NowISO()},
		{Type: EntryAssistant, UUID: "a2", ParentUUID: "u1", Timestamp: NowISO()},
	}
	if err := s.Append(path, entries...); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(path); err != nil {
		t.Fatal(err)
	}

	if err := s.RemoveByUUID(path, "a1"); err != nil {
		t.Fatal(err)
	}

	after, err := s.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 {
		t.Fatalf("removal changed the line count to %d; it must leave a tombstone", len(after))
	}
	var tomb *Entry
	for _, e := range after {
		if e.UUID == "a1" {
			tomb = e
		}
	}
	if tomb == nil {
		t.Fatal("the removed entry's line vanished instead of becoming a tombstone")
	}
	if !tomb.IsTombstone() {
		t.Error("the entry was not marked as a tombstone")
	}
	if tomb.ParentUUID != "u1" {
		t.Error("the tombstone lost its parentUuid, orphaning a2")
	}
}

func TestRemoveByUUIDUnknownIDIsNoOp(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "noop.jsonl")
	if err := s.Append(path, &Entry{Type: EntryUser, UUID: "u1", Timestamp: NowISO()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)

	if err := s.RemoveByUUID(path, "does-not-exist"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("removing an unknown id rewrote the file")
	}
}

func TestListReturnsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	cwd := t.TempDir()
	if err := os.MkdirAll(s.ProjectDir(cwd), 0o755); err != nil {
		t.Fatal(err)
	}

	older := filepath.Join(s.ProjectDir(cwd), NewUUID()+".jsonl")
	newer := filepath.Join(s.ProjectDir(cwd), NewUUID()+".jsonl")
	if err := os.WriteFile(older, []byte(
		`{"type":"user","uuid":"u1","parentUuid":null,"timestamp":"2026-10-01T00:00:00.000Z","cwd":"`+filepath.ToSlash(cwd)+`","message":{"role":"user","content":"older session"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, []byte(
		`{"type":"user","uuid":"u2","parentUuid":null,"timestamp":"2026-10-02T00:00:00.000Z","cwd":"`+filepath.ToSlash(cwd)+`","message":{"role":"user","content":"newer session"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-UUID filename must be ignored, not listed as a broken session.
	if err := os.WriteFile(filepath.Join(s.ProjectDir(cwd), "notes.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, err := s.List(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(list))
	}
	if list[0].FirstPrompt != "newer session" {
		t.Errorf("list is not newest-first: %q then %q", list[0].FirstPrompt, list[1].FirstPrompt)
	}
	if list[0].MessageCount != 1 {
		t.Errorf("MessageCount = %d", list[0].MessageCount)
	}
}

func TestSessionTitleFallbacks(t *testing.T) {
	// Long prompts are cut to 50 runes with an ellipsis, which is what a
	// session list column can show without wrapping.
	long := strings.Repeat("a", 80)
	cases := []struct {
		info SessionInfo
		want string
	}{
		{SessionInfo{CustomTitle: "renamed", Summary: "sum", FirstPrompt: "prompt"}, "renamed"},
		{SessionInfo{Summary: "sum", FirstPrompt: "prompt"}, "sum"},
		{SessionInfo{FirstPrompt: long}, long[:50] + "…"},
		{SessionInfo{SessionID: "2dedd8c3-4a03"}, "2dedd8c3"},
		{SessionInfo{}, "Untitled session"},
	}
	for _, tc := range cases {
		if got := tc.info.Title(); got != tc.want {
			t.Errorf("Title() = %q, want %q", got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

func TestHistoryReturnsNewestFirst(t *testing.T) {
	s := newTestStore(t)
	for i, display := range []string{"first", "second", "third"} {
		if err := s.AppendHistory(HistoryEntry{
			Display:   display,
			Project:   "/repo",
			Timestamp: int64(1000 + i),
			SessionID: NewUUID(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.History("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d entries, want 3", len(got))
	}
	if got[0].Display != "third" {
		t.Errorf("history is not newest-first: %v", got)
	}
}

func TestHistorySkipsMalformedLines(t *testing.T) {
	// One bad line must not stop the up-arrow picker from working.
	s := newTestStore(t)
	path := s.HistoryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{"display":"good one","project":"/repo","timestamp":1}` + "\n" +
		`{ not json` + "\n" +
		`{"display":"good two","project":"/repo","timestamp":2}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.History("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d entries, want 2", len(got))
	}
	if got[0].Display != "good two" {
		t.Errorf("newest surviving entry = %q", got[0].Display)
	}
}

func TestHistoryMissingFileIsEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.History("/repo")
	if err != nil {
		t.Errorf("missing history must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries", len(got))
	}
}

func TestHistoryIsBounded(t *testing.T) {
	// The cost of reading history must be bounded even in a file with years
	// of prompts.
	s := newTestStore(t)
	path := s.HistoryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := range MaxHistoryItems + 200 {
		b.WriteString(`{"display":"p`)
		b.WriteString(strings.Repeat("x", 1))
		b.WriteString(itoa(i))
		b.WriteString(`","project":"/repo","timestamp":1}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := s.History("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxHistoryItems {
		t.Errorf("read %d entries, want the %d cap", len(got), MaxHistoryItems)
	}
	// The newest survive the cap, because that is what the picker wants.
	if got[0].Display != "p"+"x"+itoa(MaxHistoryItems+199) {
		t.Errorf("the newest entry did not survive: %q", got[0].Display)
	}
}

// --- helpers ---------------------------------------------------------------

// TestConcurrentAppendsProduceWholeLines runs without -race available in every
// environment, so it asserts the observable property instead: under parallel
// appends every line is still a complete JSON object and none is torn.
//
// Concurrency is not hypothetical here — a turn appends from several
// goroutines (tools run in parallel), and a torn line would be skipped on read,
// silently losing a message.
func TestConcurrentAppendsProduceWholeLines(t *testing.T) {
	s := newTestStore(t)
	path := filepath.Join(t.TempDir(), "concurrent.jsonl")
	sessionID := NewUUID()

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	var appendErrs atomic.Int64
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				// The error is asserted, not discarded: a swallowed append
				// error is exactly how a concurrent-create "Access is denied"
				// on Windows used to lose transcript lines invisibly.
				if err := s.Append(path, &Entry{
					Type:        EntryUser,
					UUID:        NewUUID(),
					Timestamp:   NowISO(),
					SessionID:   sessionID,
					IsSidechain: w%2 == 1,
					Message:     userMessage{Role: "user", Content: fmt.Sprintf("w%d-%d", w, i)},
				}); err != nil {
					appendErrs.Add(1)
					t.Errorf("append w%d-%d: %v", w, i, err)
				}
			}
		}(w)
	}
	wg.Wait()
	if n := appendErrs.Load(); n > 0 {
		t.Errorf("%d append(s) failed", n)
	}
	if err := s.Flush(path); err != nil {
		t.Fatal(err)
	}

	entries, err := s.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != writers*perWriter {
		t.Errorf("read %d entries, want %d; a torn write lost data", len(entries), writers*perWriter)
	}
	for _, e := range entries {
		if e.UUID == "" || e.Type != EntryUser {
			t.Errorf("a line is malformed: %+v", e)
		}
	}
}

func mustEntries(t *testing.T, lines []string) []*Entry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStoreAt(t.TempDir())
	t.Cleanup(func() { s.Close() })
	entries, err := s.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func chainUUIDs(c *Chain) []string {
	out := make([]string, 0, len(c.Entries))
	for _, e := range c.Entries {
		out = append(out, e.UUID)
	}
	return out
}

func cwdOrRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return t.TempDir()
	}
	return wd
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
