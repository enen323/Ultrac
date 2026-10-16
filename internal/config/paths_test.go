package config

import (
	"path/filepath"
	"testing"
)

// clearHomeEnv empties the relocation knob so a test sees the defaults.
func clearHomeEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvConfigDir, "")
	t.Setenv(EnvProviderManagedByHost, "")
	t.Setenv("ANTHROPIC_UNIX_SOCKET", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "")
}

func TestConfigHomeDefaultsToUltrac(t *testing.T) {
	clearHomeEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows reads this one

	if got, want := ConfigHome(), filepath.Join(home, ".ultrac"); got != want {
		t.Fatalf("ConfigHome() = %q, want %q", got, want)
	}
	// The new stack's AgentDir is the same directory under its own name.
	if AgentDir() != ConfigHome() {
		t.Fatalf("AgentDir() = %q, want %q (since 2026-10-16 they coincide)", AgentDir(), ConfigHome())
	}
	if got, want := SessionsDir(), filepath.Join(ConfigHome(), "sessions"); got != want {
		t.Fatalf("SessionsDir() = %q, want %q", got, want)
	}
}

func TestEnvConfigDirRelocatesEverything(t *testing.T) {
	// One knob moves settings AND sessions: a partial relocation would leave
	// credentials and transcripts in two different homes.
	clearHomeEnv(t)
	root := t.TempDir()
	t.Setenv(EnvConfigDir, filepath.Join(root, "portable"))

	if got := ConfigHome(); got != filepath.Join(root, "portable") {
		t.Fatalf("ConfigHome() = %q", got)
	}
	if got, want := SessionsDir(), filepath.Join(root, "portable", "sessions"); got != want {
		t.Fatalf("SessionsDir() = %q, want %q", got, want)
	}
}

func TestSourceFilesUseUltracLayout(t *testing.T) {
	clearHomeEnv(t)
	root := t.TempDir()
	t.Setenv(EnvConfigDir, filepath.Join(root, ".ultrac"))
	proj := t.TempDir()

	// The user layer is ~/.ultrac/settings.json — where the real DeepSeek
	// endpoint and credentials live (the 2026-10-16 ruling kept them put).
	if got, want := SourceFile(SourceUser, proj), filepath.Join(root, ".ultrac", "settings.json"); got != want {
		t.Errorf("user settings = %q, want %q", got, want)
	}
	if got, want := SourceFile(SourceProject, proj), filepath.Join(proj, ".ultrac", "settings.json"); got != want {
		t.Errorf("project settings = %q, want %q", got, want)
	}
	if got, want := SourceFile(SourceLocal, proj), filepath.Join(proj, ".ultrac", "settings.local.json"); got != want {
		t.Errorf("local settings = %q, want %q", got, want)
	}
	if DirName != ".ultrac" {
		t.Errorf("DirName = %q, want .ultrac", DirName)
	}
}

func TestExpandTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := expandTilde("~"); got != home {
		t.Fatalf("expandTilde(~) = %q, want %q", got, home)
	}
	if got := expandTilde("~/agent"); got != filepath.Join(home, "agent") {
		t.Fatalf("expandTilde(~/agent) = %q", got)
	}
	if got := expandTilde(`C:\plain`); got != `C:\plain` {
		t.Fatalf("expandTilde of a plain path must pass through, got %q", got)
	}
}
