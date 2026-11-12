package triage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// Lines that are not hand-written source never count toward a change's size:
// the size only decides which small fixes pool together, and a coherent
// change ships whole whatever it weighs. Docs, tests and fixtures are
// excluded by path (IsDocs, IsTest); this file owns the rest:
//
//   - generated code, found by evidence: a lockfile name, a glob in the
//     repository's own .nm-triage-generated (one per line, like
//     .nm-triage-risk), a linguist-generated entry in the root .gitattributes
//     (as of the head commit), or a generator marker on a comment line among
//     the file's first lines (Go's own form, a line that is exactly
//     "// Code generated <anything> DO NOT EDIT.", and for any other file a
//     line opening with a comment token followed by "Code generated <anything>
//     DO NOT EDIT" or "@generated"), so prose or a string literal that only
//     mentions a marker still counts;
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

var (
	objectID          = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	goGeneratedMarker = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)
	generatedMarker   = regexp.MustCompile(`^\s*(//|#|/\*|\*|--|<!--)\s*(Code generated .*DO NOT EDIT|@generated\b)`)
)

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

// Excluded returns the production files among paths whose lines stay out of
// the size.
func (g *generatedDetector) Excluded(paths []string) (map[string]bool, error) {
	excluded := map[string]bool{}
	var unknown []string
	for _, p := range paths {
		if IsLockfile(p) || IsSpecJSON(p) || matchAny(g.override, p) {
			excluded[p] = true
			continue
		}
		decided, generated := false, false
		for _, r := range g.attrs {
			if r.matches(p) {
				decided, generated = true, r.set
			}
		}
		if decided {
			excluded[p] = generated
			continue
		}
		unknown = append(unknown, p)
	}
	marked, err := g.markedFiles(unknown)
	if err != nil {
		return nil, err
	}
	for _, p := range marked {
		excluded[p] = true
	}
	return excluded, nil
}

// markedFiles reads every file's first lines from head (from base when the
// change deleted it) through one git cat-file process and returns the files
// that carry a generator marker. A path git cannot name on one line counts.
func (g *generatedDetector) markedFiles(paths []string) ([]string, error) {
	var queries []string
	for _, p := range paths {
		if !strings.ContainsAny(p, "\n\r") {
			queries = append(queries, p)
		}
	}
	if len(queries) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(g.ctx, "git", "cat-file", "--batch")
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	winproc.Harden(cmd)
	var stdin strings.Builder
	for _, p := range queries {
		stdin.WriteString(g.head + ":" + p + "\n" + g.base + ":" + p + "\n")
	}
	cmd.Stdin = strings.NewReader(stdin.String())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %w", err)
	}
	r := bufio.NewReader(stdout)
	var marked []string
	var readErr error
	for _, p := range queries {
		atHead, headErr := readHeader(r)
		atBase, baseErr := readHeader(r)
		if readErr = errors.Join(headErr, baseErr); readErr != nil {
			break
		}
		header := atHead
		if header == "" {
			header = atBase
		}
		if header != "" && hasMarker(p, header) {
			marked = append(marked, p)
		}
	}
	if readErr != nil {
		_, _ = io.Copy(io.Discard, r)
	}
	if err := errors.Join(readErr, cmd.Wait()); err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %w", err)
	}
	return marked, nil
}

// readHeader reads one cat-file --batch answer and returns the first
// headerScanLines lines of the blob, or "" when the object is missing or not
// a blob.
func readHeader(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || !objectID.MatchString(fields[0]) {
		return "", nil
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return "", fmt.Errorf("unexpected cat-file header %q", strings.TrimSpace(line))
	}
	blob := io.LimitReader(r, size)
	lines := bufio.NewReader(blob)
	var header strings.Builder
	for n := 0; n < headerScanLines; n++ {
		l, err := lines.ReadString('\n')
		header.WriteString(l)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if _, err := io.Copy(io.Discard, blob); err != nil {
		return "", err
	}
	if _, err := r.ReadByte(); err != nil {
		return "", err
	}
	if fields[1] != "blob" {
		return "", nil
	}
	return header.String(), nil
}

// hasMarker reports whether a comment line of header is a generator marker.
func hasMarker(p, header string) bool {
	marker := generatedMarker
	if strings.HasSuffix(p, ".go") {
		marker = goGeneratedMarker
	}
	for _, line := range strings.Split(header, "\n") {
		if marker.MatchString(strings.TrimSuffix(line, "\r")) {
			return true
		}
	}
	return false
}
