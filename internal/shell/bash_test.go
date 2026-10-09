package shell

import "testing"

// TestClassification is the test the DoD asks for: dangerous patterns are
// recognised structurally, from the AST, not by substring search.
func TestClassification(t *testing.T) {
	cases := []struct {
		command string
		class   Class
	}{
		{"ls -la", ClassReadOnly},
		{"cat go.mod", ClassReadOnly},
		{"git status", ClassReadOnly},
		{"git log --oneline -5", ClassReadOnly},
		{"git branch", ClassReadOnly},
		{"grep -rn foo .", ClassReadOnly},
		{"ls | grep go", ClassReadOnly},
		{"find . -name '*.go'", ClassReadOnly},

		// The case a substring matcher gets wrong in the dangerous direction:
		// the text "rm -rf" appears, but only as an argument.
		{"echo 'rm -rf /'", ClassReadOnly},
		{"grep -rn 'rm -rf' .", ClassReadOnly},

		{"rm -rf /", ClassDestructive},
		{"rm file.txt", ClassWrite},
		{"find . -delete", ClassDestructive},
		{"git reset --hard HEAD~3", ClassDestructive},
		{"git clean -fd", ClassDestructive},
		{"git push --force origin main", ClassDestructive},
		{"dd if=/dev/zero of=/dev/sda", ClassDestructive},

		{"git commit -m x", ClassWrite},
		{"mkdir -p a/b", ClassWrite},
		{"go build ./...", ClassWrite},

		{"curl https://example.com", ClassNetwork},
		{"ssh host uptime", ClassNetwork},

		{"python -c 'print(1)'", ClassInterpreter},
		{"node script.js", ClassInterpreter},
		{"bash -c 'rm -rf /'", ClassInterpreter},

		{"frobnicate --now", ClassUnknown},
	}

	for _, c := range cases {
		got := Analyze(c.command)
		if got.Class != c.class {
			t.Errorf("Analyze(%q) = %s, want %s (reasons: %v)",
				c.command, got.Class, c.class, got.Reasons)
		}
	}
}

func TestBashDynamicAndRedirectDisqualifyReadOnly(t *testing.T) {
	cases := []struct {
		command  string
		readOnly bool
	}{
		{"ls", true},
		{"ls $HOME", false},             // dynamic word
		{"echo $(whoami)", false},       // command substitution
		{"echo hi > out.txt", false},    // redirection to a real file
		{"cat f > g", false},            // redirection to a real file
		{"sleep 1 &", false},            // background
		{"if true; then ls; fi", false}, // a predicate is not a reader

		// Redirections that write nowhere. These are everywhere in real
		// commands and must not be mistaken for writes.
		{"ls -la 2>/dev/null", true},
		{"cat f 2>/dev/null", true},
		{"ls > NUL", true},

		// Sequences of read-only commands are read-only. Compound is set, but
		// that only gates allow rules, not this predicate.
		{"git status && git diff", true},
		{"ls; pwd", true},
		{"ls | grep go", true},
	}
	for _, c := range cases {
		got := Analyze(c.command)
		if got.ReadOnly() != c.readOnly {
			t.Errorf("Analyze(%q).ReadOnly() = %v, want %v (%s)",
				c.command, got.ReadOnly(), c.readOnly, got.Explain())
		}
	}
}

// TestCompoundIsSetButNotForSingleCommands pins the property allow rules
// depend on: a prefix rule must never be matched against a compound line.
func TestCompoundIsSetButNotForSingleCommands(t *testing.T) {
	if got := Analyze("git status"); got.Compound {
		t.Error("a simple command was marked compound")
	}
	for _, cmd := range []string{"git status && rm -rf /", "ls | grep x", "ls; pwd"} {
		if got := Analyze(cmd); !got.Compound {
			t.Errorf("%q was not marked compound", cmd)
		}
	}
}

func TestBashParseFailureIsNotAllowed(t *testing.T) {
	got := Analyze("for do done |")
	if got.Class != ClassUnknown || got.ParseError == nil {
		t.Fatalf("unparseable command classified as %s (err=%v)", got.Class, got.ParseError)
	}
	if got.ReadOnly() {
		t.Fatal("unparseable command reported as read-only")
	}
}
