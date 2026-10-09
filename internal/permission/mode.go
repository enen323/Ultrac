// Package permission decides whether a tool call may run.
//
// # Layering
//
// Part of the capability layer (L2). It imports the tool contract for the
// Decision vocabulary and nothing from the engine, so the engine owns policy
// assembly and the tools only consume the verdict — the dependency points one
// way, which is what lets internal/tool stay ignorant of policy.
//
// # What is faithful and what is simplified
//
// The four modes, the rule syntax (`Tool` and `Tool(content)`), the
// deny-before-allow precedence, and the path checks inside the working
// directories are ported to match the TypeScript implementation.
//
// The Bash classifier is deliberately NOT a port. The TypeScript side runs
// two model-backed classifiers (bashClassifier, yoloClassifier) on top of its
// static rules; reproducing those needs model calls this layer does not have.
// What replaces them is structural: commands are parsed into a shell AST and
// classified by what they actually do, which is the same discipline the
// backlog asks for ("判定读结构不读措辞") and is strictly more predictable than
// substring matching. Anything the AST parse cannot decide falls through to
// "ask", never to "allow".
package permission

import "strings"

// Mode is the session-wide permission posture.
//
// The values are wire identifiers: they are what --permission-mode accepts and
// what settings.json stores. Renaming one is a compatibility break.
type Mode string

const (
	// ModeDefault asks before anything with a side effect.
	ModeDefault Mode = "default"
	// ModeAcceptEdits auto-approves file edits but still asks for commands.
	ModeAcceptEdits Mode = "acceptEdits"
	// ModePlan is read-only: it exists so the model can investigate and
	// propose without being able to change anything.
	ModePlan Mode = "plan"
	// ModeBypassPermissions approves everything an explicit deny rule does
	// not forbid.
	ModeBypassPermissions Mode = "bypassPermissions"
	// ModeDontAsk approves broadly but suppresses the prompt, so a decision
	// that would have been "ask" becomes "deny" instead of blocking a
	// non-interactive run.
	ModeDontAsk Mode = "dontAsk"
)

// AllModes is the user-addressable set, in the order the CLI lists them.
func AllModes() []Mode {
	return []Mode{ModeDefault, ModeAcceptEdits, ModePlan, ModeBypassPermissions, ModeDontAsk}
}

// ParseMode validates a mode name.
//
// Unknown input is an error rather than a silent fallback to default. A typo
// in --permission-mode that quietly downgraded to "ask for everything" would
// look like the agent ignoring the flag.
func ParseMode(s string) (Mode, error) {
	switch strings.TrimSpace(s) {
	case "", string(ModeDefault):
		return ModeDefault, nil
	case string(ModeAcceptEdits):
		return ModeAcceptEdits, nil
	case string(ModePlan):
		return ModePlan, nil
	case string(ModeBypassPermissions):
		return ModeBypassPermissions, nil
	case string(ModeDontAsk):
		return ModeDontAsk, nil
	default:
		return "", &UnknownModeError{Name: s}
	}
}

// UnknownModeError reports a mode name that is not in the addressable set.
type UnknownModeError struct{ Name string }

func (e *UnknownModeError) Error() string {
	return "invalid permission mode: " + e.Name +
		". Valid options are: default, acceptEdits, plan, bypassPermissions, dontAsk"
}

// Interactive reports whether a mode may prompt the user.
//
// DontAsk exists precisely so a headless run does not hang on a prompt it has
// no way to answer.
func (m Mode) Interactive() bool { return m != ModeDontAsk }

// ReadOnly reports whether the mode forbids mutations outright.
func (m Mode) ReadOnly() bool { return m == ModePlan }
