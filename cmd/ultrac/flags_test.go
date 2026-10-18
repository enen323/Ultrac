package main

// flags_test.go — the P5 flag surface (pi semantics, plan §6.3).

import (
	"testing"
)

func TestParseFlagsPrintAndMode(t *testing.T) {
	f, err := parseFlags([]string{"-p", "--mode", "json", "hello", "world"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.Print {
		t.Error("-p did not set Print")
	}
	if f.Mode != "json" {
		t.Errorf("mode = %q, want json", f.Mode)
	}
	if f.prompt() != "hello world" {
		t.Errorf("prompt = %q, want %q", f.prompt(), "hello world")
	}
}

func TestParseFlagsDefaults(t *testing.T) {
	f, err := parseFlags([]string{"just a prompt"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.Print {
		t.Error("a prompt alone must not require -p (pi: resolveAppMode)")
	}
	if f.Mode != "text" {
		t.Errorf("default mode = %q, want text", f.Mode)
	}
	if f.Continue || f.NoTranscript || f.Debug {
		t.Error("switches must default to false")
	}
}

func TestParseFlagsDashDashEndsFlags(t *testing.T) {
	// A prompt that looks like a flag after -- must survive verbatim: the
	// model may legitimately be asked about "--mode".
	f, err := parseFlags([]string{"--", "--mode", "json", "literal"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.Mode != "text" {
		t.Errorf("mode = %q, want text (the --mode after -- is prompt text)", f.Mode)
	}
	if f.prompt() != "--mode json literal" {
		t.Errorf("prompt = %q", f.prompt())
	}
}

func TestParseFlagsInlineValues(t *testing.T) {
	f, err := parseFlags([]string{"--model=deepseek/deepseek-chat", "--cwd=D:\\repo", "--mode=json"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.Model != "deepseek/deepseek-chat" {
		t.Errorf("model = %q", f.Model)
	}
	if f.CWD != `D:\repo` {
		t.Errorf("cwd = %q", f.CWD)
	}
	if f.Mode != "json" {
		t.Errorf("mode = %q", f.Mode)
	}
}

func TestParseFlagsUnknownFlagIsError(t *testing.T) {
	// The permission flags are gone with the permission layer; naming them
	// must fail loudly rather than silently dropping the user's intent.
	for _, arg := range []string{"--permission-mode", "--dangerously-skip-permissions", "--add-dir", "--nope"} {
		if _, err := parseFlags([]string{arg, "x"}); err == nil {
			t.Errorf("parseFlags(%q) succeeded, want an error", arg)
		}
	}
}

func TestParseFlagsModeAcceptsRPC(t *testing.T) {
	// rpc landed with P6 (rpc.go); it is the headless JSONL protocol mode,
	// and `--mode rpc` with no prompt is a complete invocation.
	f, err := parseFlags([]string{"--mode", "rpc"})
	if err != nil {
		t.Fatalf("parseFlags(--mode rpc): %v", err)
	}
	if f.Mode != "rpc" {
		t.Errorf("mode = %q, want rpc", f.Mode)
	}
	if f.prompt() != "" {
		t.Errorf("prompt = %q, want empty (prompts come from the command stream)", f.prompt())
	}
}

func TestParseFlagsMissingValue(t *testing.T) {
	if _, err := parseFlags([]string{"--model"}); err == nil {
		t.Error("--model without a value must be an error")
	}
}

func TestParseFlagsContinueShort(t *testing.T) {
	f, err := parseFlags([]string{"-c", "next"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.Continue {
		t.Error("-c did not set Continue")
	}
	if f.prompt() != "next" {
		t.Errorf("prompt = %q", f.prompt())
	}
}

func TestSignalExitCode(t *testing.T) {
	cases := map[string]int{
		"interrupt":  130,
		"hangup":     129,
		"terminated": 143,
		"":           143,
	}
	for name, want := range cases {
		if got := signalExitCode(name); got != want {
			t.Errorf("signalExitCode(%q) = %d, want %d", name, got, want)
		}
	}
}
