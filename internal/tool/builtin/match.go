package builtin

import (
	"regexp"
	"strings"
	"sync"
)

// globToRegexp turns a shell glob into an anchored regexp over slash-separated
// relative paths.
//
// The semantics that matter, and that a naive `*` → `.*` translation gets
// wrong:
//
//   - `**` crosses directory separators, `*` does not.
//   - `**/` may match nothing at all, so `**/*.go` matches `main.go`. Without
//     that, the most common pattern in use would silently miss the root.
//   - `?` matches one character, not a separator.
//   - `{a,b}` alternates.
//
// The pattern is matched against a path with forward slashes on every platform;
// Windows separators are normalised before matching, not after, because a
// pattern written on one platform has to work on the other.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch c {
		case '*':
			doubled := i+1 < len(runes) && runes[i+1] == '*'
			if doubled {
				i++
				// Absorb any further stars: `***` means the same as `**`.
				for i+1 < len(runes) && runes[i+1] == '*' {
					i++
				}
				if i+1 < len(runes) && (runes[i+1] == '/' || runes[i+1] == '\\') {
					i++
					// `**/` may match nothing.
					b.WriteString(`(?:.*/)?`)
					continue
				}
				b.WriteString(`.*`)
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			// Pass a character class through, translating a leading `!` to `^`.
			j := i + 1
			if j < len(runes) && (runes[j] == '!' || runes[j] == '^') {
				j++
			}
			if j < len(runes) && runes[j] == ']' {
				j++
			}
			for j < len(runes) && runes[j] != ']' {
				j++
			}
			if j >= len(runes) {
				// Unterminated class: treat the bracket literally rather than
				// failing the whole pattern.
				b.WriteString(regexp.QuoteMeta(string(c)))
				continue
			}
			cls := string(runes[i+1 : j])
			if strings.HasPrefix(cls, "!") {
				cls = "^" + cls[1:]
			}
			b.WriteString("[" + cls + "]")
			i = j
		case '{':
			// Find the matching close brace, allowing no nesting: nested braces
			// are rare enough that supporting them would add more failure modes
			// than it removes.
			end := -1
			for j := i + 1; j < len(runes); j++ {
				if runes[j] == '}' {
					end = j
					break
				}
			}
			if end < 0 {
				b.WriteString(regexp.QuoteMeta(string(c)))
				continue
			}
			alts := strings.Split(string(runes[i+1:end]), ",")
			b.WriteString(`(?:`)
			for k, alt := range alts {
				if k > 0 {
					b.WriteString(`|`)
				}
				b.WriteString(translateGlobFragment(alt))
			}
			b.WriteString(`)`)
			i = end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

// translateGlobFragment converts a brace alternative, which may itself contain
// stars, into regexp source.
func translateGlobFragment(frag string) string {
	var b strings.Builder
	for i := 0; i < len(frag); i++ {
		switch frag[i] {
		case '*':
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(frag[i])))
		}
	}
	return b.String()
}

var globCache sync.Map // pattern -> *regexp.Regexp

// compileGlob caches compiled patterns.
//
// A Glob or Grep call compiles one pattern and matches it against tens of
// thousands of paths; compiling per path would dominate the runtime of the
// tool it is meant to serve.
func compileGlob(pattern string) (*regexp.Regexp, error) {
	if v, ok := globCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return nil, err
	}
	globCache.Store(pattern, re)
	return re, nil
}

// globRoot returns the longest literal directory prefix of a pattern.
//
// Walking from this directory instead of the search root is what keeps
// `src/internal/**/*.go` from listing the whole repository to reject almost
// all of it.
func globRoot(pattern string) string {
	var parts []string
	for _, seg := range strings.Split(toSlash(pattern), "/") {
		if strings.ContainsAny(seg, "*?[{") {
			break
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "/")
}

func toSlash(p string) string {
	return strings.ReplaceAll(p, `\`, "/")
}
