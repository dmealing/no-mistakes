package triage

import (
	"regexp"
	"strings"
	"sync"
)

// globCache keeps each compiled pattern; the same few dozen patterns are
// matched against every changed file.
var globCache sync.Map

// matchGlob reports whether name matches pattern with shell `case` semantics:
// `*` matches any run of characters INCLUDING `/`, `?` matches one character,
// and `[...]` is a bracket expression (`[!...]` or `[^...]` negates). This is
// deliberately not path.Match, whose `*` stops at `/`: the classification and
// risk lists were written as shell case patterns (`*[Aa]uth*` must match
// `src/auth/token.go`), and repositories' `.nm-triage-risk` files rely on it.
func matchGlob(pattern, name string) bool {
	if cached, ok := globCache.Load(pattern); ok {
		return cached.(*regexp.Regexp).MatchString(name)
	}
	re := regexp.MustCompile(globToRegexp(pattern))
	globCache.Store(pattern, re)
	return re.MatchString(name)
}

func globToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString(`^`)
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			b.WriteString(`.*`)
		case '?':
			b.WriteString(`.`)
		case '[':
			end := bracketEnd(pattern, i)
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			body := pattern[i+1 : end]
			b.WriteString(`[`)
			if strings.HasPrefix(body, "!") || strings.HasPrefix(body, "^") {
				b.WriteString(`^`)
				body = body[1:]
			}
			b.WriteString(strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(body))
			b.WriteString(`]`)
			i = end
		case '\\':
			if i+1 < len(pattern) {
				i++
				b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			} else {
				b.WriteString(`\\`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`$`)
	return b.String()
}

// bracketEnd returns the index of the `]` closing the bracket expression that
// opens at start, or -1 when it is unterminated (then `[` is a literal, as in
// the shell). A `]` right after the opening (or after a negation) is literal.
func bracketEnd(pattern string, start int) int {
	j := start + 1
	if j < len(pattern) && (pattern[j] == '!' || pattern[j] == '^') {
		j++
	}
	if j < len(pattern) && pattern[j] == ']' {
		j++
	}
	for ; j < len(pattern); j++ {
		if pattern[j] == ']' {
			return j
		}
	}
	return -1
}

// matchAny reports whether name matches one of patterns.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchGlob(p, name) {
			return true
		}
	}
	return false
}
