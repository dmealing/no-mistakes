package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Conflict kinds. Only a conflict with a branch in validation stops a run;
// the base and open-PR checks are the advisory part of `axi triage`.
const (
	ConflictInValidation = "in-validation"
	ConflictBase         = "base"
	ConflictOpenPR       = "open-pr"
)

// MergeTarget is one commit to test-merge the branch against.
type MergeTarget struct {
	Kind   string
	Label  string
	Branch string
	SHA    string
}

// Conflict is a target the branch cannot merge with cleanly.
type Conflict struct {
	Kind   string
	Label  string
	Branch string
	Files  []string
}

// maxConflictFiles bounds the files named per conflict.
const maxConflictFiles = 8

// MergeConflicts test-merges head against every target and returns the ones
// that conflict. Nothing is written to any repository: git runs with a
// throwaway primary object directory, and the checkout's own objects plus
// objectDirs (the gate repositories that hold the in-validation branches)
// are read-only alternates. A target that cannot be checked (its commit is
// missing, git is too old) is returned in unchecked with the reason.
func MergeConflicts(ctx context.Context, dir, head string, targets []MergeTarget, objectDirs []string) (conflicts []Conflict, unchecked []string, err error) {
	if len(targets) == 0 {
		return nil, nil, nil
	}
	// `git merge-tree -h` exits 129 with its usage; a git older than 2.38
	// does not list --write-tree there.
	if usage, _ := exec.CommandContext(ctx, "git", "merge-tree", "-h").CombinedOutput(); !strings.Contains(string(usage), "--write-tree") {
		return nil, nil, fmt.Errorf("git is too old for merge-tree --write-tree (needs 2.38 or newer)")
	}
	own, err := gitLine(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, nil, err
	}
	scratch, err := os.MkdirTemp("", "nm-triage-objects-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(scratch)
	alternates := []string{own}
	for _, d := range objectDirs {
		if info, statErr := os.Stat(d); statErr == nil && info.IsDir() {
			alternates = append(alternates, d)
		}
	}
	env := []string{
		"GIT_OBJECT_DIRECTORY=" + scratch,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + strings.Join(alternates, string(os.PathListSeparator)),
	}
	for _, t := range targets {
		// merge-tree exits 1 for a conflict AND for a commit it cannot find,
		// so a commit missing from every object store must be ruled out
		// first: it is "cannot check", never a conflict that stops a run.
		if _, code, catErr := gitOutput(ctx, dir, env, "cat-file", "-e", t.SHA+"^{commit}"); catErr != nil || code != 0 {
			unchecked = append(unchecked, fmt.Sprintf("%s: commit %s is not available here", t.Label, short(t.SHA)))
			continue
		}
		out, code, runErr := gitOutput(ctx, dir, env, "-c", "core.quotePath=false", "merge-tree", "--write-tree", "--name-only", "--no-messages", head, t.SHA)
		if runErr != nil {
			unchecked = append(unchecked, fmt.Sprintf("%s: %v", t.Label, firstLine(runErr.Error())))
			continue
		}
		if code != 1 {
			continue
		}
		files := conflictedFiles(out)
		if len(files) == 0 {
			unchecked = append(unchecked, fmt.Sprintf("%s: the test merge failed without naming a conflicted file", t.Label))
			continue
		}
		conflicts = append(conflicts, Conflict{Kind: t.Kind, Label: t.Label, Branch: t.Branch, Files: files})
	}
	return conflicts, unchecked, nil
}

// conflictedFiles parses `merge-tree --name-only` output: the tree id, then
// one conflicted path per line up to the first blank line.
func conflictedFiles(out string) []string {
	lines := strings.Split(out, "\n")
	seen := map[string]bool{}
	var files []string
	for _, line := range lines[min(1, len(lines)):] {
		if line == "" {
			break
		}
		if !seen[line] {
			seen[line] = true
			files = append(files, line)
		}
	}
	sort.Strings(files)
	if len(files) > maxConflictFiles {
		files = files[:maxConflictFiles]
	}
	return files
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// LaneTargets turns lanes into merge targets and returns the gate object
// directories that hold their commits.
func LaneTargets(lanes []Lane) ([]MergeTarget, []string) {
	var targets []MergeTarget
	var dirs []string
	seenDir := map[string]bool{}
	for _, l := range lanes {
		if l.HeadSHA == "" {
			continue
		}
		targets = append(targets, MergeTarget{
			Kind:   ConflictInValidation,
			Label:  fmt.Sprintf("branch %s in validation (run %s)", l.Branch, l.RunID),
			Branch: l.Branch,
			SHA:    l.HeadSHA,
		})
		if l.GateDir != "" && !seenDir[l.GateDir] {
			seenDir[l.GateDir] = true
			dirs = append(dirs, filepath.Join(l.GateDir, "objects"))
		}
	}
	return targets, dirs
}

// DefaultBaseRef returns origin's default branch as a remote-tracking ref,
// falling back to origin/main.
func DefaultBaseRef(ctx context.Context, dir string) string {
	if ref, err := gitLine(ctx, dir, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil && ref != "" {
		return ref
	}
	return "origin/main"
}

// BaseTarget resolves the base ref as a merge target, fetching it first when
// fetch is set (best effort, as the old script did).
func BaseTarget(ctx context.Context, dir, baseRef string, fetch bool) (MergeTarget, error) {
	if fetch && strings.HasPrefix(baseRef, "origin/") {
		_, _, _ = gitOutput(ctx, dir, nil, "fetch", "-q", "origin", strings.TrimPrefix(baseRef, "origin/"))
	}
	sha, err := gitLine(ctx, dir, "rev-parse", "--verify", "-q", baseRef+"^{commit}")
	if err != nil {
		return MergeTarget{}, fmt.Errorf("resolve %s: %w", baseRef, err)
	}
	return MergeTarget{Kind: ConflictBase, Label: "base (" + baseRef + ")", Branch: baseRef, SHA: sha}, nil
}

// GitHubSlug returns owner/repo for a github.com remote, or "".
func GitHubSlug(remote string) string {
	key := ProjectKey(remote)
	if !strings.HasPrefix(key, "github.com/") {
		return ""
	}
	// ProjectKey lowercases; keep the original spelling for the API path.
	s := strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	for _, prefix := range []string{"git@github.com:", "https://github.com/", "ssh://git@github.com/", "http://github.com/"} {
		if strings.HasPrefix(s, prefix) {
			return strings.Trim(strings.TrimPrefix(s, prefix), "/")
		}
	}
	return strings.TrimPrefix(key, "github.com/")
}

type openPR struct {
	Number int    `json:"number"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha"`
}

// OpenPRTargets lists origin's other open, non-draft GitHub pull requests as
// merge targets, fetching a head commit the checkout lacks. skip names
// branches already checked (this branch, branches in validation).
func OpenPRTargets(ctx context.Context, dir, head string, skip map[string]bool) ([]MergeTarget, error) {
	remote, err := gitLine(ctx, dir, "remote", "get-url", "origin")
	if err != nil {
		return nil, fmt.Errorf("origin remote: %w", err)
	}
	slug := GitHubSlug(remote)
	if slug == "" {
		return nil, fmt.Errorf("origin is not GitHub: open PRs not checked")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("gh not installed: open PRs not checked")
	}
	cmd := exec.CommandContext(ctx, "gh", "api", "repos/"+slug+"/pulls?state=open&per_page=100",
		"--jq", `[.[] | select(.draft|not) | {number: .number, ref: .head.ref, sha: .head.sha}]`)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api failed: open PRs not checked")
	}
	var prs []openPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh api output: %w", err)
	}
	var targets []MergeTarget
	for _, pr := range prs {
		if skip[pr.Ref] || pr.SHA == head {
			continue
		}
		if _, err := gitLine(ctx, dir, "cat-file", "-e", pr.SHA+"^{commit}"); err != nil {
			_, _, _ = gitOutput(ctx, dir, nil, "fetch", "-q", "--no-write-fetch-head", "origin", fmt.Sprintf("refs/pull/%d/head", pr.Number))
		}
		targets = append(targets, MergeTarget{Kind: ConflictOpenPR, Label: fmt.Sprintf("open PR #%d (%s)", pr.Number, pr.Ref), Branch: pr.Ref, SHA: pr.SHA})
	}
	return targets, nil
}
