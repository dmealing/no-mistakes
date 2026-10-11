package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testSkipReason = "CI is local"

func TestExecutor_SkipStepWithReasonRecordsReason(t *testing.T) {
	database, p, run, repo := setupTest(t)
	review := newPassStep(types.StepReview)
	ci := newPassStep(types.StepCI)
	exec := NewExecutor(database, p, nil, nil, []Step{review, ci}, nil)
	exec.SkipStepWithReason(types.StepCI, testSkipReason)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := ci.callCount(); got != 0 {
		t.Fatalf("ci executed %d times, want 0", got)
	}
	if got := review.callCount(); got != 1 {
		t.Fatalf("review executed %d times, want 1", got)
	}
	assertSkippedWithReason(t, database, run.ID, types.StepCI, testSkipReason)
}

func TestExecutor_SkipStepWithReasonOverridesPlainSkip(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ci := newPassStep(types.StepCI)
	exec := NewExecutor(database, p, nil, nil, []Step{ci}, nil)
	exec.SetSkippedSteps([]types.StepName{types.StepCI})
	exec.SkipStepWithReason(types.StepCI, testSkipReason)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	assertSkippedWithReason(t, database, run.ID, types.StepCI, testSkipReason)
}

// A run parked before CI and recovered after a daemon restart must not start
// the CI step its launch would have skipped.
func TestExecutor_RecoveredRemainderHonorsSkipWithReason(t *testing.T) {
	database, p, run, repo := setupTest(t)
	test := newPassStep(types.StepTest)
	ci := newPassStep(types.StepCI)
	for _, name := range []types.StepName{types.StepTest, types.StepCI} {
		if _, err := database.InsertStepResult(run.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	exec := NewExecutor(database, p, nil, nil, []Step{test, ci}, nil)
	exec.SkipStepWithReason(types.StepCI, testSkipReason)
	exec.initializeRunScopes(run.ID)

	if err := exec.executeRecoveredRemainder(context.Background(), run, repo, t.TempDir(), t.TempDir(), 0, false); err != nil {
		t.Fatalf("executeRecoveredRemainder() error = %v", err)
	}
	if got := test.callCount(); got != 1 {
		t.Fatalf("test executed %d times, want 1", got)
	}
	if got := ci.callCount(); got != 0 {
		t.Fatalf("ci executed %d times, want 0", got)
	}
	assertSkippedWithReason(t, database, run.ID, types.StepCI, testSkipReason)
}

func assertSkippedWithReason(t *testing.T, database *db.DB, runID string, step types.StepName, reason string) {
	t.Helper()
	results, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.StepName != step {
			continue
		}
		if result.Status != types.StepStatusSkipped {
			t.Fatalf("%s status = %s, want %s", step, result.Status, types.StepStatusSkipped)
		}
		if result.SkipReason == nil || *result.SkipReason != reason {
			t.Fatalf("%s skip reason = %v, want %q", step, result.SkipReason, reason)
		}
		return
	}
	t.Fatalf("no %s step result", step)
}
