package triage

import "strings"

// surfacePatterns are production paths that carry a user-facing surface even
// inside an internal/ package: CLI commands and help text, configuration and
// schema, and public API or wire definitions. A change touching one is never
// internal-only.
var surfacePatterns = []string{
	"cmd/*", "*/cmd/*", "cli/*", "*/cli/*",
	"*[Cc]onfig*", "*[Ss]chema*", "*[Hh]elp*", "*[Uu]sage*",
	"api/*", "*/api/*", "*.proto", "*.graphql", "*[Oo]pen[Aa][Pp][Ii]*", "*[Ss]wagger*",
}

// InternalOnly reports whether a change is internal-only: it touches no
// documentation (IsDocs), no user-facing surface (surfacePatterns), and every
// production file lives under an internal/ directory, which Go makes
// unimportable from outside its module. Tests and fixtures never count
// against it. It is deliberately one-sided: an empty change, a production
// file outside internal/, or any doubtful path is not internal-only, so a
// caller that skips work on this signal keeps the work whenever the signal
// is not clean.
func InternalOnly(files []string) bool {
	seen := false
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		seen = true
		switch {
		case IsDocs(f):
			return false
		case IsTest(f):
			continue
		case matchAny(surfacePatterns, f), !underInternalDir(f):
			return false
		}
	}
	return seen
}

func underInternalDir(path string) bool {
	return strings.HasPrefix(path, "internal/") || strings.Contains(path, "/internal/")
}
