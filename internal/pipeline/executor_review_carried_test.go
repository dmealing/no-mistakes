package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A Review that carried an earlier approval (review.incremental) completes on
// its info notice alone: no fix round, no park, the approval recorded on the
// run, and the notice kept on the step result for the PR body, with the
// earlier run's findings beside it as a record and never as work. The round
// cap and auto-fix are on, so a notice or a carried finding mistaken for work
// would start a fix round.
func TestExecutor_CarriedReviewCompletesOnItsNotice(t *testing.T) {
	database, p, run, repo := setupTest(t)
	const head = "1111111111111111111111111111111111111111"
	carried := `{"findings":[{"id":"` + types.FindingIDReviewCarried + `","severity":"info","description":"review carried from run r0 at 2222222","action":"no-op"}],"risk_level":"low","risk_rationale":"carried","carried_review":{"source_run_id":"r0","findings":[{"id":"review-1","severity":"warning","file":"feature.txt","line":1,"description":"left as a note","action":"auto-fix"}]}}`
	calls := 0
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		calls++
		return &StepOutcome{Findings: carried, ReviewCoverageComplete: true, ReviewApprovedHeadSHA: head}, nil
	}}
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}
	cfg.Review.RoundCap = 2
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("review executions = %d, want 1 (no fix round on a carried approval)", calls)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ReviewApprovedHeadSHA == nil || *got.ReviewApprovedHeadSHA != head {
		t.Fatalf("review-approved head = %#v, want %s", got.ReviewApprovedHeadSHA, head)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusCompleted || steps[0].FindingsJSON == nil || !hasFindingID(*steps[0].FindingsJSON, types.FindingIDReviewCarried) {
		t.Fatalf("review step = %+v, want completed with the carried notice", steps[0])
	}
	findings, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if record := findings.CarriedReview; record == nil || record.SourceRunID != "r0" || len(record.Findings) != 1 || record.Findings[0].Description != "left as a note" {
		t.Fatalf("the step result lost the record of the earlier run's findings: %+v", record)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("a carried finding joined the step's own findings: %s", *steps[0].FindingsJSON)
	}
}
