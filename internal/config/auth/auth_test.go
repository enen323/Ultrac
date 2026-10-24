package auth

// auth_test.go — the credential store (P2).
//
// What is under test: pi's auth-storage.ts load and validation rules,
// ported — with the plan's three deviations pinned down by assertions
// rather than comments:
//
//   - oauth is refused explicitly (D4): an oauth entry lands in Problems
//     as *OAuthUnsupportedError, never in Entries;
//   - a read failure degrades per provider (§4.5): a malformed entry
//     costs one provider, a truncated file costs the whole file — and
//     even then only as a warning the caller chooses to print;
//   - the key is a literal (D5/D6): a non-string key is a named problem,
//     and nothing expands or executes it.
//
// The file lock the plan keeps out of scope (§4.5) has no tests here;
// what replaces it — the atomic tmp+rename — is exercised by the
// concurrent-reader test at the bottom.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// write is the sandbox helper: one auth.json under a temp dir, its path
// handed to Load.
func write(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadEntryClassification(t *testing.T) {
	cases := []struct {
		name        string
		doc         string
		wantEntry   bool
		wantKey     string
		wantProblem string // substring of the per-provider problem; "" means none
		wantOAuth   bool
	}{
		{
			name: "api_key with key", doc: `{"p":{"type":"api_key","key":"sk-1"}}`,
			wantEntry: true, wantKey: "sk-1",
		},
		{
			// Valid but useless: APIKey reports "" and the P3 chain
			// falls through. Refusing it here would be pi's mistake —
			// the file is editable by hand and half-written is normal.
			name: "api_key without key", doc: `{"p":{"type":"api_key"}}`,
			wantEntry: true, wantKey: "",
		},
		{
			name: "api_key with null key", doc: `{"p":{"type":"api_key","key":null}}`,
			wantEntry: true, wantKey: "",
		},
		{
			name: "api_key with empty key", doc: `{"p":{"type":"api_key","key":""}}`,
			wantEntry: true, wantKey: "",
		},
		{
			name: "api_key with number key", doc: `{"p":{"type":"api_key","key":42}}`,
			wantProblem: "key must be a string",
		},
		{
			name: "api_key with object key", doc: `{"p":{"type":"api_key","key":{"a":1}}}`,
			wantProblem: "key must be a string",
		},
		{
			name: "no type", doc: `{"p":{"key":"sk-1"}}`,
			wantProblem: `credential has no "type"`,
		},
		{
			name: "unknown type", doc: `{"p":{"type":"bearer","key":"x"}}`,
			wantProblem: "unsupported credential type",
		},
		{
			// D4: the schema accepts it, the chain refuses it.
			name: "oauth", doc: `{"p":{"type":"oauth","accessToken":"x"}}`,
			wantOAuth: true,
		},
		{
			name: "entry not an object", doc: `{"p":"sk-1"}`,
			wantProblem: "credential must be an object",
		},
		{
			name: "type not a string", doc: `{"p":{"type":42,"key":"x"}}`,
			wantProblem: "credential must be an object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Load(write(t, tc.doc))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			e, ok := f.Entries["p"]
			if ok != tc.wantEntry {
				t.Fatalf("entry present = %v, want %v (problems: %v)", ok, tc.wantEntry, f.Problems["p"])
			}
			if ok {
				if e.Type != EntryTypeAPIKey {
					t.Errorf("entry type = %q, want %q", e.Type, EntryTypeAPIKey)
				}
				if e.Key != tc.wantKey {
					t.Errorf("entry key = %q, want %q", e.Key, tc.wantKey)
				}
			}
			// An entry and a problem for the same provider are
			// mutually exclusive by construction.
			if ok && f.Problems["p"] != nil {
				t.Errorf("provider has both an entry and a problem: %v", f.Problems["p"])
			}
			var oauth *OAuthUnsupportedError
			gotOAuth := errors.As(f.Problems["p"], &oauth)
			if gotOAuth != tc.wantOAuth {
				t.Errorf("oauth rejection = %v, want %v (problem: %v)", gotOAuth, tc.wantOAuth, f.Problems["p"])
			}
			switch {
			case tc.wantProblem == "" && !tc.wantOAuth && f.Problems["p"] != nil:
				t.Errorf("unexpected problem: %v", f.Problems["p"])
			case tc.wantProblem != "":
				if got := fmt.Sprint(f.Problems["p"]); !strings.Contains(got, tc.wantProblem) {
					t.Errorf("problem = %q, want it to contain %q", got, tc.wantProblem)
				}
			}
		})
	}
}

func TestLoadMissingFileAndEmptyPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "auth.json")
	for _, path := range []string{missing, "", "   "} {
		f, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q): %v", path, err)
		}
		if len(f.Entries) != 0 || len(f.Problems) != 0 {
			t.Errorf("Load(%q) = %+v, want an empty file", path, f)
		}
	}
}

func TestLoadTruncatedDocumentDegrades(t *testing.T) {
	// An editor's half-save: the whole document is unusable, so it is a
	// file-level error — and the caller's contract is that this becomes
	// a warning, not a stop (§4.5).
	path := write(t, `{"p":{"type":"api_key","key":"sk-1"`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("a truncated document must be a file-level error")
	}
	var fileErr *Error
	if !errors.As(err, &fileErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if fileErr.Op != "parse" {
		t.Errorf("op = %q, want %q", fileErr.Op, "parse")
	}
	if !strings.Contains(fileErr.Error(), path) {
		t.Errorf("message %q does not name the file", fileErr.Error())
	}

	var warned []error
	stack := LoadStack([]string{path}, func(e error) { warned = append(warned, e) })
	if len(warned) != 1 {
		t.Fatalf("warned %d times, want 1: %v", len(warned), warned)
	}
	if !strings.Contains(fmt.Sprint(warned[0]), path) {
		t.Errorf("warning %q does not name the file", warned[0])
	}
	if len(stack.Providers()) != 0 {
		t.Errorf("providers = %v, want none", stack.Providers())
	}
	if stack.APIKey("p") != "" {
		t.Errorf("APIKey = %q, want empty", stack.APIKey("p"))
	}
	// A file-level failure is not a per-provider problem: there is no
	// provider to blame, so nothing is listed.
	if stack.Problem("p") != nil {
		t.Errorf("problem = %v, want nil", stack.Problem("p"))
	}
}

func TestLoadNonObjectDocument(t *testing.T) {
	for _, doc := range []string{`[1,2]`, `"auth.json"`, `42`} {
		if _, err := Load(write(t, doc)); err == nil {
			t.Errorf("Load(%s) succeeded, want a parse error", doc)
		}
	}
}

func TestLoadBOMPrefix(t *testing.T) {
	// Notepad on Windows writes one; encoding/json rejects it with a
	// confusing "invalid character" without the strip.
	path := write(t, "\xef\xbb\xbf"+`{"p":{"type":"api_key","key":"sk-bom"}}`)
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Entries["p"].Key; got != "sk-bom" {
		t.Errorf("key = %q, want %q", got, "sk-bom")
	}
}

func TestLoadPerProviderProblemsKeepTheRest(t *testing.T) {
	doc := `{
	  "good":  {"type":"api_key","key":"sk-ok"},
	  "oauth": {"type":"oauth","accessToken":"x"},
	  "weird": {"type":"nope"}
	}`
	f, err := Load(write(t, doc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := f.Entries["good"].Key; got != "sk-ok" {
		t.Errorf("good key = %q, want %q", got, "sk-ok")
	}
	if len(f.Entries) != 1 {
		t.Errorf("entries = %v, want only the good one", f.Entries)
	}
	problems := f.Problems
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want 2", problems)
	}
	var oauth *OAuthUnsupportedError
	if !errors.As(problems["oauth"], &oauth) {
		t.Errorf("oauth problem = %v, want *OAuthUnsupportedError", problems["oauth"])
	}
	if got := fmt.Sprint(problems["weird"]); !strings.Contains(got, "unsupported credential type") {
		t.Errorf("weird problem = %q", got)
	}
}

func TestBuildProjectLayerWinsAndHeals(t *testing.T) {
	base := File{
		Entries: map[string]Entry{
			"a": {Type: EntryTypeAPIKey, Key: "base-a"},
			"b": {Type: EntryTypeAPIKey, Key: "base-b"},
		},
		Problems: map[string]error{"c": errors.New("base-c is broken")},
	}
	project := File{
		Entries: map[string]Entry{
			"b": {Type: EntryTypeAPIKey, Key: "project-b"}, // wholesale replace
			"c": {Type: EntryTypeAPIKey, Key: "project-c"}, // heals the problem
		},
		Problems: map[string]error{"a": errors.New("project-a is broken")}, // shadows
	}
	stack := Build(base, project)

	if _, ok := stack.Entry("a"); ok {
		t.Error("provider a still has an entry; the project layer must shadow it")
	}
	if got := stack.Problem("a"); got == nil || !strings.Contains(got.Error(), "project-a") {
		t.Errorf("provider a problem = %v, want the project layer's", got)
	}
	if got := stack.APIKey("b"); got != "project-b" {
		t.Errorf("provider b key = %q, want %q", got, "project-b")
	}
	if got := stack.APIKey("c"); got != "project-c" {
		t.Errorf("provider c key = %q, want %q (the project entry must heal the base problem)", got, "project-c")
	}
	if stack.Problem("c") != nil {
		t.Errorf("provider c problem = %v, want nil", stack.Problem("c"))
	}
	want := []string{"b", "c"}
	if got := stack.Providers(); !slices.Equal(got, want) {
		t.Errorf("providers = %v, want %v", got, want)
	}
}

func TestLoadStackWarnsAndContinues(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	broken := filepath.Join(home, "auth.json")
	good := filepath.Join(proj, "auth.json")
	if err := os.WriteFile(broken, []byte("{trunc"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good, []byte(`{"p":{"type":"api_key","key":"sk-proj"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var warned []error
	stack := LoadStack([]string{broken, good}, func(e error) { warned = append(warned, e) })
	if len(warned) != 1 {
		t.Fatalf("warned %d times, want 1: %v", len(warned), warned)
	}
	if !strings.Contains(fmt.Sprint(warned[0]), broken) {
		t.Errorf("warning %q does not name the broken file", warned[0])
	}
	if got := stack.APIKey("p"); got != "sk-proj" {
		t.Errorf("key = %q, want %q — one broken layer must not sink the other", got, "sk-proj")
	}
}

func TestStackAccessors(t *testing.T) {
	stack := Build(File{Entries: map[string]Entry{
		"zebra": {Type: EntryTypeAPIKey, Key: "sk-z"},
		"alpha": {Type: EntryTypeAPIKey}, // entry without a key
		"mike":  {Type: EntryTypeAPIKey, Key: "sk-m"},
	}})

	want := []string{"alpha", "mike", "zebra"}
	if got := stack.Providers(); !slices.Equal(got, want) {
		t.Errorf("providers = %v, want %v (lexical)", got, want)
	}
	if _, ok := stack.Entry("nope"); ok {
		t.Error("Entry(nope) reported a credential")
	}
	if got := stack.APIKey("alpha"); got != "" {
		t.Errorf("APIKey(alpha) = %q, want empty (entry without a key)", got)
	}
	if got := stack.APIKey("mike"); got != "sk-m" {
		t.Errorf("APIKey(mike) = %q, want %q", got, "sk-m")
	}
	if got := stack.APIKey("nope"); got != "" {
		t.Errorf("APIKey(nope) = %q, want empty", got)
	}
	if stack.Problem("mike") != nil {
		t.Errorf("Problem(mike) = %v, want nil", stack.Problem("mike"))
	}
	if got := stack.Problems(); len(got) != 0 {
		t.Errorf("Problems() = %v, want none", got)
	}
}

func TestProblemsAreSortedByProvider(t *testing.T) {
	stack := Build(File{Problems: map[string]error{
		"zebra": errors.New("z"),
		"alpha": errors.New("a"),
		"mike":  errors.New("m"),
	}})
	if got := stack.Problems(); len(got) != 3 || got[0].Error() != "a" || got[2].Error() != "z" {
		t.Errorf("Problems() = %v, want [a m z]", got)
	}
	if len(stack.Providers()) != 0 {
		t.Errorf("providers = %v, want none", stack.Providers())
	}
}

func TestOAuthRejectionNamesTheProvider(t *testing.T) {
	f, err := Load(write(t, `{"my-proxy":{"type":"oauth","accessToken":"x"}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var oauth *OAuthUnsupportedError
	if !errors.As(f.Problems["my-proxy"], &oauth) {
		t.Fatalf("problem = %v, want *OAuthUnsupportedError", f.Problems["my-proxy"])
	}
	if oauth.Provider != "my-proxy" {
		t.Errorf("provider = %q, want %q", oauth.Provider, "my-proxy")
	}
	msg := oauth.Error()
	for _, want := range []string{"my-proxy", "oauth", EntryTypeAPIKey} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	// A nested path: MkdirAll(0700) is part of the contract.
	path := filepath.Join(t.TempDir(), "nested", "deeper", "auth.json")
	v1 := []byte(`{"p":{"type":"api_key","key":"sk-1"}}`)
	if err := WriteFile(path, v1); err != nil {
		t.Fatalf("WriteFile v1: %v", err)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatalf("Load after v1: %v", err)
	}
	if got := f.Entries["p"].Key; got != "sk-1" {
		t.Errorf("key = %q, want %q", got, "sk-1")
	}

	v2 := []byte(`{"p":{"type":"api_key","key":"sk-2"},"q":{"type":"api_key","key":"sk-3"}}`)
	if err := WriteFile(path, v2); err != nil {
		t.Fatalf("WriteFile v2: %v", err)
	}
	f, err = Load(path)
	if err != nil {
		t.Fatalf("Load after v2: %v", err)
	}
	if got := f.Entries["p"].Key; got != "sk-2" || f.Entries["q"].Key != "sk-3" {
		t.Errorf("entries = %v, want the v2 document", f.Entries)
	}

	// The tmp sibling must not survive the rename.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("temp file left behind (stat error: %v)", err)
	}

	// Windows has no POSIX mode bits beyond read-only, so 0600 is only
	// assertable where it means something.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("mode = %#o, want 0600", mode)
		}
	}
}

func TestWriteFileEmptyPath(t *testing.T) {
	err := WriteFile("  ", []byte("{}"))
	if err == nil {
		t.Fatal("WriteFile with an empty path succeeded")
	}
	if !strings.Contains(err.Error(), "empty path") {
		t.Errorf("error = %v, want it to mention the empty path", err)
	}
}

func TestLoadStackWithoutPaths(t *testing.T) {
	// No warn callback, no paths: nothing to report, nothing to load.
	stack := LoadStack(nil, nil)
	if len(stack.Providers()) != 0 || len(stack.Problems()) != 0 {
		t.Errorf("stack = %+v, want empty", stack)
	}
	if got := Build(); len(got.Providers()) != 0 {
		t.Errorf("Build() = %v, want empty", got)
	}
}

// The two documents the concurrent test alternates between: different
// shapes and different lengths, so a truncated or mixed read is
// detectable as a wrong entry set, not just a decode failure.
const (
	docA = `{"alpha":{"type":"api_key","key":"a1"}}`
	docB = `{"beta":{"type":"api_key","key":"b1"},"gamma":{"type":"api_key","key":"g1"}}`
)

// whole reports whether f is one of the two complete states the writer
// publishes. There is no third possibility: the replace is a single
// atomic rename (no remove-then-rename window), and a reader that
// opened the old file before it lands reads the old inode to the end.
// An empty state here would mean the path went missing — a bug.
func whole(f File) bool {
	switch {
	case len(f.Entries) == 1 && f.Entries["alpha"].Key == "a1" && len(f.Problems) == 0:
		return true
	case len(f.Entries) == 2 && f.Entries["beta"].Key == "b1" && f.Entries["gamma"].Key == "g1" && len(f.Problems) == 0:
		return true
	}
	return false
}

func TestConcurrentReadersSeeWholeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := WriteFile(path, []byte(docA)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		readers    = 8
		iterations = 200
		writes     = 300
	)

	var (
		mu        sync.Mutex
		writeErr  error
		readErrs  []error
		sawA      bool
		sawB      bool
		recordErr = func(err error) {
			mu.Lock()
			defer mu.Unlock()
			if readErrs == nil {
				readErrs = []error{}
			}
			if len(readErrs) < 8 {
				readErrs = append(readErrs, err)
			}
		}
	)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range writes {
			doc := docA
			if i%2 == 1 {
				doc = docB
			}
			if err := WriteFile(path, []byte(doc)); err != nil {
				mu.Lock()
				if writeErr == nil {
					writeErr = err
				}
				mu.Unlock()
				return
			}
		}
	}()

	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				f, err := Load(path)
				if err != nil {
					recordErr(fmt.Errorf("read failed: %w", err))
					continue
				}
				if !whole(f) {
					recordErr(fmt.Errorf("partial read: %+v", f))
					continue
				}
				mu.Lock()
				if len(f.Entries) == 1 {
					sawA = true
				}
				if len(f.Entries) == 2 {
					sawB = true
				}
				mu.Unlock()
				// The plan's premise (§4.5) is a reader that holds the
				// file for microseconds between long gaps — Load at
				// startup, an occasional reload. A tight loop with no
				// pause would hold the file ~100% of the time and
				// starve even a correct retry schedule, which is the
				// sustained-contention regime the archived lock owns.
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()

	if writeErr != nil {
		t.Errorf("writer failed (the retry schedule must clear a microsecond reader): %v", writeErr)
	}
	for _, err := range readErrs {
		t.Errorf("reader saw a torn file: %v", err)
	}
	// Sanity: the interleaving actually happened, or the assertions
	// above are vacuous.
	if !sawA || !sawB {
		t.Errorf("observed states: A=%v B=%v, want both", sawA, sawB)
	}
}
