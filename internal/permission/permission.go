package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"ultrac/internal/contract/tool"
	"ultrac/internal/shell"
)

// Kind is the coarse category of a call.
//
// The tool's own predicates already say whether a *particular* call reads or
// writes; Kind exists for the cases where the category itself carries policy,
// such as "plan mode forbids edits" and "a shell command needs structural
// classification".
type Kind int

const (
	// KindRead only reads.
	KindRead Kind = iota
	// KindEdit mutates files.
	KindEdit
	// KindBash runs a shell command.
	KindBash
	// KindInternal touches nothing outside the session: the todo list, the
	// tool registry, a task's own output. It is separate from KindRead
	// because "session bookkeeping" and "reading the disk" are different
	// claims, and collapsing them would make a reviewer assume the wrong one.
	KindInternal
	// KindOther is everything else: network, interaction, subagents.
	KindOther
)

func (k Kind) String() string {
	switch k {
	case KindRead:
		return "read"
	case KindEdit:
		return "edit"
	case KindBash:
		return "bash"
	case KindInternal:
		return "internal"
	default:
		return "other"
	}
}

// Request is one call awaiting a verdict.
type Request struct {
	Tool  string
	Input json.RawMessage
	Kind  Kind

	// Content is what a rule's parenthesised part is compared against: the
	// file path for file tools, the command line for Bash. Empty means the
	// rule can only match at the tool level.
	Content string

	// ContentAlt is a second form of the same thing, tried when Content does
	// not match.
	//
	// It exists for paths, and it is not a convenience: rules in settings.json
	// are written relative to the project (`Read(go.mod)`), while the engine
	// works in absolute paths. Without a second candidate, a rule a user wrote
	// by hand would silently never match, which is the worst possible failure
	// for a permission rule — the user believes they have restricted something.
	ContentAlt string

	// Paths are the concrete paths this call touches, already absolute. They
	// are validated against the working directories and the protected-path
	// list.
	Paths []string

	// ReadOnly and Destructive come from the tool's own predicates, which
	// know the input. The engine does not second-guess them downwards: a tool
	// that says destructive is treated as destructive even in acceptEdits.
	ReadOnly    bool
	Destructive bool

	// Bash is the structural analysis, present when Kind is KindBash.
	Bash shell.Analysis
}

// Result is the verdict.
type Result struct {
	Decision tool.Decision
	Reason   string
	// Rule is the rule that produced the verdict, when one did. The user
	// asking "why" deserves the rule, not just the answer.
	Rule *Rule
	// Warning is extra context shown before a prompt — "this deletes files"
	// is more useful than "allow?".
	Warning string
	// UpdatedInput lets policy rewrite a call before it runs, matching the
	// contract's PermissionResult. The engine substitutes it when set.
	UpdatedInput json.RawMessage
}

// Allowed reports whether the call may proceed without asking.
func (r Result) Allowed() bool { return r.Decision == tool.DecisionAllow }

// Denied reports whether the call is refused.
func (r Result) Denied() bool { return r.Decision == tool.DecisionDeny }

// Engine decides calls for one session.
//
// It holds no mutable state apart from denial counters, so it is safe for
// concurrent use.
type Engine struct {
	Mode      Mode
	Rules     *Set
	WorkDir   string
	ExtraDirs []string

	// ConfigDir is ~/.ultrac. Files inside it are protected because
	// settings.json is what configures permission itself, so a tool that can
	// write it can grant itself anything.
	ConfigDir string
}

// NewEngine builds an engine with default rules.
func NewEngine(mode Mode, workDir string) *Engine {
	return &Engine{
		Mode:    mode,
		Rules:   NewSet(),
		WorkDir: workDir,
	}
}

// Check produces a verdict for one call.
//
// The order is fixed and each step exists for a reason:
//
//  1. An explicit deny rule wins over everything, including bypass mode. A
//     deny rule that bypass mode could ignore would not be a deny rule.
//  2. Bypass mode allows what is left.
//  3. Plan mode refuses anything that is not read-only.
//  4. Ask rules, then allow rules, then the mode's own default.
//
// Steps 3 and 4 are in that order deliberately: in plan mode even an allow
// rule does not grant a write, because the user asked for a mode that cannot
// write.
func (e *Engine) Check(_ context.Context, req Request) Result {
	if rule, ok := e.matchRule(req, BehaviorDeny); ok {
		r := rule
		return Result{
			Decision: tool.DecisionDeny,
			Reason:   fmt.Sprintf("denied by rule %q from %s", r.String(), r.Source),
			Rule:     &r,
		}
	}

	if e.Mode == ModeBypassPermissions {
		return Result{Decision: tool.DecisionAllow, Reason: "permission mode bypassPermissions"}
	}

	if e.Mode == ModePlan && !req.ReadOnly {
		return Result{
			Decision: tool.DecisionDeny,
			Reason:   "plan mode is read-only; " + req.Tool + " would change something",
		}
	}

	if rule, ok := e.matchRule(req, BehaviorAsk); ok {
		r := rule
		return e.finalise(req, Result{
			Decision: tool.DecisionAsk,
			Reason:   fmt.Sprintf("matched ask rule %q from %s", r.String(), r.Source),
			Rule:     &r,
		})
	}

	// An allow rule is a statement about the text it matched. For a shell
	// command that text is a prefix, so honouring it on a compound line would
	// let `Bash(git:*)` approve `git status; rm -rf /`. Compound commands skip
	// the allow rules and go to the classifier, which reads the structure.
	allowRulesApply := !(req.Kind == KindBash && req.Bash.Compound)
	if allowRulesApply {
		if rule, ok := e.matchRule(req, BehaviorAllow); ok {
			r := rule
			return Result{
				Decision: tool.DecisionAllow,
				Reason:   fmt.Sprintf("matched allow rule %q from %s", r.String(), r.Source),
				Rule:     &r,
			}
		}
	}

	if req.Destructive {
		return e.finalise(req, Result{
			Decision: tool.DecisionAsk,
			Reason:   req.Tool + " is marked destructive",
			Warning:  "this call is irreversible",
		})
	}

	switch req.Kind {
	case KindRead:
		return Result{Decision: tool.DecisionAllow, Reason: "read-only call"}

	case KindInternal:
		// Session bookkeeping: it changes nothing the user has to review.
		return Result{Decision: tool.DecisionAllow, Reason: "changes nothing outside the session"}

	case KindBash:
		return e.checkBash(req)

	case KindEdit:
		if reason := e.pathProblem(req); reason != "" {
			return e.finalise(req, Result{Decision: tool.DecisionAsk, Reason: reason})
		}
		if e.Mode == ModeAcceptEdits {
			return Result{Decision: tool.DecisionAllow, Reason: "acceptEdits mode approves file edits"}
		}
		return e.finalise(req, Result{
			Decision: tool.DecisionAsk,
			Reason:   "file edits require approval in default mode",
		})

	default:
		return e.finalise(req, Result{
			Decision: tool.DecisionAsk,
			Reason:   req.Tool + " is not in the auto-approved set",
		})
	}
}

// matchRule finds the first rule with the given behaviour that applies.
//
// Both content forms are tried before a rule is declared not to match: a rule
// may be written against the project-relative path or the absolute one, and
// which one depends on who wrote it.
func (e *Engine) matchRule(req Request, behavior Behavior) (Rule, bool) {
	if r, ok := e.Rules.MatchFor(req.Tool, req.Content, behavior); ok {
		return r, true
	}
	if req.ContentAlt != "" && req.ContentAlt != req.Content {
		return e.Rules.MatchFor(req.Tool, req.ContentAlt, behavior)
	}
	return Rule{}, false
}

// checkBash turns the structural analysis into a verdict.
func (e *Engine) checkBash(req Request) Result {
	a := req.Bash

	if a.ParseError != nil {
		return e.finalise(req, Result{
			Decision: tool.DecisionAsk,
			Reason:   "the command could not be parsed: " + a.ParseError.Error(),
			Warning:  "an unparseable command cannot be checked",
		})
	}

	if a.ReadOnly() {
		return Result{Decision: tool.DecisionAllow, Reason: "read-only shell command"}
	}

	// A destructive command is never auto-approved, whatever the mode says.
	// acceptEdits covers file edits the user can see in a diff; it is not a
	// licence to run `rm -rf`.
	if a.Class == shell.ClassDestructive {
		return e.finalise(req, Result{
			Decision: tool.DecisionAsk,
			Reason:   "destructive shell command",
			Warning:  a.Explain(),
		})
	}

	if reason := e.pathProblem(req); reason != "" {
		return e.finalise(req, Result{
			Decision: tool.DecisionAsk,
			Reason:   reason,
			Warning:  a.Explain(),
		})
	}

	return e.finalise(req, Result{
		Decision: tool.DecisionAsk,
		Reason:   "shell command " + a.Class.String(),
		Warning:  a.Explain(),
	})
}

// finalise applies the "cannot ask" rule.
//
// DontAsk exists for headless runs. Converting an unanswered prompt into a
// deny is the only honest outcome; leaving it as "ask" would hang a process
// with nobody to answer.
func (e *Engine) finalise(_ Request, r Result) Result {
	if r.Decision == tool.DecisionAsk && !e.Mode.Interactive() {
		r.Decision = tool.DecisionDeny
		r.Reason += " (denied: permission mode " + string(e.Mode) + " cannot prompt)"
	}
	return r
}

// pathProblem reports why a path may not be written without asking.
//
// Two checks, both of which the TypeScript side performs: the path must be
// inside a directory the session was granted, and it must not be one of the
// files whose modification is a privilege escalation rather than an edit.
func (e *Engine) pathProblem(req Request) string {
	for _, p := range req.Paths {
		if reason := e.protectedPath(p); reason != "" {
			return reason
		}
		if !e.withinWorkDirs(p) {
			return fmt.Sprintf("%s is outside the working directories", p)
		}
	}
	return ""
}

func (e *Engine) withinWorkDirs(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	roots := append([]string{e.WorkDir}, e.ExtraDirs...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		rootAbs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if within(rootAbs, abs) {
			return true
		}
	}
	return false
}

// within reports whether path is root or sits below it.
//
// A prefix comparison on the raw strings would treat /work-other as inside
// /work, which is exactly the kind of hole a permission check must not have.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// protectedPathFragments are path segments whose contents are credentials or
// keying material. A write anywhere below them is treated as a privilege
// escalation, not an edit.
var protectedPathFragments = []string{".ssh/", ".aws/", ".gnupg/", ".kube/"}

// protectedFileNames are files that run code or authenticate on next use.
// They are matched on the base name so the check does not depend on where the
// file lives: a repository can ship a .bashrc, and a `.bashrc` in a cloned
// repo is exactly how "just run the tests" becomes a backdoor.
var protectedFileNames = []string{
	".bashrc", ".bash_profile", ".bash_login", ".bash_logout",
	".profile", ".zshrc", ".zprofile", ".zshenv", ".zlogin",
	".gitconfig", ".netrc", ".npmrc", ".pypirc", ".vimrc", ".inputrc",
	"authorized_keys", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
}

var protectedSuffixes = []string{
	filepath.Join("etc", "passwd"),
	filepath.Join("etc", "shadow"),
	filepath.Join("etc", "sudoers"),
	filepath.Join("etc", "hosts"),
}

func (e *Engine) protectedPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	norm := filepath.ToSlash(abs)

	for _, suffix := range protectedSuffixes {
		if strings.HasSuffix(norm, "/"+filepath.ToSlash(suffix)) {
			return abs + " is a system file"
		}
	}

	// ~/.ultrac/settings.json decides permissions. A tool that can rewrite it
	// can grant itself anything, so it is checked before anything else.
	if e.ConfigDir != "" {
		if cfgAbs, err := filepath.Abs(e.ConfigDir); err == nil && within(cfgAbs, abs) {
			return abs + " is inside the agent configuration directory"
		}
	}

	base := strings.ToLower(filepath.Base(abs))
	for _, name := range protectedFileNames {
		if base == name {
			return abs + " is a credential or shell-init file"
		}
	}

	if strings.Contains(norm, "/.git/hooks/") {
		return abs + " is a git hook, which runs on every commit"
	}
	for _, frag := range protectedPathFragments {
		if strings.Contains(norm, "/"+frag) {
			return abs + " holds credentials or keys"
		}
	}
	return ""
}

// CanUse adapts the engine to the callback the tool contract expects.
//
// The engine owns the mapping from a raw tool input to a Request, because only
// it knows the session's working directories; the tools stay free of policy.
func (e *Engine) CanUse(classify func(name string, input json.RawMessage) (Request, bool)) tool.CanUseFunc {
	return func(_ context.Context, name string, input json.RawMessage) (tool.PermissionResult, error) {
		req, ok := classify(name, input)
		if !ok {
			// An unknown tool has no classification, so nothing vouches for
			// it. Deferring to the user is the only safe answer.
			return tool.PermissionResult{
				Decision: tool.DecisionAsk,
				Reason:   "no permission classification for tool " + name,
			}, nil
		}
		res := e.Check(context.Background(), req)
		return tool.PermissionResult{
			Decision: res.Decision,
			Reason:   reasonWithWarning(res),
		}, nil
	}
}

func reasonWithWarning(r Result) string {
	if r.Warning == "" {
		return r.Reason
	}
	return r.Reason + " — " + r.Warning
}
