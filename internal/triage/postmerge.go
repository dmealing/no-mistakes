package triage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// PostMergeVerdict says whether a branch that already passed validation needs
// another run after merging its base.
type PostMergeVerdict struct {
	// Regate is true when the merge needs a full run; false when running the
	// repository's local test suite is enough.
	Regate bool
	Lines  []string
}

// PostMerge judges HEAD as ONE merge of the base branch on top of the head
// that already passed validation (gated; empty assumes HEAD^1). The only
// unreviewed content of such a merge is the hand resolution of its
// conflicts, which `git show --remerge-diff` isolates:
//   - a resolution touching only docs/test files (or none) needs no new run:
//     run the full local test suite and, if green, push the merge;
//   - a resolution touching any other file, or a HEAD that is not that shape,
//     needs a full run.
//
// The repository's .nm-triage-nonprod adds globs that count as
// non-production here.
func PostMerge(ctx context.Context, dir, gated, baseRef string) (PostMergeVerdict, error) {
	var v PostMergeVerdict
	regate := func(line string) (PostMergeVerdict, error) {
		v.Regate = true
		v.Lines = append(v.Lines, line)
		return v, nil
	}
	p1, err := gitLine(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^1")
	if err != nil {
		return v, fmt.Errorf("post-merge: HEAD has no parent")
	}
	p2, err := gitLine(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^2")
	if err != nil {
		return regate("HEAD is not a merge commit -> full run")
	}
	if gated != "" {
		g, err := gitLine(ctx, dir, "rev-parse", "-q", "--verify", gated+"^{commit}")
		if err != nil {
			return v, fmt.Errorf("post-merge: unknown --gated commit %s", gated)
		}
		if p1 != g {
			return regate(fmt.Sprintf("HEAD is not one merge on top of the validated head %s -> full run", short(g)))
		}
	} else {
		v.Lines = append(v.Lines, fmt.Sprintf("no --gated commit given; assuming HEAD^1 (%s) is the head that passed validation", short(p1)))
	}
	if _, code, err := gitOutput(ctx, dir, nil, "merge-base", "--is-ancestor", p2, baseRef); err != nil || code != 0 {
		return regate(fmt.Sprintf("the merged-in side is not on %s -> full run", baseRef))
	}
	out, err := gitLine(ctx, dir, "-c", "core.quotePath=false", "show", "--remerge-diff", "--format=", "--name-only", "HEAD")
	if err != nil {
		return v, fmt.Errorf("post-merge: %w", err)
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		v.Lines = append(v.Lines, "clean merge, no hand resolution -> NO new run: run the full local test suite; if green, push and report ready")
		return v, nil
	}
	extra, err := ReadPatternFile(filepath.Join(dir, ".nm-triage-nonprod"))
	if err != nil {
		return v, fmt.Errorf("read .nm-triage-nonprod: %w", err)
	}
	var production []string
	for _, f := range files {
		if IsDocs(f) || IsTest(f) || matchAny(extra, f) {
			continue
		}
		production = append(production, f)
	}
	v.Lines = append(v.Lines, "resolved files: "+strings.Join(files, ", "))
	if len(production) > 0 {
		return regate("resolution touches production code: " + strings.Join(production, ", ") + " -> full run")
	}
	v.Lines = append(v.Lines, "resolution is tests/docs only -> NO new run: run the full local test suite; if green, push the merge to the PR branch and report it ready")
	return v, nil
}

func short(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}
