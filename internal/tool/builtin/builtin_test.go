package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ultrac/internal/contract/tool"
)

// call runs a registered tool by name with raw JSON input.
func call(t *testing.T, name string, input string, uc *tool.UseContext) *tool.Result {
	t.Helper()
	tl, ok := tool.NewRegistry(tool.Builtins()...).Lookup(name)
	if !ok {
		t.Fatalf("tool %q is not registered", name)
	}
	res, err := tl.Call(context.Background(), json.RawMessage(input), uc, nil, nil)
	if err != nil {
		t.Fatalf("%s returned a Go error: %v", name, err)
	}
	if res == nil {
		t.Fatalf("%s returned no result", name)
	}
	return res
}

func ctxAt(dir string) *tool.UseContext {
	return &tool.UseContext{WorkDir: dir, SessionID: "test-session"}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRegisteredToolSet pins the T1.3 deliverable: the twelve core tools, by
// name, because the names are the model's interface.
func TestRegisteredToolSet(t *testing.T) {
	want := []string{
		"Agent", "AskUserQuestion", "Bash", "Edit", "Glob", "Grep",
		"Read", "TaskOutput", "TodoWrite", "WebFetch", "WebSearch", "Write",
	}
	got := tool.BuiltinNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("built-in tools = %v, want %v", got, want)
	}
	for _, tl := range tool.Builtins() {
		schema := tl.InputSchema()
		if len(schema) == 0 {
			t.Errorf("%s has no input schema", tl.Name())
		}
		if !json.Valid(schema) {
			t.Errorf("%s has an invalid input schema", tl.Name())
		}
		var probe map[string]any
		if err := json.Unmarshal(schema, &probe); err != nil {
			t.Errorf("%s schema does not parse: %v", tl.Name(), err)
		}
		if probe["type"] != "object" {
			t.Errorf("%s schema is not an object schema", tl.Name())
		}
		if tl.SearchHint() == "" {
			t.Errorf("%s has no search hint", tl.Name())
		}
	}
}

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	write(t, path, "line one\nline two\nline three\n")

	res := call(t, "Read", `{"file_path":`+jsonStr(path)+`}`, ctxAt(dir))
	if res.IsError {
		t.Fatalf("read failed: %s", res.Content)
	}
	// cat -n shape: right-aligned number, tab, content.
	if !strings.Contains(res.Content, "     1\tline one") {
		t.Errorf("output is not cat -n formatted:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "     3\tline three") {
		t.Errorf("last line missing:\n%s", res.Content)
	}

	meta := res.Metadata
	if meta["totalLines"] != 3 {
		t.Errorf("totalLines = %v", meta["totalLines"])
	}
	if meta["numLines"] != 3 {
		t.Errorf("numLines = %v", meta["numLines"])
	}
}

func TestReadOffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	write(t, path, "1\n2\n3\n4\n5\n")

	res := call(t, "Read", `{"file_path":`+jsonStr(path)+`,"offset":1,"limit":2}`, ctxAt(dir))
	if res.IsError {
		t.Fatalf("read failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "     2\t2") || !strings.Contains(res.Content, "     3\t3") {
		t.Errorf("window is wrong:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "     1\t1") || strings.Contains(res.Content, "     4\t4") {
		t.Errorf("window leaked outside offset/limit:\n%s", res.Content)
	}
	if res.Metadata["truncated"] != true {
		t.Errorf("truncated = %v, want true", res.Metadata["truncated"])
	}
}

func TestReadErrors(t *testing.T) {
	dir := t.TempDir()
	missing := call(t, "Read", `{"file_path":`+jsonStr(filepath.Join(dir, "nope.txt"))+`}`, ctxAt(dir))
	if !missing.IsError || !strings.Contains(missing.Content, "does not exist") {
		t.Errorf("missing file result = %+v", missing)
	}

	empty := filepath.Join(dir, "empty.txt")
	write(t, empty, "")
	got := call(t, "Read", `{"file_path":`+jsonStr(empty)+`}`, ctxAt(dir))
	if got.IsError {
		t.Errorf("an empty file is not an error: %s", got.Content)
	}
	if got.Metadata["empty"] != true {
		t.Errorf("empty flag = %v", got.Metadata["empty"])
	}

	if r := call(t, "Read", `{}`, ctxAt(dir)); !r.IsError {
		t.Error("a missing file_path was accepted")
	}
}

func TestReadDirectoryListsEntries(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sub", "x"), "x")
	write(t, filepath.Join(dir, "y.txt"), "y")

	res := call(t, "Read", `{"file_path":`+jsonStr(dir)+`}`, ctxAt(dir))
	if res.IsError {
		t.Fatalf("listing a directory should succeed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "y.txt") || !strings.Contains(res.Content, "sub/") {
		t.Errorf("directory listing is incomplete:\n%s", res.Content)
	}
}

func TestReadTruncatesHugeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(path, make([]byte, maxReadBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	res := call(t, "Read", `{"file_path":`+jsonStr(path)+`}`, ctxAt(dir))
	if !res.IsError {
		t.Fatal("an oversized file should be refused, not silently truncated")
	}
	if !strings.Contains(res.Content, "limit") {
		t.Errorf("refusal does not explain the limit: %s", res.Content)
	}
}

func TestWriteCreatesAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out", "new.txt")

	created := call(t, "Write", `{"file_path":`+jsonStr(path)+`,"content":"hello"}`, ctxAt(dir))
	if created.IsError {
		t.Fatalf("write failed: %s", created.Content)
	}
	if created.Metadata["type"] != "create" {
		t.Errorf("type = %v, want create", created.Metadata["type"])
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "hello" {
		t.Fatalf("file content = %q, err = %v", data, err)
	}
	if created.Metadata["originalFile"] != nil {
		t.Errorf("originalFile for a new file = %v, want nil", created.Metadata["originalFile"])
	}

	updated := call(t, "Write", `{"file_path":`+jsonStr(path)+`,"content":"hello\nworld"}`, ctxAt(dir))
	if updated.Metadata["type"] != "update" {
		t.Errorf("type = %v, want update", updated.Metadata["type"])
	}
	// The diff must show the added line, or the model cannot confirm what it
	// did without re-reading the file.
	if !strings.Contains(updated.Content, "+world") {
		t.Errorf("update output has no diff:\n%s", updated.Content)
	}
}

func TestEdit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "code.go")
	write(t, path, "package main\n\nfunc main() {\n\tprintln(\"a\")\n}\n")

	res := call(t, "Edit",
		`{"file_path":`+jsonStr(path)+`,"old_string":"println(\"a\")","new_string":"println(\"b\")"}`,
		ctxAt(dir))
	if res.IsError {
		t.Fatalf("edit failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "-"+`	println("a")`) || !strings.Contains(res.Content, "+"+`	println("b")`) {
		t.Errorf("edit diff is wrong:\n%s", res.Content)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `println("b")`) {
		t.Errorf("file was not edited: %s", data)
	}
}

func TestEditRefusesAmbiguousAndMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	write(t, path, "dup\ndup\n")

	ambiguous := call(t, "Edit",
		`{"file_path":`+jsonStr(path)+`,"old_string":"dup","new_string":"one"}`,
		ctxAt(dir))
	if !ambiguous.IsError || !strings.Contains(ambiguous.Content, "2 times") {
		t.Errorf("ambiguous edit result = %+v", ambiguous)
	}
	if data, _ := os.ReadFile(path); string(data) != "dup\ndup\n" {
		t.Errorf("a refused edit modified the file: %q", data)
	}

	all := call(t, "Edit",
		`{"file_path":`+jsonStr(path)+`,"old_string":"dup","new_string":"one","replace_all":true}`,
		ctxAt(dir))
	if all.IsError {
		t.Fatalf("replace_all failed: %s", all.Content)
	}
	if data, _ := os.ReadFile(path); string(data) != "one\none\n" {
		t.Errorf("replace_all result = %q", data)
	}

	missing := call(t, "Edit",
		`{"file_path":`+jsonStr(path)+`,"old_string":"absent","new_string":"x"}`,
		ctxAt(dir))
	if !missing.IsError || !strings.Contains(missing.Content, "was not found") {
		t.Errorf("missing old_string result = %+v", missing)
	}

	same := call(t, "Edit",
		`{"file_path":`+jsonStr(path)+`,"old_string":"one","new_string":"one"}`,
		ctxAt(dir))
	if !same.IsError {
		t.Error("an identical replacement was accepted")
	}

	noFile := call(t, "Edit",
		`{"file_path":`+jsonStr(filepath.Join(dir, "ghost.txt"))+`,"old_string":"a","new_string":"b"}`,
		ctxAt(dir))
	if !noFile.IsError {
		t.Error("editing a missing file was accepted")
	}
}

func TestGlob(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "a")
	write(t, filepath.Join(dir, "b.txt"), "b")
	write(t, filepath.Join(dir, "sub", "c.go"), "c")
	write(t, filepath.Join(dir, "sub", "deep", "d.go"), "d")

	res := call(t, "Glob", `{"pattern":"**/*.go"}`, ctxAt(dir))
	if res.IsError {
		t.Fatalf("glob failed: %s", res.Content)
	}
	for _, want := range []string{"a.go", "sub/c.go", "sub/deep/d.go"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("glob missed %s:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "b.txt") {
		t.Errorf("glob matched a non-matching file:\n%s", res.Content)
	}
	if res.Metadata["numFiles"] != 3 {
		t.Errorf("numFiles = %v", res.Metadata["numFiles"])
	}

	// A pattern whose literal prefix narrows the walk still matches from the
	// right root.
	scoped := call(t, "Glob", `{"pattern":"sub/**/*.go"}`, ctxAt(dir))
	if scoped.IsError {
		t.Fatalf("scoped glob failed: %s", scoped.Content)
	}
	if !strings.Contains(scoped.Content, "sub/deep/d.go") {
		t.Errorf("scoped glob missed a deep file:\n%s", scoped.Content)
	}
	if strings.Contains(scoped.Content, "\na.go") || strings.HasPrefix(scoped.Content, "a.go") {
		t.Errorf("scoped glob leaked outside its prefix:\n%s", scoped.Content)
	}

	none := call(t, "Glob", `{"pattern":"**/*.rs"}`, ctxAt(dir))
	if none.IsError || !strings.Contains(none.Content, "No files found") {
		t.Errorf("empty glob result = %+v", none)
	}

	bad := call(t, "Glob", `{"pattern":"[unterminated"}`, ctxAt(dir))
	if bad.IsError {
		// An unterminated class is treated literally rather than failing.
		t.Errorf("unterminated class should degrade, not error: %s", bad.Content)
	}
}

func TestGlobSkipsIgnoredDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "node_modules", "dep", "index.js"), "x")
	write(t, filepath.Join(dir, ".git", "config"), "x")
	write(t, filepath.Join(dir, "src", "index.js"), "x")

	res := call(t, "Glob", `{"pattern":"**/*.js"}`, ctxAt(dir))
	if strings.Contains(res.Content, "node_modules") {
		t.Errorf("glob walked node_modules:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "src/index.js") {
		t.Errorf("glob missed a real file:\n%s", res.Content)
	}
}

func TestGrepModes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package main\nfunc hello() {}\n")
	write(t, filepath.Join(dir, "b.go"), "package other\n")
	write(t, filepath.Join(dir, "c.txt"), "hello hello hello\n")

	files := call(t, "Grep", `{"pattern":"hello"}`, ctxAt(dir))
	if files.IsError {
		t.Fatalf("grep failed: %s", files.Content)
	}
	if !strings.Contains(files.Content, "a.go") || !strings.Contains(files.Content, "c.txt") {
		t.Errorf("files_with_matches missed a file:\n%s", files.Content)
	}
	if strings.Contains(files.Content, "b.go") {
		t.Errorf("files_with_matches matched a file with no match:\n%s", files.Content)
	}
	if files.Metadata["mode"] != "files_with_matches" {
		t.Errorf("mode = %v, want files_with_matches (the default)", files.Metadata["mode"])
	}

	content := call(t, "Grep", `{"pattern":"hello","output_mode":"content"}`, ctxAt(dir))
	if content.IsError {
		t.Fatalf("content grep failed: %s", content.Content)
	}
	// rg-style line: file:line:text for a match.
	if !strings.Contains(content.Content, "a.go:2:func hello() {}") {
		t.Errorf("content mode is not file:line:text:\n%s", content.Content)
	}

	count := call(t, "Grep", `{"pattern":"hello","output_mode":"count"}`, ctxAt(dir))
	if count.IsError {
		t.Fatalf("count grep failed: %s", count.Content)
	}
	if !strings.Contains(count.Content, "c.txt:3") {
		t.Errorf("count mode is wrong:\n%s", count.Content)
	}
	if !strings.Contains(count.Content, "occurrences") {
		t.Errorf("count mode has no summary:\n%s", count.Content)
	}
}

func TestGrepFilters(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "Needle\n")
	write(t, filepath.Join(dir, "b.py"), "needle\n")

	caseSensitive := call(t, "Grep", `{"pattern":"Needle"}`, ctxAt(dir))
	if strings.Contains(caseSensitive.Content, "b.py") {
		t.Errorf("case-sensitive search matched a lowercase file:\n%s", caseSensitive.Content)
	}

	insensitive := call(t, "Grep", `{"pattern":"Needle","-i":true}`, ctxAt(dir))
	if !strings.Contains(insensitive.Content, "b.py") {
		t.Errorf("case-insensitive search missed a file:\n%s", insensitive.Content)
	}

	byGlob := call(t, "Grep", `{"pattern":"needle","-i":true,"glob":"*.go"}`, ctxAt(dir))
	if strings.Contains(byGlob.Content, "b.py") {
		t.Errorf("glob filter was ignored:\n%s", byGlob.Content)
	}

	byType := call(t, "Grep", `{"pattern":"needle","-i":true,"type":"py"}`, ctxAt(dir))
	if !strings.Contains(byType.Content, "b.py") || strings.Contains(byType.Content, "a.go") {
		t.Errorf("type filter is wrong:\n%s", byType.Content)
	}

	badType := call(t, "Grep", `{"pattern":"x","type":"brainfuck"}`, ctxAt(dir))
	if !badType.IsError {
		t.Error("an unknown type was silently accepted, so 'no results' and 'filter ignored' look alike")
	}

	badPattern := call(t, "Grep", `{"pattern":"("}`, ctxAt(dir))
	if !badPattern.IsError {
		t.Error("an invalid regexp was accepted")
	}
}

func TestGrepPagination(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "match line")
	}
	write(t, filepath.Join(dir, "big.txt"), strings.Join(lines, "\n")+"\n")

	limited := call(t, "Grep", `{"pattern":"match","output_mode":"content","head_limit":5}`, ctxAt(dir))
	if got := strings.Count(limited.Content, "match line"); got != 5 {
		t.Errorf("head_limit=5 produced %d lines:\n%s", got, limited.Content)
	}

	offset := call(t, "Grep", `{"pattern":"match","output_mode":"content","head_limit":3,"offset":2}`, ctxAt(dir))
	if !strings.Contains(offset.Content, "[Showing results with pagination") {
		t.Errorf("offset output has no pagination note:\n%s", offset.Content)
	}

	unlimited := call(t, "Grep", `{"pattern":"match","output_mode":"content","head_limit":0}`, ctxAt(dir))
	if got := strings.Count(unlimited.Content, "match line"); got != 20 {
		t.Errorf("head_limit=0 produced %d lines, want 20", got)
	}
}

func TestBashReadsAndReportsExitCode(t *testing.T) {
	dir := t.TempDir()
	res := call(t, "Bash", `{"command":"echo hello-from-bash"}`, ctxAt(dir))
	if res.IsError {
		t.Fatalf("echo failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hello-from-bash") {
		t.Errorf("stdout not captured: %s", res.Content)
	}
	if res.Metadata["exitCode"] != 0 {
		t.Errorf("exitCode = %v", res.Metadata["exitCode"])
	}

	failing := call(t, "Bash", `{"command":"exit 3"}`, ctxAt(dir))
	if !failing.IsError {
		t.Error("a non-zero exit was not marked as an error")
	}
	if failing.Metadata["exitCode"] != 3 {
		t.Errorf("exitCode = %v, want 3", failing.Metadata["exitCode"])
	}
}

func TestBashStderrIsSeparated(t *testing.T) {
	dir := t.TempDir()
	res := call(t, "Bash", `{"command":"echo out; echo err 1>&2"}`, ctxAt(dir))
	if !strings.Contains(res.Content, "out") || !strings.Contains(res.Content, "[stderr]") {
		t.Errorf("stderr was not labelled:\n%s", res.Content)
	}
}

func TestBashTimeout(t *testing.T) {
	dir := t.TempDir()
	res := call(t, "Bash", `{"command":"sleep 5","timeout":300}`, ctxAt(dir))
	if res.Metadata["timedOut"] != true {
		t.Errorf("timedOut = %v, want true", res.Metadata["timedOut"])
	}
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("timeout is not reported: %s", res.Content)
	}
}

// TestBashPredicatesAreStructural is the tool-level half of the DoD: the
// read-only judgement has to come from the parsed command, not the text.
func TestBashPredicatesAreStructural(t *testing.T) {
	tl, ok := tool.NewRegistry(tool.Builtins()...).Lookup("Bash")
	if !ok {
		t.Fatal("Bash is not registered")
	}
	cases := []struct {
		command     string
		readOnly    bool
		destructive bool
	}{
		{`{"command":"ls -la"}`, true, false},
		{`{"command":"rm -rf /"}`, false, true},
		{`{"command":"echo 'rm -rf /'"}`, true, false},
		{`{"command":"git status"}`, true, false},
		{`{"command":"git push --force"}`, false, true},
		{`{"command":"python -c 'x'"}`, false, false},
		{`{"command":"ls && echo hi"}`, true, false},
		{`{"command":"while true; do ls; done"}`, false, false},
	}
	for _, c := range cases {
		input := json.RawMessage(c.command)
		if got := tl.IsReadOnly(input); got != c.readOnly {
			t.Errorf("%s IsReadOnly = %v, want %v", c.command, got, c.readOnly)
		}
		if got := tl.IsDestructive(input); got != c.destructive {
			t.Errorf("%s IsDestructive = %v, want %v", c.command, got, c.destructive)
		}
	}
}

func TestTodoWrite(t *testing.T) {
	ClearTodos("todo-test")
	uc := &tool.UseContext{WorkDir: t.TempDir(), SessionID: "todo-test"}

	res := call(t, "TodoWrite",
		`{"todos":[{"content":"Do a thing","status":"in_progress","activeForm":"Doing a thing"}]}`, uc)
	if res.IsError {
		t.Fatalf("todo write failed: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[~] Do a thing") {
		t.Errorf("rendered list is wrong:\n%s", res.Content)
	}
	if got := RenderTodos("todo-test"); !strings.Contains(got, "Do a thing") {
		t.Errorf("the list did not survive for the prompt: %q", got)
	}

	bad := call(t, "TodoWrite", `{"todos":[{"content":"x","status":"done","activeForm":"y"}]}`, uc)
	if !bad.IsError {
		t.Error("an invalid status was accepted")
	}
	empty := call(t, "TodoWrite", `{"todos":[{"content":"","status":"pending","activeForm":"y"}]}`, uc)
	if !empty.IsError {
		t.Error("an empty content was accepted")
	}

	// A fully completed list is stored empty.
	_ = call(t, "TodoWrite",
		`{"todos":[{"content":"Do a thing","status":"completed","activeForm":"Doing a thing"}]}`, uc)
	if got := RenderTodos("todo-test"); got != "" {
		t.Errorf("a completed list was kept: %q", got)
	}
	ClearTodos("todo-test")
}

func TestWebFetchRejectsBadURLs(t *testing.T) {
	dir := t.TempDir()
	for _, in := range []string{
		`{"url":"","prompt":"x"}`,
		`{"url":"ftp://example.com","prompt":"x"}`,
		`{"url":"https://","prompt":"x"}`,
	} {
		res := call(t, "WebFetch", in, ctxAt(dir))
		if !res.IsError {
			t.Errorf("%s was accepted", in)
		}
	}
}

func TestWebSearchValidatesQuery(t *testing.T) {
	res := call(t, "WebSearch", `{"query":"a"}`, ctxAt(t.TempDir()))
	if !res.IsError {
		t.Error("a one-character query was accepted")
	}
}

func TestAskWithoutAFrontend(t *testing.T) {
	SetAskHandler(nil)
	dir := t.TempDir()

	res := call(t, "AskUserQuestion",
		`{"questions":[{"question":"Which?","header":"Choice","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`,
		ctxAt(dir))
	if !res.IsError {
		t.Fatal("AskUserQuestion succeeded with no frontend attached")
	}
	if !strings.Contains(res.Content, "no interactive frontend") {
		t.Errorf("the refusal does not say why: %s", res.Content)
	}

	bad := call(t, "AskUserQuestion",
		`{"questions":[{"question":"Which?","header":"Choice","options":[{"label":"A","description":"a"}]}]}`,
		ctxAt(dir))
	if !bad.IsError {
		t.Error("a question with one option was accepted")
	}

	tooMany := call(t, "AskUserQuestion",
		`{"questions":[{"question":"a","header":"h","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]},{"question":"a","header":"h","options":[{"label":"A","description":"a"},{"label":"B","description":"b"}]}]}`,
		ctxAt(dir))
	if !tooMany.IsError {
		t.Error("a repeated question was accepted")
	}
}

func TestAgentWithoutARunner(t *testing.T) {
	SetAgentRunner(nil)
	res := call(t, "Agent", `{"description":"look around","prompt":"find files"}`, ctxAt(t.TempDir()))
	if !res.IsError {
		t.Fatal("Agent succeeded with no runner installed")
	}
	if !strings.Contains(res.Content, "no subagent runner") {
		t.Errorf("the refusal does not say why: %s", res.Content)
	}

	SetAgentRunner(func(_ context.Context, run AgentRun) (string, error) {
		return "subagent saw: " + run.Prompt, nil
	})
	defer SetAgentRunner(nil)

	ok := call(t, "Agent", `{"description":"look around","prompt":"find files"}`, ctxAt(t.TempDir()))
	if ok.IsError {
		t.Fatalf("Agent failed with a runner installed: %s", ok.Content)
	}
	if !strings.Contains(ok.Content, "subagent saw: find files") {
		t.Errorf("runner output missing: %s", ok.Content)
	}

	missing := call(t, "Agent", `{"description":"","prompt":"x"}`, ctxAt(t.TempDir()))
	if !missing.IsError {
		t.Error("an empty description was accepted")
	}
}

func TestTaskOutputWithoutARegistry(t *testing.T) {
	SetTaskOutputProvider(nil)
	res := call(t, "TaskOutput", `{"task_id":"abc"}`, ctxAt(t.TempDir()))
	if !res.IsError || !strings.Contains(res.Content, "no task registry") {
		t.Errorf("result = %+v", res)
	}

	SetTaskOutputProvider(func(id string) (TaskOutput, bool) {
		if id != "known" {
			return TaskOutput{}, false
		}
		return TaskOutput{TaskID: id, TaskType: "bash", Status: "completed", Output: "it worked"}, true
	})
	defer SetTaskOutputProvider(nil)

	ok := call(t, "TaskOutput", `{"task_id":"known","block":false}`, ctxAt(t.TempDir()))
	if ok.IsError || !strings.Contains(ok.Content, "it worked") {
		t.Errorf("result = %+v", ok)
	}
	unknown := call(t, "TaskOutput", `{"task_id":"nope","block":false}`, ctxAt(t.TempDir()))
	if !unknown.IsError {
		t.Error("an unknown task id was accepted")
	}
}

func TestUnifiedDiff(t *testing.T) {
	got := unifiedDiff("a\nb\nc\n", "a\nB\nc\n", 3)
	if !strings.Contains(got, "-b") || !strings.Contains(got, "+B") {
		t.Errorf("diff body is wrong:\n%s", got)
	}
	if !strings.Contains(got, "@@ -1,3 +1,3 @@") {
		t.Errorf("hunk header is wrong:\n%s", got)
	}
	if unifiedDiff("same\n", "same\n", 3) != "" {
		t.Error("an identical input produced a diff")
	}
	// A large change far apart produces two hunks.
	long := strings.Repeat("x\n", 40)
	oldText := "start\n" + long + "middle\n" + long + "end\n"
	newText := "START\n" + long + "middle\n" + long + "END\n"
	if got := strings.Count(unifiedDiff(oldText, newText, 3), "@@"); got != 4 {
		t.Errorf("expected two hunks (4 @@ markers), got %d:\n%s", got, unifiedDiff(oldText, newText, 3))
	}
}

func TestGlobPatternTranslation(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "sub/a.go", false},
		{"**/*.go", "a.go", true},
		{"**/*.go", "sub/deep/a.go", true},
		{"src/*/*.go", "src/a/b.go", true},
		{"src/*/*.go", "src/a/b/c.go", false},
		{"src/**", "src/a/b/c.go", true},
		{"*.{ts,tsx}", "x.ts", true},
		{"*.{ts,tsx}", "x.tsx", true},
		{"*.{ts,tsx}", "x.js", false},
		{"file?.txt", "file1.txt", true},
		{"file?.txt", "file12.txt", false},
		{"**/secrets/**", "a/secrets/b", true},
		{"**/secrets/**", "secrets/b", true},
		{"[abc].go", "a.go", true},
		{"[!abc].go", "d.go", true},
		{"[!abc].go", "a.go", false},
	}
	for _, c := range cases {
		re, err := compileGlob(c.pattern)
		if err != nil {
			t.Errorf("compileGlob(%q): %v", c.pattern, err)
			continue
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%q matching %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestGlobRootExtraction(t *testing.T) {
	cases := map[string]string{
		"**/*.go":           "",
		"src/**/*.go":       "src",
		"src/internal/*.go": "src/internal",
		"a/b/c.go":          "a/b/c.go",
	}
	for pattern, want := range cases {
		if got := globRoot(pattern); got != want {
			t.Errorf("globRoot(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestPathResolution(t *testing.T) {
	dir := t.TempDir()
	uc := ctxAt(dir)

	if got := resolvePath(uc, "a/b.txt"); got != filepath.Join(dir, "a", "b.txt") {
		t.Errorf("relative path = %q", got)
	}
	if got := resolvePath(uc, "./x"); got != filepath.Join(dir, "x") {
		t.Errorf("dot-relative path = %q", got)
	}
	abs := filepath.Join(dir, "y")
	if got := resolvePath(uc, abs); got != abs {
		t.Errorf("absolute path = %q", got)
	}
	if got := resolvePath(uc, ""); got != "" {
		t.Errorf("empty path = %q", got)
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
