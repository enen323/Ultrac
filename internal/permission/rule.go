package permission

import (
	"sort"
	"strings"
)

// Behavior is what a rule or a decision says to do.
type Behavior string

const (
	// BehaviorAllow runs the call without asking.
	BehaviorAllow Behavior = "allow"
	// BehaviorAsk defers to the user.
	BehaviorAsk Behavior = "ask"
	// BehaviorDeny refuses outright.
	BehaviorDeny Behavior = "deny"
)

// RuleSource records where a rule came from.
//
// It is carried on every rule rather than collapsed into "the merged list"
// because the answer to "why was I asked for this?" is the source, and that
// question is the whole reason someone opens /permissions.
type RuleSource string

const (
	SourceUserSettings    RuleSource = "userSettings"
	SourceProjectSettings RuleSource = "projectSettings"
	SourceLocalSettings   RuleSource = "localSettings"
	SourceFlagSettings    RuleSource = "flagSettings"
	SourcePolicySettings  RuleSource = "policySettings"
	SourceSession         RuleSource = "session"
	SourceCLIArg          RuleSource = "cliArg"
	SourceCommand         RuleSource = "command"
)

// Rule is one permission rule.
//
// The syntax is the TypeScript side's, unchanged, because these strings live
// in settings.json files that both implementations read:
//
//	Bash                     any Bash call
//	Bash(git status)         exactly that command
//	Bash(git:*)              any command starting with "git"
//	Edit(//tmp/**)           any path under //tmp
type Rule struct {
	Tool     string
	Content  string
	Behavior Behavior
	Source   RuleSource
}

// ParseRule turns a rule string such as `Bash(git status:*)` into a Rule.
//
// The split is on the first unescaped `(` and the last unescaped `)`, matching
// permissionRuleValueFromString: a malformed string degrades to "the whole
// thing is a tool name" rather than being dropped, so a typo'd rule still
// shows up in a listing instead of disappearing.
func ParseRule(ruleString string, behavior Behavior, source RuleSource) Rule {
	s := strings.TrimSpace(ruleString)
	open := indexUnescaped(s, '(', false)
	if open < 0 {
		return Rule{Tool: normalizeToolName(s), Behavior: behavior, Source: source}
	}
	close := indexUnescaped(s, ')', true)
	if close < 0 || close <= open || close != len(s)-1 {
		return Rule{Tool: normalizeToolName(s), Behavior: behavior, Source: source}
	}
	return Rule{
		Tool:     normalizeToolName(s[:open]),
		Content:  unescapeRuleContent(s[open+1 : close]),
		Behavior: behavior,
		Source:   source,
	}
}

// String renders the rule back to its settings.json form.
func (r Rule) String() string {
	if r.Content == "" {
		return r.Tool
	}
	return r.Tool + "(" + escapeRuleContent(r.Content) + ")"
}

// Matches reports whether the rule applies to a call.
//
// Content comparison is skipped when the rule carries no content (a
// tool-level rule) or when the call has no matchable content.
func (r Rule) Matches(tool string, content string) bool {
	if !strings.EqualFold(r.Tool, tool) {
		return false
	}
	if r.Content == "" {
		return true
	}
	if content == "" {
		return false
	}
	return matchRuleContent(r.Content, content)
}

// Set is an ordered collection of rules.
//
// Order is preserved because the first matching rule within a behavior class
// is the one reported, and "which rule fired" is surfaced to the user.
type Set struct {
	rules []Rule
}

// NewSet builds a Set from rules.
func NewSet(rules ...Rule) *Set { return &Set{rules: rules} }

// Add appends rules.
func (s *Set) Add(rules ...Rule) {
	if s == nil {
		return
	}
	s.rules = append(s.rules, rules...)
}

// Rules returns a copy of the rules, for diagnostics.
func (s *Set) Rules() []Rule {
	if s == nil {
		return nil
	}
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}

// Len is the number of rules.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.rules)
}

// MatchFor finds the first rule that applies with the given behavior.
//
// Deny is consulted before allow by every caller, because an allow that
// outranked a deny would make deny rules unenforceable — the one thing a deny
// rule exists to guarantee.
func (s *Set) MatchFor(tool, content string, behavior Behavior) (Rule, bool) {
	if s == nil {
		return Rule{}, false
	}
	for _, r := range s.rules {
		if r.Behavior != behavior {
			continue
		}
		if r.Matches(tool, content) {
			return r, true
		}
	}
	return Rule{}, false
}

// Sorted returns the rules ordered for display: by tool, then content.
func (s *Set) Sorted() []Rule {
	out := s.Rules()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Tool != out[j].Tool {
			return out[i].Tool < out[j].Tool
		}
		return out[i].Content < out[j].Content
	})
	return out
}

// normalizeToolName maps legacy tool names onto current ones.
//
// The TypeScript side did this when tools were renamed; old settings.json
// files still carry the old spellings, so a rule written before the rename has
// to keep working.
func normalizeToolName(name string) string {
	switch strings.TrimSpace(name) {
	case "Task":
		return "Agent"
	case "ReadFile":
		return "Read"
	case "WriteFile":
		return "Write"
	case "EditFile":
		return "Edit"
	default:
		return strings.TrimSpace(name)
	}
}

// matchRuleContent compares a rule's content against a call's content.
//
// Three shapes, in order:
//
//   - `prefix:*` is a prefix match. This is the shorthand for "any command
//     starting with git", and treating the `:*` as a literal would make the
//     rule match nothing at all.
//   - A pattern with glob metacharacters is matched as a path glob, where `**`
//     crosses separators and `*` does not.
//   - Anything else is an exact match.
//
// Path comparison normalises separators first. A settings file is a thing
// people copy between machines, so `Read(src/a.go)` written on Linux has to
// keep working after the same repository is opened on Windows — where the
// engine's own paths arrive with backslashes. The prefix form is left alone:
// it matches commands, and a backslash there is part of the command.
func matchRuleContent(pattern, value string) bool {
	if prefix, ok := strings.CutSuffix(pattern, ":*"); ok {
		return matchPrefix(prefix, value)
	}
	pattern = toSlashPath(pattern)
	value = toSlashPath(value)
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern == value
	}
	return globMatch(pattern, value)
}

// toSlashPath normalises Windows separators to forward slashes.
func toSlashPath(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	return strings.ReplaceAll(s, `\`, "/")
}

// matchPrefix implements the `:*` form.
//
// The prefix must end at a word boundary. `Bash(git:*)` is meant to approve
// `git status`; a bare strings.HasPrefix would also approve `gitx`, which is a
// different program entirely. That is a small hole, but it is the kind that
// only has to be found once.
func matchPrefix(prefix, value string) bool {
	if value == prefix {
		return true
	}
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	switch value[len(prefix)] {
	case ' ', '\t', '\n', '/', '\\':
		return true
	default:
		return false
	}
}

// globMatch is a path glob with `**` semantics.
//
// Written by hand rather than pulled from a library because the exact
// semantics here are load-bearing: `**` must cross directory separators and
// `*` must not, and a general-purpose matcher that got that wrong would let
// `Edit(/src/*)` silently permit `/src/a/b.go`.
func globMatch(pattern, name string) bool {
	return globSegment(pattern, name)
}

func globSegment(pattern, name string) bool {
	// Iterative backtracking on the single-star case, recursive on `**`.
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			doubleStar := len(pattern) > 1 && pattern[1] == '*'
			if doubleStar {
				// Collapse `***` to `**` and absorb an immediately following
				// separator, so `**/x` also matches `x`.
				rest := pattern[2:]
				for len(rest) > 0 && rest[0] == '*' {
					rest = rest[1:]
				}
				if len(rest) > 0 && (rest[0] == '/' || rest[0] == '\\') {
					rest = rest[1:]
					// `**/x` matches `x` at the root too.
					if globSegment(rest, name) {
						return true
					}
				}
				for i := 0; i <= len(name); i++ {
					if globSegment(rest, name[i:]) {
						return true
					}
				}
				return false
			}
			// Single star: match any run of non-separator characters.
			rest := pattern[1:]
			for i := 0; i <= len(name); i++ {
				if i > 0 && isPathSep(name[i-1]) {
					break
				}
				if globSegment(rest, name[i:]) {
					return true
				}
			}
			return false

		case '?':
			if len(name) == 0 || isPathSep(name[0]) {
				return false
			}
			pattern, name = pattern[1:], name[1:]

		default:
			if len(name) == 0 || pattern[0] != name[0] {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		}
	}
	return len(name) == 0
}

func isPathSep(c byte) bool { return c == '/' || c == '\\' }

// indexUnescaped finds the first or last unescaped occurrence of ch.
//
// A character is escaped when preceded by an odd number of backslashes, which
// is what lets a rule content contain a literal parenthesis.
func indexUnescaped(s string, ch byte, last bool) int {
	found := -1
	for i := 0; i < len(s); i++ {
		if s[i] != ch {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			continue
		}
		if !last {
			return i
		}
		found = i
	}
	return found
}

func escapeRuleContent(s string) string {
	return strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(s)
}

// unescapeRuleContent undoes escapeRuleContent, and only that.
//
// An earlier version consumed a backslash whenever it was followed by
// anything, which is fine for a rule about a command and catastrophic for one
// about a Windows path: `Read(src\a.go)` became `Read(srca.go)` and matched
// nothing. Only the three characters the escaper writes are unescaped.
func unescapeRuleContent(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\', '(', ')':
				i++
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
