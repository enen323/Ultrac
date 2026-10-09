package builtin

import (
	"os"
	"path/filepath"
	"strings"

	"ultrac/internal/contract/tool"
)

// resolvePath turns what the model sent into an absolute path.
//
// The model is told to send absolute paths, and mostly does. The three cases
// here are the ones where it does not and the tool should still work rather
// than fail with a confusing ENOENT:
//
//   - `~` and `~/x` — a home-relative path.
//   - a relative path — resolved against the session's working directory, not
//     the process's. They differ whenever the engine has chdir'd or is serving
//     a request for another directory, and resolving against the wrong one is
//     how a write lands in the wrong checkout.
//   - an already-absolute path — cleaned.
func resolvePath(uc *tool.UseContext, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			rest := strings.TrimPrefix(strings.TrimPrefix(p[1:], "/"), `\`)
			return filepath.Clean(filepath.Join(home, rest))
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	base := ""
	if uc != nil {
		base = uc.WorkDir
	}
	if base == "" {
		base, _ = os.Getwd()
	}
	return filepath.Clean(filepath.Join(base, p))
}

// workDir returns the session's working directory, falling back to the
// process's. Tools run against the session's, which is what the model was told.
func workDir(uc *tool.UseContext) string {
	if uc != nil && uc.WorkDir != "" {
		return uc.WorkDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// displayPath shortens a path for a message without making it ambiguous.
func displayPath(p string) string {
	if p == "" {
		return "(none)"
	}
	return filepath.ToSlash(p)
}
