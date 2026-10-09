// Package shell classifies shell commands by their structure.
//
// # Why it is its own package
//
// Two callers need the same answer and neither may import the other. The
// permission engine asks "may this run", and BashTool asks "should this be
// marked read-only" so the scheduler knows whether it can run in parallel.
// Putting the analysis in either one would make the other import it, which is
// the horizontal L2 dependency the layering rules forbid. As an infrastructure
// package below both, it is importable from anywhere.
//
// # Why an AST and not patterns
//
// Every question answered here — is this destructive, does it reach the
// network, can a variable change what it does — is a question about structure.
// Substring matching gets `echo 'rm -rf /'` wrong and `rm  -rf  /` wrong in
// the other direction, and the direction where it is wrong about safety is the
// one that costs data.
package shell

import (
	"sort"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Class is what a shell command actually does, as far as can be
// established without running it.
//
// The order is a severity ranking: a pipeline is classified by the worst
// program in it, so the enum's ordering is used, not just its values.
type Class int

const (
	// ClassUnknown means the command could not be classified. It is the zero
	// value on purpose: an unparsed or exotic command must never be treated
	// as safe by accident.
	ClassUnknown Class = iota
	// ClassReadOnly only reads.
	ClassReadOnly
	// ClassWrite changes state — files, the repository, a container.
	ClassWrite
	// ClassNetwork reaches outside the machine.
	ClassNetwork
	// ClassInterpreter runs arbitrary code by design.
	ClassInterpreter
	// ClassDestructive irreversibly destroys data.
	ClassDestructive
)

func (c Class) String() string {
	switch c {
	case ClassReadOnly:
		return "read-only"
	case ClassWrite:
		return "write"
	case ClassNetwork:
		return "network"
	case ClassInterpreter:
		return "interpreter"
	case ClassDestructive:
		return "destructive"
	default:
		return "unknown"
	}
}

// Analysis is the result of parsing one command.
type Analysis struct {
	Class Class
	// Programs lists every command name invoked, in source order. It is what
	// makes a rejection explainable: "it runs rm" is actionable, "denied" is
	// not.
	Programs []string
	// Reasons accumulates why the class is what it is.
	Reasons []string

	// Dynamic means the command contains a variable, command substitution,
	// arithmetic expansion, subshell or function definition. Anything dynamic
	// can evaluate to something other than what the text suggests, so it is
	// never auto-approved.
	Dynamic bool
	// WritesFile means a redirection writes to a file. `echo hi > /etc/passwd`
	// is a write however read-only `echo` is.
	WritesFile bool
	// Background means the statement was put in the background.
	Background bool
	// Compound means the command line performs more than one action: a
	// sequence, a pipeline, a subshell, a loop.
	//
	// It gates allow rules, and that is its whole purpose. `Bash(git:*)` is a
	// statement about a command's *prefix*, so letting it approve
	// `git status; rm -rf /` would turn every prefix rule into an unlimited
	// one. Compound commands are therefore classified instead of matched.
	Compound bool

	// ControlFlow means the command branches or repeats: a loop, a
	// conditional, a subshell, a function.
	//
	// A sequence is bounded and every part of it was enumerated, so a
	// read-only sequence is a read. A loop is neither: `while true; do ls;
	// done` is read-only by classification and would hang the agent forever.
	// Control flow therefore disqualifies auto-approval even when everything
	// inside it only reads.
	ControlFlow bool
	// ParseError is set when the command is not valid shell. It forces
	// ClassUnknown.
	ParseError error

	// calls counts the simple commands on the line, which is how Compound is
	// decided for sequence and pipe forms.
	calls int
}

// ReadOnly reports whether the command may run without asking.
//
// Every escape hatch is closed here rather than at the call site: a dynamic
// word, a redirection, a background job or a parse failure all disqualify it.
// Making the caller remember that is how one of them gets forgotten.
func (a Analysis) ReadOnly() bool {
	return a.Class == ClassReadOnly && !a.Dynamic && !a.WritesFile &&
		!a.Background && !a.ControlFlow
}

// Destructive reports whether the command irreversibly destroys data.
func (a Analysis) Destructive() bool { return a.Class == ClassDestructive }

// Unclassified reports whether the command could not be classified at all.
//
// It exists so callers can say "treat this as unknown" without comparing
// against the zero value, which reads ambiguously at a call site.
func (a Analysis) Unclassified() bool {
	return a.Class == ClassUnknown || a.ParseError != nil
}

// Analyze parses a command into an AST and classifies it.
//
// The parser is the real one from mvdan.cc/sh, not a regex or a split on
// spaces, because the questions being asked here are structural: is this
// `rm -rf /` or an argument that happens to contain the text. String matching
// answers those questions wrong in both directions, and the direction where it
// answers "safe" is the one that costs data.
func Analyze(command string) Analysis {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return Analysis{Class: ClassUnknown, Reasons: []string{"empty command"}}
	}

	file, err := syntax.NewParser().Parse(strings.NewReader(trimmed), "")
	if err != nil {
		return Analysis{
			Class:      ClassUnknown,
			ParseError: err,
			Reasons:    []string{"command is not valid shell syntax: " + err.Error()},
		}
	}

	var a Analysis
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			if n.Background {
				a.Background = true
			}
			for _, r := range n.Redirs {
				if redirectsToFile(r) {
					a.WritesFile = true
				}
				if wordIsDynamic(r.Word) || wordIsDynamic(r.Hdoc) {
					a.Dynamic = true
				}
			}

		case *syntax.CallExpr:
			a.calls++
			name, ok := literalWord(firstWord(n.Args))
			if !ok {
				// `$CMD args` — the program itself is not known statically.
				a.Dynamic = true
				a.Programs = append(a.Programs, "(dynamic)")
				return true
			}
			a.Programs = append(a.Programs, name)
			if anyDynamic(n.Args) || anyDynamicAssigns(n.Assigns) {
				a.Dynamic = true
			}
			class, why := classifyProgram(name, literalArgs(n.Args))
			a.raise(class, why)

		case *syntax.BinaryCmd:
			// `a && b`, `a || b`, `a | b`: every sub-command is walked and
			// classified above, so the class already reflects the worst of
			// them. Compound is set because an allow *rule* must not be
			// matched against a prefix (see Analysis.Compound), but the
			// command is not dynamic — nothing about it is unknowable.
			a.Compound = true

		case *syntax.Subshell, *syntax.Block, *syntax.IfClause, *syntax.WhileClause,
			*syntax.ForClause, *syntax.CaseClause:
			// Branches and repeats. Everything inside is walked as well, so
			// this is compound rather than dynamic — but it is control flow,
			// which no allow rule and no auto-approval may cover.
			a.Compound = true
			a.ControlFlow = true

		case *syntax.FuncDecl, *syntax.ArithmCmd, *syntax.DeclClause,
			*syntax.LetClause, *syntax.TestClause, *syntax.TestDecl:
			// A function's body is walked, but the body is only knowable at
			// the point it is called, which may be after the definition in a
			// way this walk does not model. Declarations and arithmetic are
			// not readable by inspection at all.
			a.Compound = true
			a.ControlFlow = true
			a.Dynamic = true

		case *syntax.CmdSubst, *syntax.ParamExp, *syntax.ArithmExp, *syntax.ProcSubst,
			*syntax.ExtGlob:
			a.Dynamic = true
		}
		return true
	})

	if a.calls > 1 {
		// `a && b`, `a | b`, `a; b` — one line, several actions.
		a.Compound = true
	}

	if len(a.Programs) == 0 {
		a.Reasons = append(a.Reasons, "no command found")
		return a
	}
	return a
}

// raise records a class and its reason, keeping the most severe one.
func (a *Analysis) raise(class Class, why string) {
	if why != "" {
		a.Reasons = append(a.Reasons, why)
	}
	if class > a.Class {
		a.Class = class
	}
}

// redirectsToFile reports whether a redirection creates or truncates a file.
//
// Duplicating an existing descriptor (`2>&1`) is not a file write; `>` and
// `>>` are. A redirection to the null device is not either, and treating it as
// one matters more than it looks: `2>/dev/null` is in a large fraction of the
// shell commands anybody types, and classifying those as writes would prompt
// for things that change nothing.
func redirectsToFile(r *syntax.Redirect) bool {
	switch r.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob,
		syntax.RdrAll, syntax.AppAll, syntax.RdrInOut:
		if target, ok := literalWord(r.Word); ok && isNullDevice(target) {
			return false
		}
		return true
	case syntax.DplOut:
		// `>&file` is a write; `>&2` is not. A non-numeric target is a file.
		target, ok := literalWord(r.Word)
		if !ok {
			return true
		}
		return !isNumeric(target) && !isNullDevice(target)
	default:
		return false
	}
}

// isNullDevice reports whether a redirection target discards output.
//
// Both spellings are recognised regardless of platform: a command written on
// Windows with /dev/null (Git Bash) or on Unix with NUL is still a command
// whose output goes nowhere.
func isNullDevice(target string) bool {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "/dev/null", "nul", "nul:", "$null":
		return true
	default:
		return false
	}
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func firstWord(args []*syntax.Word) *syntax.Word {
	if len(args) == 0 {
		return nil
	}
	return args[0]
}

// literalWord returns a word's text when it is a literal.
//
// Quoting is modelled because the parser distinguishes it: a single-quoted
// word is an *SglQuoted, not an *Lit, and treating anything that is not an
// *Lit as computed would classify `echo 'a b'` as dynamic — which would then
// disqualify it from auto-approval and prompt for every quoted argument
// anybody ever types.
//
// The distinction that matters is expansion, not quoting. `'a b'` and `"a b"`
// are literal text; `"$x"` and `"$(cmd)"` are not, which is why a
// double-quoted word has to be inspected part by part.
func literalWord(w *syntax.Word) (string, bool) {
	if w == nil {
		return "", false
	}
	var b strings.Builder
	for _, part := range w.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			// Single quotes suppress every expansion, including escapes. The
			// text is what it is.
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				lit, ok := inner.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	s := b.String()
	if s == "" {
		return "", false
	}
	return s, true
}

func literalArgs(args []*syntax.Word) []string {
	out := make([]string, 0, len(args))
	for _, w := range args {
		if s, ok := literalWord(w); ok {
			out = append(out, s)
		}
	}
	return out
}

func anyDynamic(args []*syntax.Word) bool {
	for _, w := range args {
		if wordIsDynamic(w) {
			return true
		}
	}
	return false
}

// anyDynamicAssigns checks the `FOO=$(rm -rf /)` prefix form. An assignment
// whose right-hand side is computed is dynamic even though no program is
// invoked by it.
func anyDynamicAssigns(assigns []*syntax.Assign) bool {
	for _, a := range assigns {
		if wordIsDynamic(a.Value) {
			return true
		}
	}
	return false
}

func wordIsDynamic(w *syntax.Word) bool {
	if w == nil {
		return false
	}
	for _, part := range w.Parts {
		if partIsDynamic(part) {
			return true
		}
	}
	return false
}

// partIsDynamic reports whether a word part can evaluate to something other
// than its literal text.
func partIsDynamic(part syntax.WordPart) bool {
	switch p := part.(type) {
	case *syntax.Lit:
		return false
	case *syntax.SglQuoted:
		return false
	case *syntax.DblQuoted:
		// Inside double quotes a * is literal but a $ is not, so the inner
		// parts have to be checked rather than the whole word assumed safe.
		for _, inner := range p.Parts {
			if partIsDynamic(inner) {
				return true
			}
		}
		return false
	default:
		// ParamExp, CmdSubst, ArithmExp, ProcSubst, ExtGlob and anything the
		// syntax package adds later.
		return true
	}
}

func hasAnyFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || (strings.HasPrefix(a, f) && !strings.HasPrefix(a, "--")) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Program classification
// ---------------------------------------------------------------------------

// readOnlyPrograms are commands that only read, with no argument-dependent
// behaviour worth checking. The list is deliberately short: a program missing
// from it costs one prompt, while a program wrongly present in it costs files.
var readOnlyPrograms = map[string]struct{}{
	"basename": {}, "cat": {}, "cmp": {}, "comm": {}, "cut": {}, "date": {},
	"df": {}, "diff": {}, "dirname": {}, "du": {}, "echo": {}, "file": {},
	"free": {}, "groups": {}, "head": {}, "hostname": {}, "id": {},
	"join": {}, "ls": {}, "md5sum": {}, "nproc": {}, "paste": {}, "printenv": {},
	"printf": {}, "ps": {}, "pwd": {}, "readlink": {}, "realpath": {},
	"sha1sum": {}, "sha256sum": {}, "sort": {}, "stat": {}, "tail": {},
	"tree": {}, "tr": {}, "uname": {}, "uniq": {}, "uptime": {}, "wc": {},
	"which": {}, "whoami": {},
	// Nullary and predicate builtins. They run no program and touch no file,
	// so they belong here even though they are not "readers" in any
	// interesting sense.
	"true": {}, "false": {}, ":": {}, "test": {}, "[": {}, "type": {},
	// Search *tools* are read-only but take patterns and paths; they are
	// classified here rather than separately because none of their flags make
	// them write. find and rg are excluded on purpose — see below.
	"grep": {},
}

// interpreterPrograms run arbitrary code by design.
var interpreterPrograms = map[string]struct{}{
	"awk": {}, "bash": {}, "bun": {}, "bunx": {}, "deno": {}, "env": {},
	"eval": {}, "exec": {}, "fish": {}, "gawk": {}, "lua": {}, "nohup": {},
	"node": {}, "npm": {}, "npx": {}, "perl": {}, "php": {}, "pnpm": {},
	"python": {}, "python2": {}, "python3": {}, "ruby": {}, "sed": {},
	"sh": {}, "sudo": {}, "time": {}, "tsx": {}, "xargs": {}, "yarn": {},
	"zsh": {},
}

// networkPrograms reach the network.
var networkPrograms = map[string]struct{}{
	"curl": {}, "ftp": {}, "gh": {}, "git-lfs": {}, "nc": {}, "ncat": {},
	"rsync": {}, "scp": {}, "sftp": {}, "socat": {}, "ssh": {}, "telnet": {},
	"wget": {},
}

// destructivePrograms irreversibly destroy data.
var destructivePrograms = map[string]struct{}{
	"chown": {}, "dd": {}, "fdisk": {}, "format": {}, "mkfs": {},
	"mkswap": {}, "parted": {}, "shred": {}, "shutdown": {}, "swapoff": {},
	"truncate": {}, "wipefs": {},
}

// classifyProgram maps one command name (plus its arguments) to a class.
func classifyProgram(name string, args []string) (Class, string) {
	base := programBase(name)

	if _, ok := destructivePrograms[base]; ok {
		return ClassDestructive, base + " can destroy data"
	}
	if strings.HasPrefix(base, "mkfs.") {
		return ClassDestructive, base + " reformats a filesystem"
	}
	if _, ok := networkPrograms[base]; ok {
		return ClassNetwork, base + " reaches the network"
	}
	if _, ok := interpreterPrograms[base]; ok {
		return ClassInterpreter, base + " runs arbitrary code"
	}
	if base == "rm" {
		// `rm -rf` is the canonical irreversible command, and `rm` without
		// flags still deletes. Either way it never auto-approves.
		if hasAnyFlag(args, "-r", "-R", "--recursive") || hasAnyFlag(args, "-f", "--force") {
			return ClassDestructive, "rm with recursive or force flags"
		}
		return ClassWrite, "rm deletes files"
	}
	if base == "mv" || base == "cp" || base == "tee" || base == "install" ||
		base == "ln" || base == "touch" || base == "mkdir" || base == "rmdir" ||
		base == "chmod" || base == "rename" {
		return ClassWrite, base + " changes the filesystem"
	}
	if base == "find" {
		// find is read-only until -delete / -exec / -fprint appear, and those
		// are exactly the flags people forget about.
		for _, a := range args[1:] {
			switch a {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprint0", "-fls":
				return ClassDestructive, "find with " + a
			}
		}
		return ClassReadOnly, ""
	}
	if base == "rg" {
		// --files-with-matches and friends only read; rg has no write mode.
		return ClassReadOnly, ""
	}
	if base == "git" {
		return classifyGit(args)
	}
	if base == "docker" || base == "podman" {
		return classifyContainer(args)
	}
	if base == "go" {
		switch subcommand(args) {
		case "env", "version", "list", "doc", "vet":
			return ClassReadOnly, ""
		default:
			return ClassWrite, "go " + subcommand(args) + " writes build output"
		}
	}
	if _, ok := readOnlyPrograms[base]; ok {
		return ClassReadOnly, ""
	}
	return ClassUnknown, "unrecognised command " + base
}

// classifyGit separates the read subcommands from the ones that move refs.
//
// A blanket "git is safe" rule would approve `git push --force`; a blanket
// "git is unsafe" rule would prompt for `git status`. Neither is acceptable,
// so the subcommand is what decides.
func classifyGit(args []string) (Class, string) {
	sub := subcommand(args)
	rest := argsFrom(args, sub)
	switch sub {
	case "status", "log", "show", "diff", "blame", "describe", "shortlog",
		"whatchanged", "rev-parse", "rev-list", "ls-files", "ls-tree",
		"cat-file", "diff-tree", "name-rev", "symbolic-ref", "for-each-ref",
		"check-ignore", "count-objects", "verify-commit", "grep", "stash":
		if sub == "stash" && len(rest) > 0 && rest[0] != "list" && rest[0] != "show" {
			return ClassWrite, "git stash " + rest[0] + " changes the worktree"
		}
		return ClassReadOnly, ""

	case "reset":
		if hasAnyFlag(rest, "--hard", "--merge", "--keep") {
			return ClassDestructive, "git reset --hard discards work"
		}
		return ClassWrite, "git reset changes the index"

	case "clean":
		return ClassDestructive, "git clean deletes untracked files"

	case "push":
		if hasAnyFlag(rest, "--force", "--force-with-lease", "-f") {
			return ClassDestructive, "git push --force rewrites remote history"
		}
		return ClassNetwork, "git push publishes commits"

	case "filter-branch", "filter-repo":
		return ClassDestructive, "git " + sub + " rewrites history"

	case "branch":
		if hasAnyFlag(rest, "-d", "-D", "--delete", "-m", "-M") {
			return ClassWrite, "git branch modifies refs"
		}
		return ClassReadOnly, ""

	case "tag":
		if hasAnyFlag(rest, "-d", "--delete", "-f", "--force") {
			return ClassWrite, "git tag modifies refs"
		}
		return ClassReadOnly, ""

	case "remote", "config", "submodule", "worktree":
		for _, a := range rest {
			switch a {
			case "add", "remove", "rm", "set", "set-url", "unset", "delete", "prune":
				return ClassWrite, "git " + sub + " " + a + " changes configuration"
			}
		}
		return ClassReadOnly, ""

	case "add", "commit", "checkout", "switch", "restore", "merge", "rebase",
		"cherry-pick", "revert", "reflog", "apply", "am", "init", "clone",
		"fetch", "pull", "gc", "prune", "maintenance", "update-index", "sparse-checkout":
		return ClassWrite, "git " + sub + " changes the repository"

	default:
		return ClassUnknown, "unrecognised git subcommand " + sub
	}
}

func classifyContainer(args []string) (Class, string) {
	sub := subcommand(args)
	switch sub {
	case "ps", "images", "version", "info", "logs", "inspect":
		return ClassReadOnly, ""
	case "rm", "rmi", "prune", "system":
		return ClassDestructive, "container removal is irreversible"
	case "exec":
		return ClassInterpreter, "exec runs a command in a container"
	default:
		return ClassWrite, "container mutation"
	}
}

// subcommand returns the first non-flag argument after the program name.
func subcommand(args []string) string {
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// argsFrom returns the arguments after the given one.
func argsFrom(args []string, after string) []string {
	if after == "" {
		return nil
	}
	for i, a := range args {
		if a == after {
			return args[i+1:]
		}
	}
	return nil
}

// programBase strips a directory prefix: `/usr/bin/rm` is still rm.
func programBase(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// Explain renders a human-readable summary for a permission prompt.
func (a Analysis) Explain() string {
	parts := make([]string, 0, 3)
	parts = append(parts, "classified as "+a.Class.String())
	if len(a.Programs) > 0 {
		progs := append([]string(nil), a.Programs...)
		sort.Strings(progs)
		parts = append(parts, "runs: "+strings.Join(dedupe(progs), ", "))
	}
	if a.WritesFile {
		parts = append(parts, "writes a file via redirection")
	}
	if a.Dynamic {
		parts = append(parts, "contains expansions that cannot be checked statically")
	}
	return strings.Join(parts, "; ")
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// ParseForTest is a thin wrapper so tests can assert parseability without
// reaching into the syntax package.
func ParseForTest(command string) error {
	_, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	return err
}
