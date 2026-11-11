package triage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const fixtureUpstream = "https://github.com/owner/repo.git"

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func commit(t *testing.T, dir, msg string, files map[string]string) string {
	t.Helper()
	writeTree(t, dir, files)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

func lines(n int, prefix string) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%s %d\n", prefix, i)
	}
	return b.String()
}

// rulesFixture is a checkout (work) and a run database whose in-validation
// branches were pushed from ANOTHER checkout of the same upstream into that
// checkout's gate: their commits exist only in the gate, so the conflict
// check must read them from there.
type rulesFixture struct {
	work, other string
	base        string
	p           *paths.Paths
	d           *db.DB
	mine, peer  *db.Repo
}

func newRulesFixture(t *testing.T) *rulesFixture {
	t.Helper()
	root := t.TempDir()
	f := &rulesFixture{work: filepath.Join(root, "work"), other: filepath.Join(root, "other"), p: paths.WithRoot(filepath.Join(root, "nm"))}
	if err := os.MkdirAll(f.p.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, f.work, "init", "-q", "-b", "main")
	f.base = commit(t, f.work, "base", map[string]string{"shared.go": "package x\n\nvar v = 1\n", "README.md": "readme\n"})
	git(t, root, "clone", "-q", f.work, f.other)

	d, err := db.Open(f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	f.d = d
	if f.mine, err = d.InsertRepoWithID("repo-mine", f.work, fixtureUpstream, "main"); err != nil {
		t.Fatal(err)
	}
	// The same upstream registered from another checkout, SSH spelling.
	if f.peer, err = d.InsertRepoWithID("repo-peer", f.other, "git@github.com:owner/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	git(t, root, "clone", "-q", "--bare", f.other, f.p.RepoDir(f.peer.ID))
	return f
}

// lane commits a branch in the other checkout, pushes it to that checkout's
// gate, and records an active run for it.
func (f *rulesFixture) lane(t *testing.T, branch string, files map[string]string) *db.Run {
	t.Helper()
	git(t, f.other, "checkout", "-q", "-b", branch, f.base)
	head := commit(t, f.other, branch, files)
	git(t, f.other, "push", "-q", f.p.RepoDir(f.peer.ID), branch)
	run, err := f.d.InsertRun(f.peer.ID, "refs/heads/"+branch, head, f.base)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// change commits this checkout's branch and runs the check on it.
func (f *rulesFixture) check(t *testing.T, files map[string]string) Result {
	t.Helper()
	git(t, f.work, "checkout", "-q", "-B", "mine", f.base)
	head := commit(t, f.work, "mine", files)
	return Check(context.Background(), Input{
		Dir: f.work, Head: head, Branch: "mine", Base: f.base,
		Paths: f.p, DB: f.d, Repo: f.mine, ProjectKey: ProjectKey(f.mine.UpstreamURL),
	})
}

func stopRules(res Result) string { return strings.Join(res.StopRules(), ",") }

func TestCheck_StopsOnAConflictWithABranchInValidation(t *testing.T) {
	f := newRulesFixture(t)
	f.lane(t, "lane-a", map[string]string{"shared.go": "package x\n\nvar v = 2\n"})

	res := f.check(t, map[string]string{"shared.go": "package x\n\nvar v = 3\n", "big.go": lines(400, "// big")})
	if res.ConflictsErr != nil || len(res.Unchecked) > 0 {
		t.Fatalf("conflict check did not run: err=%v unchecked=%v", res.ConflictsErr, res.Unchecked)
	}
	if got := stopRules(res); got != string(RuleInValidationConflict) {
		t.Fatalf("stops = %q, want only %s", got, RuleInValidationConflict)
	}
	if !strings.Contains(res.Stops[0].Detail, "lane-a") || !strings.Contains(res.Stops[0].Detail, "shared.go") {
		t.Fatalf("stop detail does not name the branch and file: %s", res.Stops[0].Detail)
	}
	if !res.Overridable() {
		t.Fatal("a conflict stop must be overridable with a reason")
	}
	// The test merge wrote nothing: the lane's commit is still absent here.
	if err := exec.Command("git", "-C", f.work, "cat-file", "-e", res.Lanes[0].HeadSHA).Run(); err == nil {
		t.Fatal("the conflict check copied the lane's commit into the checkout")
	}
}

func TestCheck_StopsTheThirdBranchOfAProject(t *testing.T) {
	f := newRulesFixture(t)
	f.lane(t, "lane-a", map[string]string{"a.go": "package x\n"})
	f.lane(t, "lane-b", map[string]string{"b.go": "package x\n"})

	res := f.check(t, map[string]string{"big.go": lines(400, "// big")})
	if got := stopRules(res); got != string(RuleThirdBranch) {
		t.Fatalf("stops = %q, want only %s", got, RuleThirdBranch)
	}
	if len(res.Lanes) != 2 {
		t.Fatalf("lanes = %+v", res.Lanes)
	}
}

func TestCheck_StopsASmallChangeWithASiblingInValidation(t *testing.T) {
	f := newRulesFixture(t)
	f.lane(t, "lane-a", map[string]string{"a.go": "package x\n"})

	// Tests and docs do not count toward the size: 20 production lines.
	res := f.check(t, map[string]string{"small.go": lines(20, "// s"), "small_test.go": lines(500, "// t"), "docs/notes.md": lines(500, "n")})
	if res.Change == nil || res.Change.ProductionLines != 20 {
		t.Fatalf("change = %+v", res.Change)
	}
	if got := stopRules(res); got != string(RuleSmallWithSibling) {
		t.Fatalf("stops = %q, want only %s", got, RuleSmallWithSibling)
	}
}

func TestCheck_GoesWhenNoRuleHolds(t *testing.T) {
	f := newRulesFixture(t)
	// A parked lane is in validation too; one sibling is allowed.
	run := f.lane(t, "lane-a", map[string]string{"a.go": "package x\n"})
	step, err := f.d.InsertStepResult(run.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.ParkStepForApproval(run.ID, step.ID, "awaiting_approval", 0, 1, nil); err != nil {
		t.Fatal(err)
	}
	// A run of another project and a finished run of this one do not count.
	other, err := f.d.InsertRepoWithID("repo-elsewhere", t.TempDir(), "https://github.com/owner/another.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.InsertRun(other.ID, "refs/heads/x", f.base, f.base); err != nil {
		t.Fatal(err)
	}
	done, err := f.d.InsertRun(f.peer.ID, "refs/heads/finished", f.base, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(done.ID, "completed"); err != nil {
		t.Fatal(err)
	}

	res := f.check(t, map[string]string{"big.go": lines(400, "// big")})
	if len(res.Stops) != 0 {
		t.Fatalf("stops = %q, want none", stopRules(res))
	}
	if len(res.Lanes) != 1 || !res.Lanes[0].Parked || res.Lanes[0].Branch != "lane-a" {
		t.Fatalf("lanes = %+v", res.Lanes)
	}
}

func TestCheck_HoldStopsAndCannotBeOverridden(t *testing.T) {
	f := newRulesFixture(t)
	if err := os.WriteFile(HoldPath(f.p.Root()), []byte("who: installer\nwhy: install\nuntil: the install finishes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.check(t, map[string]string{"big.go": lines(400, "// big")})
	if got := stopRules(res); got != string(RuleHold) {
		t.Fatalf("stops = %q, want only %s", got, RuleHold)
	}
	if res.Overridable() {
		t.Fatal("a hold must not be overridable")
	}
}

func TestPostMerge(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "r")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	base := commit(t, repo, "base", map[string]string{"code.go": "a\n", "code_test.go": "t1\n"})
	git(t, repo, "checkout", "-q", "-b", "feature")
	gated := commit(t, repo, "feature", map[string]string{"code_test.go": "t-feature\n", "feat.go": "f\n"})
	git(t, repo, "checkout", "-q", "main")
	commit(t, repo, "main moves", map[string]string{"code_test.go": "t-main\n", "other.go": "o\n"})
	git(t, repo, "checkout", "-q", "feature")
	// The merge conflicts on the test file only; resolve it by hand.
	cmd := exec.Command("git", "merge", "-q", "main")
	cmd.Dir = repo
	_ = cmd.Run()
	commit(t, repo, "merge main", map[string]string{"code_test.go": "t-both\n"})

	v, err := PostMerge(context.Background(), repo, gated, "main")
	if err != nil {
		t.Fatal(err)
	}
	if v.Regate {
		t.Fatalf("a tests-only resolution must not need a new run: %v", v.Lines)
	}
	v, err = PostMerge(context.Background(), repo, base, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Regate {
		t.Fatalf("a merge not on top of the validated head must need a new run: %v", v.Lines)
	}
}

// A lane whose commit is in no object store this check can read is "cannot
// check", never a conflict: git merge-tree exits 1 for an unknown commit too.
func TestCheck_ALaneCommitThatIsNowhereIsUncheckedNotAConflict(t *testing.T) {
	f := newRulesFixture(t)
	if _, err := f.d.InsertRun(f.peer.ID, "refs/heads/ghost", strings.Repeat("a", 40), f.base); err != nil {
		t.Fatal(err)
	}
	res := f.check(t, map[string]string{"big.go": lines(400, "// big")})
	if len(res.Stops) != 0 {
		t.Fatalf("stops = %q, want none", stopRules(res))
	}
	if len(res.Unchecked) != 1 || !strings.Contains(res.Unchecked[0], "ghost") {
		t.Fatalf("unchecked = %v", res.Unchecked)
	}
}
