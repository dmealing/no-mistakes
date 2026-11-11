package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RunsMidStep counts what a clean stop would fail: pending or running runs
// that are not parked at a gate.
func TestRunsMidStepSkipsParkedAndFinishedRuns(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://github.com/owner/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := d.InsertRun(repo.ID, "refs/heads/pending", "a", "b")
	running, _ := d.InsertRun(repo.ID, "refs/heads/running", "a", "b")
	if err := d.UpdateRunStatus(running.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	parked, _ := d.InsertRun(repo.ID, "refs/heads/parked", "a", "b")
	if err := d.UpdateRunStatus(parked.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(parked.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.ParkStepForApproval(parked.ID, step.ID, types.StepStatusAwaitingApproval, 0, 1, nil); err != nil {
		t.Fatal(err)
	}
	done, _ := d.InsertRun(repo.ID, "refs/heads/done", "a", "b")
	if err := d.UpdateRunStatus(done.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	runs, err := d.RunsMidStep()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range runs {
		got[r.ID] = true
	}
	if len(runs) != 2 || !got[pending.ID] || !got[running.ID] {
		t.Fatalf("RunsMidStep = %+v, want only the pending and the running (unparked) run", runs)
	}
}
