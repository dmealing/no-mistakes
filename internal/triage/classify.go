// Package triage is the pre-run check every validation run passes through. It
// classifies a branch's change by the paths it touches, suggests the run
// command and the pre-run review, and applies the fleet rules that decide
// whether a NEW run may start at all: a hold on new runs (an install in
// progress), a conflict with a branch already in validation, a third branch of
// the same project in validation, a small change that has a sibling lane to
// ride with, and a bundle of fixes over the bundle limit. It also says which
// first review the change gets (ReviewFor) and gives bundle-shape notes that
// never stop a run.
//
// Everything here is deterministic and path-based on purpose: the decisions
// that matter are properties of the changed paths, the run database, and git,
// so the check costs no model call and its verdict can be audited.
package triage

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Tier is a change's classification. The strictest changed file wins: one
// production file anywhere makes the whole change CODE.
type Tier string

const (
	TierDocs Tier = "DOCS"
	TierTest Tier = "TEST"
	TierCode Tier = "CODE"
	TierRisk Tier = "RISK"
)

func (t Tier) rank() int {
	switch t {
	case TierDocs:
		return 0
	case TierTest:
		return 1
	case TierRisk:
		return 3
	default:
		return 2
	}
}

// A change's "production lines" are the lines of hand-written source: docs,
// tests and fixtures, generated code, lockfiles and spec or registry JSON
// never count (see generated.go). The size only decides which small fixes
// pool together; a coherent change ships whole whatever it weighs.

// SmallChangeLines is the production-line size at or under which a change
// counts as small: it gets no pre-run review, and it should ride with a
// sibling lane rather than take a validation run of its own.
const SmallChangeLines = 300

// StrongBundleLines is the production-line size a strong-review bundle should
// stay at or under: past it a bundle costs more than the work it pools saves.
const StrongBundleLines = 600

// MaxBundleLines is the bundle limit: fix bundles over it cost about 1.7 times
// a single change that size and needed a third run three times as often.
const MaxBundleLines = 1000

// MaxOtherBranchesInValidation is how many OTHER branches of one project may be in
// validation when a new one starts; the next would be the third.
const MaxOtherBranchesInValidation = 1

var docsPatterns = []string{
	"*.md", "*.mdx", "*.rst", "docs/*", ".specify/*", ".claude/*", ".agents/*", ".cursor/*",
	".gitignore", ".gitattributes", "LICENSE*", "CHANGELOG*", "AUTHORS*",
}

var testPatterns = []string{
	"*/src/test/*", "test/*", "tests/*", "*/test/*", "*/tests/*", "*/__tests__/*", "e2e/*", "*/e2e/*",
	"*_test.go", "*_test.py", "test_*.py", "*/test_*.py",
	"*.test.[jt]s", "*.test.[jt]sx", "*.spec.[jt]s", "*.spec.[jt]sx",
	"*Test.java", "*Tests.java", "*IT.java", "*Test.kt", "*Tests.kt", "*_spec.rb",
	// Fixtures are test data: they are never counted, never risk paths.
	"fixtures/*", "*/fixtures/*", "testdata/*", "*/testdata/*",
}

// DefaultRiskPatterns are production paths where a small diff can still do
// large damage. Only production files are checked: a test named AuthTest is
// still a test.
var DefaultRiskPatterns = []string{
	"*migration/*", "*migrations/*", "*.sql",
	"*[Aa]uth*", "*[Ss]ecurity*", "*[Cc]rypto*", "*[Ss]ecret*", "*[Pp]ayment*", "*[Bb]illing*",
	".github/workflows/*", "Dockerfile*", "*docker-compose*", "*compose.yml", "*compose.yaml",
	"pom.xml", "*/pom.xml", "build.gradle*", "package.json", "*/package.json", "Cargo.toml", "go.mod", "pyproject.toml",
	"scripts/nm-*", ".no-mistakes.yaml", "*.tf", "deploy/*", "*/deploy/*",
}

// IsDocs reports whether path is prose or agent configuration.
func IsDocs(path string) bool { return matchAny(docsPatterns, path) }

// IsTest reports whether path is test source or a test fixture.
func IsTest(path string) bool { return matchAny(testPatterns, path) }

// IsProduction reports whether path is neither docs nor test.
func IsProduction(path string) bool { return !IsDocs(path) && !IsTest(path) }

// Classify returns the strictest tier among files.
func Classify(files []string, risk []string) Tier {
	tier := TierDocs
	for _, f := range files {
		if f == "" {
			continue
		}
		c := TierCode
		switch {
		case IsDocs(f):
			c = TierDocs
		case IsTest(f):
			c = TierTest
		case matchAny(risk, f):
			c = TierRisk
		}
		if c.rank() > tier.rank() {
			tier = c
		}
	}
	return tier
}

// SkipFor is the --skip value a tier's run should use. Review is never
// skipped: it catches prose that contradicts itself and test fixtures that
// keep a broken path green. CI is not listed: the global ci_mode owns it.
func SkipFor(tier Tier) string {
	switch tier {
	case TierDocs:
		return "test,lint,document"
	case TierTest:
		return "document"
	default:
		return ""
	}
}

// PreRunAdvice is the review to do before the run. The run's own review is the
// universal pass; anything before it is extra.
func PreRunAdvice(tier Tier, productionLines int) string {
	small := productionLines <= SmallChangeLines
	switch tier {
	case TierDocs, TierTest:
		return "none - the run's review covers it"
	case TierRisk:
		if small {
			return fmt.Sprintf("`/code-review high` (an independent opinion on a risk path); no simplify - %d production lines or fewer to simplify", SmallChangeLines)
		}
		return "`/code-review high` (an independent opinion on a risk path), then a simplification pass on a cheap model"
	default:
		if small {
			return fmt.Sprintf("none - %d production lines or fewer, the run's review covers it", SmallChangeLines)
		}
		return "optional `/code-review low`; for cleanups, a simplification pass on a cheap model"
	}
}

// Change is the classified diff of a branch against its base.
type Change struct {
	Base            string
	Files           []string
	ProductionFiles int
	ProductionLines int
	RiskPaths       []string
	// ExcludedFiles and ExcludedLines are the generated, lockfile and spec
	// or registry JSON files kept out of ProductionFiles and ProductionLines.
	ExcludedFiles int
	ExcludedLines int
	// RiskLines is the share of ProductionLines on RiskPaths. A risk path
	// whose lines are excluded still lands in RiskPaths (it keeps the strong
	// review) but adds nothing here.
	RiskLines int
	// FixCommits counts the non-merge commits of base..head that isPoolFix
	// reads as a separate fix: a bundle of fixes, not one feature.
	FixCommits int
	Tier       Tier
}

// Small reports whether the change is small enough to ride with a sibling.
func (c Change) Small() bool { return c.ProductionLines <= SmallChangeLines }

// ReadPatternFile reads one glob per line, skipping blanks and # comments. A
// missing file is no patterns.
func ReadPatternFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// reviewFollowUp is the rest of a fix subject, after "fix", an optional
// "(scope)", "!" and ":", that answers review on the same change: "Fix review
// comments", "fix: address PR feedback", "fix: the code review findings".
var reviewFollowUp = regexp.MustCompile(`^(address(es|ed|ing)?\s+)?(the\s+)?(code\s+)?(review|pr)\s+(comments?|feedback|findings|nits)\b`)

var fixPrefix = regexp.MustCompile(`^fix(es|ed)?(\([^)]*\))?!?:?\s*`)

// isPoolFix reports whether a commit subject reads as one separate fix of a
// pool rather than part of one change. It is a subject that starts with "fix"
// (any case), except a git autosquash commit ("fixup! ", "squash! ",
// "amend! ") and a fix subject that answers review comments or feedback
// (reviewFollowUp). The pipeline's own fix-round commits use the
// commit.fix_message subject, by default "no-mistakes(<step>): ...", so they
// never start with "fix"; a custom commit.fix_message that does start with
// "fix" counts its commits.
func isPoolFix(subject string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	for _, autosquash := range []string{"fixup!", "squash!", "amend!"} {
		if strings.HasPrefix(s, autosquash) {
			return false
		}
	}
	prefix := fixPrefix.FindString(s)
	if prefix == "" {
		return false
	}
	return !reviewFollowUp.MatchString(s[len(prefix):])
}

// RiskPatterns returns the default risk globs plus the repository's own
// .nm-triage-risk list.
func RiskPatterns(repoRoot string) ([]string, error) {
	extra, err := ReadPatternFile(filepath.Join(repoRoot, ".nm-triage-risk"))
	if err != nil {
		return nil, fmt.Errorf("read .nm-triage-risk: %w", err)
	}
	return append(append([]string(nil), DefaultRiskPatterns...), extra...), nil
}

// DiffChange classifies base..head in the repository at dir.
func DiffChange(ctx context.Context, dir, base, head string, risk []string) (Change, error) {
	c := Change{Base: base}
	gen, err := newGeneratedDetector(ctx, dir, base, head)
	if err != nil {
		return c, fmt.Errorf("read %s: %w", GeneratedOverrideFile, err)
	}
	out, _, err := gitOutput(ctx, dir, nil, "-c", "core.quotePath=false", "diff", "--no-renames", "--no-ext-diff", "--numstat", "-z", base+".."+head)
	if err != nil {
		return c, err
	}
	type numstat struct {
		path  string
		lines int
	}
	var production []numstat
	var productionPaths []string
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		c.Files = append(c.Files, path)
		if !IsProduction(path) {
			continue
		}
		n := 0
		if parts[0] != "-" {
			added, _ := strconv.Atoi(parts[0])
			deleted, _ := strconv.Atoi(parts[1])
			n = added + deleted
		}
		production = append(production, numstat{path, n})
		productionPaths = append(productionPaths, path)
	}
	excluded, err := gen.Excluded(productionPaths)
	if err != nil {
		return c, err
	}
	for _, f := range production {
		n := f.lines
		if excluded[f.path] {
			c.ExcludedFiles++
			c.ExcludedLines += n
			n = 0
		} else {
			c.ProductionFiles++
			c.ProductionLines += n
		}
		if matchAny(risk, f.path) {
			c.RiskPaths = append(c.RiskPaths, f.path)
			c.RiskLines += n
		}
	}
	c.Tier = Classify(c.Files, risk)
	if len(c.RiskPaths) > 0 {
		c.Tier = TierRisk
	}
	log, _, err := gitOutput(ctx, dir, nil, "log", "--no-merges", "--format=%s", base+".."+head)
	if err != nil {
		return c, err
	}
	for _, subject := range strings.Split(log, "\n") {
		if isPoolFix(subject) {
			c.FixCommits++
		}
	}
	return c, nil
}
