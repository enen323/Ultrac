// Package tool holds the eight pi coding tools ported to Go: read, bash,
// powershell, edit, write, grep, find and ls
// (docs/pi-core-harness-plan.md §3.4, phase P3).
//
// # What this package is
//
// This is the tool half of the pi harness port. Every tool is an
// agent.AgentTool[In] — the C3 contract frozen in internal/agent during P0 —
// and self-registers from its init() the same way the provider registry and
// the old builtin package do: adding a tool is one file plus one Register
// call, with zero engine changes (plan G4).
//
// # Implementation logic was ported, not rewritten
//
// The behaviours that carry hard-won lessons from the pre-port engine are
// kept deliberately: bash collects subprocess output through temp files
// (a bytes.Buffer makes os/exec's Wait hang on grandchildren holding the
// pipe — plan R6), and the edit tool's diff uses the hand-rolled LCS
// renderer from internal/tool/builtin/diff.go rather than a new dependency
// (plan §5.2: "若已够用则不引").
//
// # Layering
//
// tool imports agent for the AgentTool contract type only. That is not the
// "tool must not import agent" cycle the plan's §4.2 diagram warns about:
// agent never imports tool (tools are injected through agent.Registry), so
// the edge is one-way. The constraint's intent — a tool must never call
// back into engine machinery — holds absolutely: nothing in this package
// calls an agent function. The alternative (moving C3 out of agent) would
// reopen a contract frozen in P0.
//
// # cwd is closed over, not passed per call
//
// pi resolves paths against ctx.cwd with the construction-time cwd as
// fallback (tools/path-utils.ts resolveToCwd callers). The Go port's
// Execute signature carries no per-call cwd — the harness does not port
// pi's ExtensionContext — so each tool closes over the cwd it was built
// with and the wiring passes the session's working directory to All(cwd).
package tool

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"ultrac/internal/agent"
	"ultrac/internal/ai"
)

// base64Std encodes bytes for image blocks (base64 rather than raw: a tool
// result is a string, and raw bytes would corrupt the transcript).
func base64Std(raw []byte) string { return base64.StdEncoding.EncodeToString(raw) }

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// factory builds one tool for a specific working directory.
type factory struct {
	name string
	make func(cwd string) agent.Tool
}

var (
	regMu sync.RWMutex
	regs  []factory
)

// Register adds a tool factory under a name. Duplicate names panic, mirroring
// both the old builtin.RegisterBuiltin and the provider registry: a collision
// is a programming error, and failing at init beats silently shadowing a
// tool the model was promised.
//
// Every built-in tool file calls Register from its init(); the make function
// builds the typed AgentTool and erases it for the runtime.
func Register(name string, make func(cwd string) agent.Tool) {
	if name == "" {
		panic("tool: Register called with an empty name")
	}
	if make == nil {
		panic("tool: Register(" + name + ") called with a nil factory")
	}
	regMu.Lock()
	defer regMu.Unlock()
	for _, f := range regs {
		if f.name == name {
			panic("tool: duplicate tool " + name)
		}
	}
	regs = append(regs, factory{name: name, make: make})
}

// All builds every registered tool for cwd, sorted by name.
//
// Tools are built per call rather than cached: construction is a struct
// literal, and a fresh build per run keeps cwd (and any future per-run
// option) from leaking between sessions.
func All(cwd string) []agent.Tool {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]agent.Tool, 0, len(regs))
	for _, f := range regs {
		out = append(out, f.make(cwd))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Names returns the registered tool names, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(regs))
	for _, f := range regs {
		out = append(out, f.name)
	}
	sort.Strings(out)
	return out
}

// Lookup builds the named tool for cwd.
func Lookup(cwd, name string) (agent.Tool, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	for _, f := range regs {
		if f.name == name {
			return f.make(cwd), true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Path resolution (port of pi's tools/path-utils.ts + utils/paths.ts)
// ---------------------------------------------------------------------------

// unicodeSpaces matches the space variants pi normalizes before resolving a
// path (utils/paths.ts UNICODE_SPACES). A model that emits U+00A0 in a path
// would otherwise get an ENOENT that looks like a missing file.
const unicodeSpaces = "\u00A0\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200A\u202F\u205F\u3000"

// resolveToCwd turns a model-supplied path into an absolute one.
//
// Port of pi's resolveToCwd → resolvePath(filePath, cwd,
// {normalizeUnicodeSpaces: true, stripAtPrefix: true}) (path-utils.ts:74-76,
// utils/paths.ts:104-118): unicode spaces become regular spaces, a leading
// "@" (pi's CLI @file syntax) is stripped, Git-Bash-style drive paths
// (/c/foo) are converted to native Windows form, "~" expands to the home
// directory, and the result is resolved against cwd.
//
// The macOS-only read-path variants (NFD filenames, curly-quote screenshot
// names) are deliberately not ported: this project targets Windows first.
func resolveToCwd(p, cwd string) string {
	s := normalizeInputPath(p)
	if s == "" {
		return ""
	}
	if s == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Clean(home)
		}
	}
	if strings.HasPrefix(s, "~/") || (windowsOS && strings.HasPrefix(s, `~\`)) {
		if home, err := os.UserHomeDir(); err == nil {
			rest := s[2:]
			return filepath.Clean(filepath.Join(home, filepath.FromSlash(rest)))
		}
	}
	if strings.HasPrefix(s, "file://") {
		s = strings.TrimPrefix(s, "file://")
		// A file:// URL percent-encodes; decode the common escapes so the
		// path stays usable. Full URL decoding is out of scope — pi's
		// fileURLToPath does more, but models rarely send file:// paths.
		s = strings.NewReplacer("%20", " ", "%23", "#", "%3F", "?", "%25", "%").Replace(s)
	}
	if filepath.IsAbs(s) {
		return filepath.Clean(s)
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	return filepath.Clean(filepath.Join(cwd, s))
}

// normalizeInputPath applies pi's normalizePath input normalizations that do
// not need the filesystem (utils/paths.ts:82-102).
func normalizeInputPath(p string) string {
	if p == "" {
		return ""
	}
	if strings.ContainsAny(p, unicodeSpaces) {
		var b strings.Builder
		b.Grow(len(p))
		for _, r := range p {
			if strings.ContainsRune(unicodeSpaces, r) {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(r)
		}
		p = b.String()
	}
	p = strings.TrimPrefix(p, "@")
	if windowsOS {
		p = normalizeWindowsShellPath(p)
	}
	return p
}

// normalizeWindowsShellPath converts Git Bash / MSYS / Cygwin / WSL drive
// paths to a form native Windows APIs accept (utils/paths.ts:68-76).
// "/d/repo" becomes "D:\repo"; anything already native passes through.
func normalizeWindowsShellPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.Contains(p, `\`) {
		return p
	}
	rest := p[1:]
	rest = strings.TrimPrefix(rest, "mnt/")
	rest = strings.TrimPrefix(rest, "cygdrive/")
	if len(rest) < 2 || rest[1] != '/' {
		return p
	}
	drive := rest[0]
	if drive < 'a' || drive > 'z' {
		if drive < 'A' || drive > 'Z' {
			return p
		}
	}
	suffix := strings.ReplaceAll(rest[2:], "/", `\`)
	return strings.ToUpper(string(drive)) + `:\` + suffix
}

// toSlash renders a path with forward slashes for tool output, so results
// read the same on every platform (pi relativizes to posix separators in
// grep/find).
func toSlash(p string) string { return filepath.ToSlash(p) }

// windowsOS reports whether the build targets Windows. It exists so the
// path/shell helpers can stay platform-specific without per-file build
// tags: on Unix the two branches collapse to no-ops.
var windowsOS = runtime.GOOS == "windows"

// ---------------------------------------------------------------------------
// Small shared helpers
// ---------------------------------------------------------------------------

// jsonSchemaPrefer is the constrainedSampling value read, bash, edit and
// write declare in pi (e.g. read.ts:101): prefer schema-constrained
// sampling, do not require it.
func jsonSchemaPrefer() *ai.ConstrainedSampling {
	return &ai.ConstrainedSampling{Kind: "json_schema", Strict: "prefer"}
}

// textResult builds a single-text-block agent.Result.
func textResult(text string) agent.Result { return agent.NewTextResult(text) }

// errResult builds an error result. Failures travel as Result.IsError, not
// as a Go error: the model is supposed to read them and adjust. Execute
// returning a Go error is for the cases where the loop itself should see a
// thrown tool (pi: types.ts:479-482); both paths land as an isError tool
// result for the model.
func errResult(text string) agent.Result {
	r := agent.NewTextResult(text)
	r.IsError = true
	return r
}

// errAborted is the in-queue sentinel for a cancelled mutation: it must
// distinguish "the caller went away" from a filesystem error, which still
// surfaces verbatim.
var errAborted = errAbortedSentinel{}

type errAbortedSentinel struct{}

func (errAbortedSentinel) Error() string { return "Operation aborted" }
