package triage

import "strings"

// surfacePatterns are production paths that carry a user-facing surface even
// inside an internal/ package: CLI commands and help text, the terminal UI,
// generated skill sources, configuration and schema (including SQL and
// migrations), and public API or wire definitions. A change touching one is
// never internal-only.
var surfacePatterns = []string{
	"cmd/*", "*/cmd/*", "cli/*", "*/cli/*",
	"*[Cc]onfig*", "*[Ss]chema*", "*.sql", "*migration/*", "*migrations/*", "*[Hh]elp*", "*[Uu]sage*",
	"internal/tui/*", "*/internal/tui/*", "internal/skill/*", "*/internal/skill/*",
	"api/*", "*/api/*", "*.proto", "*.graphql", "*[Oo]pen[Aa][Pp][Ii]*", "*[Ss]wagger*",
}

// InternalOnly reports whether a change is internal-only: it touches no
// documentation (IsDocs), no user-facing surface (surfacePatterns), and every
// production file lives under an internal/ directory, which Go makes
// importable only from the tree rooted at that directory's parent. Tests and fixtures never count
// against it, but at least one production file must change. It is
// deliberately one-sided: an empty or tests-only change, a production file
// outside internal/, or any doubtful path is not internal-only, so a
// caller that skips work on this signal keeps the work whenever the signal
// is not clean.
func InternalOnly(files []string) bool {
	production := false
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		switch {
		case IsDocs(f):
			return false
		case IsTest(f):
			continue
		case matchAny(surfacePatterns, f), !underInternalDir(f):
			return false
		}
		production = true
	}
	return production
}

func underInternalDir(path string) bool {
	return strings.HasPrefix(path, "internal/") || strings.Contains(path, "/internal/")
}
