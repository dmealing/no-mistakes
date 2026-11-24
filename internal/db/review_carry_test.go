package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// LatestApprovedReview returns the newest completed, approved Review of the
// branch, never another branch's, never one whose step did not complete, and
// never a run whose approval a revalidation revoked.
func TestLatestApprovedReview(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	approve := func(branch, head string, complete bool) *Run {
		t.Helper()
		run, err := d.InsertRun(repo.ID, branch, head, "base")
		if err != nil {
			t.Fatal(err)
		}
		step, err := d.InsertStepResult(run.ID, types.StepReview)
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			if err := d.CompleteReviewStep(step.ID, run.ID, head, 0, 0, ""); err != nil {
				t.Fatal(err)
			}
		}
		return run
	}

	if run, _, err := d.LatestApprovedReview(repo.ID, "feature"); err != nil || run != nil {
		t.Fatalf("empty branch = %v, %v; want nil, nil", run, err)
	}
	older := approve("feature", "aaa", true)
	approve("other", "bbb", true)
	approve("feature", "ccc", false)
	run, step, err := d.LatestApprovedReview(repo.ID, "feature")
	if err != nil || run == nil || run.ID != older.ID || step.StepName != types.StepReview {
		t.Fatalf("got %v, %v, %v; want run %s", run, step, err, older.ID)
	}

	newer := approve("feature", "ddd", true)
	if run, _, _ := d.LatestApprovedReview(repo.ID, "feature"); run == nil || run.ID != newer.ID {
		t.Fatalf("got %v, want the newer approval %s", run, newer.ID)
	}
	if err := d.UpdateRunHeadSHAForRevalidation(newer.ID, "eee"); err != nil {
		t.Fatal(err)
	}
	if run, _, _ := d.LatestApprovedReview(repo.ID, "feature"); run == nil || run.ID != older.ID {
		t.Fatalf("got %v, want the older approval once the newer one was revoked", run)
	}
}
