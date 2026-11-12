package triage

import (
	"context"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Lines that are not hand-written source never count toward a change's size:
// the size only decides which small fixes pool together, and a coherent
// change ships whole whatever it weighs. Docs, tests and fixtures are
// excluded by path (IsDocs, IsTest); this file owns the rest:
//
//   - generated code, found by evidence: a lockfile name, a glob in the
//     repository's own .nm-triage-generated (one per line, like
//     .nm-triage-risk), a linguist-generated entry in the root .gitattributes
//     (as of the head commit), or a generator marker in the file's first lines
//     ("Code generated ... DO NOT EDIT." or "@generated");
//   - spec or registry JSON: *.json under a spec/, specs/, registry/,
//     schema/ or schemas/ directory. Other JSON is hand-written and counts.
//
// An excluded file on the risk list still lifts the review to strong; only
// its lines stay out of the count.

var lockfileNames = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"bun.lock": true, "bun.lockb": true, "go.sum": true, "Cargo.lock": true, "poetry.lock": true,
	"Pipfile.lock": true, "uv.lock": true, "Gemfile.lock": true, "composer.lock": true,
}

var specJSONDirs = map[string]bool{"spec": true, "specs": true, "registry": true, "schema": true, "schemas": true}

// GeneratedOverrideFile is the repository file naming generator output, one
// glob per line.
const GeneratedOverrideFile = ".nm-triage-generated"

// headerScanLines is how far into a file a generator marker may sit.
const headerScanLines = 20

var generatedMarker = regexp.MustCompile(`Code generated .*DO NOT EDIT|@generated\b`)

// IsLockfile reports whether path is a dependency lockfile.
func IsLockfile(p string) bool { return lockfileNames[path.Base(p)] }

// IsSpecJSON reports whether path is JSON under a spec or registry directory.
func IsSpecJSON(p string) bool {
	if !strings.HasSuffix(p, ".json") {
		return false
	}
	parts := strings.Split(p, "/")
	for _, dir := range parts[:len(parts)-1] {
		if specJSONDirs[dir] {
			return true
		}
	}
	return false
}

// attrRule is one .gitattributes line that sets or unsets linguist-generated.
type attrRule struct {
	pattern string
	set     bool
}

func parseGeneratedAttrs(content string) []attrRule {
	var rules []attrRule
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, a := range fields[1:] {
			switch a {
			case "linguist-generated", "linguist-generated=true":
				rules = append(rules, attrRule{fields[0], true})
			case "-linguist-generated", "!linguist-generated", "linguist-generated=false":
				rules = append(rules, attrRule{fields[0], false})
			}
		}
	}
	return rules
}

func (r attrRule) matches(p string) bool {
	pat := r.pattern
	if strings.Contains(strings.TrimSuffix(pat, "/"), "/") {
		return matchGlob(strings.TrimPrefix(pat, "/"), p)
	}
	return matchGlob(strings.TrimPrefix(pat, "/"), path.Base(p))
}

// generatedDetector decides which changed files are generated.
type generatedDetector struct {
	ctx        context.Context
	dir        string
	base, head string
	override   []string
	attrs      []attrRule
}

func newGeneratedDetector(ctx context.Context, dir, base, head string) (*generatedDetector, error) {
	g := &generatedDetector{ctx: ctx, dir: dir, base: base, head: head}
	var err error
	if g.override, err = ReadPatternFile(filepath.Join(dir, GeneratedOverrideFile)); err != nil {
		return nil, err
	}
	// A missing .gitattributes is the common case and means no entries.
	if out, code, err := gitOutput(ctx, dir, nil, "show", head+":.gitattributes"); err == nil && code == 0 {
		g.attrs = parseGeneratedAttrs(out)
	}
	return g, nil
}

// Excluded reports whether a production file's lines stay out of the size.
func (g *generatedDetector) Excluded(p string) bool {
	if IsLockfile(p) || IsSpecJSON(p) || matchAny(g.override, p) {
		return true
	}
	decided, generated := false, false
	for _, r := range g.attrs {
		if r.matches(p) {
			decided, generated = true, r.set
		}
	}
	if decided {
		return generated
	}
	return g.hasMarker(p)
}

// hasMarker reads the file's first lines from head (from base when the change
// deleted it) looking for a generator marker.
func (g *generatedDetector) hasMarker(p string) bool {
	for _, rev := range []string{g.head, g.base} {
		out, code, err := gitOutput(g.ctx, g.dir, nil, "show", rev+":"+p)
		if err != nil || code != 0 {
			continue
		}
		return generatedMarker.MatchString(firstLines(out, headerScanLines))
	}
	return false
}

func firstLines(s string, n int) string {
	idx := 0
	for i := 0; i < n; i++ {
		j := strings.IndexByte(s[idx:], '\n')
		if j < 0 {
			return s
		}
		idx += j + 1
	}
	return s[:idx]
}
