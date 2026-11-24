//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewPromptMarker is the opening line of the Review step's prompt. Every
// reviewer turn carries it, so counting fake-agent invocations that contain it
// counts the Review turns a run paid for.
const reviewPromptMarker = "Review the code changes and return structured findings with a risk assessment."

// TestIncrementalReviewJourney drives the real daemon, pipeline, and CLI
// through the incremental Review on one branch:
//
//  1. a first push gets a full Review and is approved;
//  2. a push with a new commit reviews only that commit, with the earlier
//     approval as decided context;
//  3. a merge of main that touches no reviewed hunk is carried: zero reviewer
//     turns and a review-carried notice naming run 2;
//  4. a merge of main that touches a line within the 3-line margin of a
//     reviewed hunk gets a full Review;
//  5. a rewritten branch (amend plus force-push) gets a full Review.
//
// The oracle for each step is git itself (the commits pushed) and the fake
// agent's invocation log, not the planner's own output.
func TestIncrementalReviewJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	h.NMBin = stampedNoMistakesBinary(t)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// Shared base: ten numbered lines so a hunk can sit inside or outside the
	// 3-line margin of another change.
	var base strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&base, "line%d\n", i)
	}
	h.CommitChange("main", "README.md", base.String(), "seed ten lines")
	h.runGitMust(t, "push", "origin", "main")
	const branch = "incremental-journey"

	var log strings.Builder
	note := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		log.WriteString(line + "\n")
		t.Log(line)
	}

	// Pushes the current branch and waits for a run that did not exist before
	// the push to reach a terminal state.
	pushAndWait := func(description string) (*ipc.RunInfo, int) {
		t.Helper()
		known := map[string]bool{}
		for _, r := range h.Runs() {
			known[r.ID] = true
		}
		before := len(h.AgentInvocations())
		h.PushToGate(branch)
		run := waitForNewRun(t, h, known, branch, 240*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("%s: run %s status = %s, want completed; error=%v", description, run.ID, run.Status, run.Error)
		}
		reviewTurns := countReviewTurns(h.AgentInvocations()[before:])
		note("%s: run %s completed, review turns=%d", description, run.ID, reviewTurns)
		return run, reviewTurns
	}

	// Step 1: full Review on the first push.
	h.CommitChange(branch, "README.md", strings.Replace(base.String(), "line5\n", "line5 branch edit\n", 1), "branch edits line 5")
	run1, turns1 := pushAndWait("run1 first push")
	if turns1 == 0 {
		t.Fatalf("run1 is the first approval for the branch and must get a full Review with reviewer turns, got 0")
	}
	head1 := h.WorktreeRefSHA("HEAD")
	reviewStep1 := mustReviewStep(t, run1)
	if reviewStep1.Status != types.StepStatusCompleted {
		t.Fatalf("run1 review step status = %s", reviewStep1.Status)
	}
	if strings.Contains(readRunLog(t, h, run1.ID, "review"), "incremental review") {
		t.Fatalf("run1 has no prior approval and must not be incremental")
	}

	// Step 2: a new commit adds reviewable work. Only that commit is reviewed.
	h.CommitChange(branch, "feature.txt", "new feature\n", "add feature")
	run2, turns2 := pushAndWait("run2 new commit")
	if turns2 == 0 {
		t.Fatalf("run2 adds reviewable work and needs reviewer turns, got 0")
	}
	if !strings.Contains(readRunLog(t, h, run2.ID, "review"), "incremental review: run "+run1.ID+" approved head") {
		t.Fatalf("run2 review log does not name run1's approval:\n%s", readRunLog(t, h, run2.ID, "review"))
	}
	reviewPrompt := lastReviewPrompt(h.AgentInvocations())
	wantScope := "the review approved at " + head1
	if !strings.Contains(reviewPrompt, wantScope) {
		t.Fatalf("run2 review prompt does not scope the Review to changes since %s", head1)
	}
	head2 := h.WorktreeRefSHA("HEAD")
	note("run2 scope: changes since %s, head %s", head1, head2)

	// Step 3: main moves in a file the branch never touched, and the branch
	// merges it. The merge touches no reviewed hunk, so the approval carries.
	h.CommitChange("main", "other.txt", "unrelated base change\n", "base moves elsewhere")
	h.runGitMust(t, "push", "origin", "main")
	h.Checkout(branch)
	h.runGitMust(t, "merge", "--no-edit", "main")
	run3, turns3 := pushAndWait("run3 clean merge of main")
	if turns3 != 0 {
		t.Fatalf("run3 merges only untouched base changes and must carry with zero reviewer turns, got %d", turns3)
	}
	assertCarriedFrom(t, h, run3, run2.ID)
	note("run3 carried approval from run %s with zero reviewer turns", run2.ID)

	// Step 4: main changes line 7, two lines away from the reviewed line 5
	// edit. Git merges it cleanly, but it sits inside the 3-line margin, so
	// the Review must be full.
	h.Checkout("main")
	h.CommitChange("main", "README.md", strings.Replace(base.String(), "line7\n", "line7 base edit\n", 1), "base edits line 7")
	h.runGitMust(t, "push", "origin", "main")
	h.Checkout(branch)
	h.runGitMust(t, "merge", "--no-edit", "main")
	run4, turns4 := pushAndWait("run4 merge of main touching the reviewed margin")
	if turns4 == 0 {
		t.Fatalf("run4 base edit sits within the reviewed margin and must get a full Review, got 0 reviewer turns")
	}
	if !strings.Contains(readRunLog(t, h, run4.ID, "review"), "full review: base movement since the approved review touches reviewed code in README.md") {
		t.Fatalf("run4 log does not explain the full Review:\n%s", readRunLog(t, h, run4.ID, "review"))
	}
	note("run4 full Review: base movement touches reviewed code in README.md")

	// Step 5: the branch is rewritten (amend and force-push). The approved
	// head is no longer an ancestor, so the Review must be full.
	h.runGitMust(t, "commit", "--amend", "--no-edit", "--allow-empty")
	// Snapshot the known runs before the push: the push creates run5 at once.
	known := map[string]bool{}
	for _, r := range h.Runs() {
		known[r.ID] = true
	}
	before := len(h.AgentInvocations())
	h.runGitMust(t, "push", "-f", "no-mistakes", branch)
	run5 := waitForNewRun(t, h, known, branch, 240*time.Second)
	if run5.Status != types.RunCompleted {
		t.Fatalf("run5 rewritten status = %s, want completed; error=%v", run5.Status, run5.Error)
	}
	turns5 := countReviewTurns(h.AgentInvocations()[before:])
	if turns5 == 0 {
		t.Fatalf("run5 rewrote the branch and must get a full Review, got 0 reviewer turns")
	}
	if !strings.Contains(readRunLog(t, h, run5.ID, "review"), "is not an ancestor") {
		t.Fatalf("run5 log does not explain the full Review:\n%s", readRunLog(t, h, run5.ID, "review"))
	}
	note("run5 rewritten branch: full Review, review turns=%d", turns5)

	if out := os.Getenv("NM_E2E_EVIDENCE_FILE"); out != "" {
		if err := os.WriteFile(out, []byte(log.String()), 0o644); err != nil {
			t.Fatalf("write evidence: %v", err)
		}
	}
}

// stampedNoMistakesBinary builds the no-mistakes binary with a build identity
// the way release builds do. The harness build is unstamped, and an unstamped
// build records no identity, so a carry would be refused by design. The
// stamp is a fixed synthetic value.
func stampedNoMistakesBinary(t *testing.T) string {
	t.Helper()
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	out := filepath.Join(t.TempDir(), executableName("no-mistakes"))
	ldflags := "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Version=v0.0.0-e2e-incremental " +
		"-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=e2eincrementalbuild"
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, "./cmd/no-mistakes")
	cmd.Dir = repoRoot
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stamped no-mistakes: %v\n%s", err, data)
	}
	return out
}

func (h *Harness) runGitMust(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := h.runGit(ctx, h.WorkDir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// waitForNewRun returns the first terminal run on branch whose ID is not in
// known.
func waitForNewRun(t *testing.T, h *Harness, known map[string]bool, branch string, timeout time.Duration) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, r := range h.Runs() {
			if r.Branch != branch || known[r.ID] {
				continue
			}
			if !r.Status.Terminal() {
				// A run parked on a gate never finishes by itself: name the
				// parked step and its reason instead of waiting out the timeout.
				for _, step := range r.Steps {
					if step.Status == types.StepStatusAwaitingApproval {
						t.Fatalf("run %s parked at %s (%s): error=%v findings=%v", r.ID, step.StepName, step.Status, deref(step.Error), deref(step.FindingsJSON))
					}
				}
				continue
			}
			run := r
			return &run
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatalf("no terminal run for %s appeared within %v", branch, timeout)
	return nil
}


func countReviewTurns(invs []Invocation) int {
	n := 0
	for _, inv := range invs {
		if strings.Contains(inv.Prompt, reviewPromptMarker) {
			n++
		}
	}
	return n
}

func lastReviewPrompt(invs []Invocation) string {
	for i := len(invs) - 1; i >= 0; i-- {
		if strings.Contains(invs[i].Prompt, reviewPromptMarker) {
			return invs[i].Prompt
		}
	}
	return ""
}

func mustReviewStep(t *testing.T, run *ipc.RunInfo) ipc.StepResultInfo {
	t.Helper()
	step, ok := findStep(run.Steps, types.StepReview)
	if !ok {
		t.Fatalf("run %s has no review step", run.ID)
	}
	return step
}

// readRunLog returns the persisted step log for a run's step, the same file
// `axi logs` reads.
func readRunLog(t *testing.T, h *Harness, runID, step string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.NMHome, "logs", runID, step+".log"))
	if err != nil {
		t.Fatalf("read %s log for run %s: %v", step, runID, err)
	}
	return string(bytes.TrimSpace(data))
}

// assertCarriedFrom checks the stored Review result: the notice names the
// source run, and the step ran no reviewer turn.
func assertCarriedFrom(t *testing.T, h *Harness, run *ipc.RunInfo, sourceRunID string) {
	t.Helper()
	step := mustReviewStep(t, run)
	if step.FindingsJSON == nil {
		t.Fatalf("run %s review step recorded no findings", run.ID)
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse review findings: %v", err)
	}
	if findings.CarriedReview == nil || findings.CarriedReview.SourceRunID != sourceRunID {
		t.Fatalf("run %s carried review = %+v, want source run %s", run.ID, findings.CarriedReview, sourceRunID)
	}
	found := false
	for _, item := range findings.Items {
		if item.ID == types.FindingIDReviewCarried {
			found = true
		}
	}
	if !found {
		t.Fatalf("run %s review result lacks the %s notice: %+v", run.ID, types.FindingIDReviewCarried, findings.Items)
	}
	if got := readRunLog(t, h, run.ID, "review"); !strings.Contains(got, "review carried from run "+sourceRunID) {
		t.Fatalf("run %s review log does not record the carry:\n%s", run.ID, got)
	}
}
