package db

import (
	"database/sql"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// LatestApprovedReview returns the newest Review of a repository branch that
// completed with a recorded approval: the run whose review_approved_head_sha
// is set and whose Review step row is completed, ordered by when that step
// completed. The current run is not excluded, so a run whose own Review
// already completed is its own latest approval. A revalidation revokes the
// run's approval (UpdateRunHeadSHAForRevalidation) and resets its Review row,
// so a revoked approval is never returned. It returns nil, nil when the
// branch has no such Review.
//
// The incremental Review (pipeline/steps/review_incremental.go) is the only
// consumer; everything it carries is re-proven from git and the returned
// rows, never trusted from this selection alone.
func (d *DB) LatestApprovedReview(repoID, branch string) (*Run, *StepResult, error) {
	var runID, stepID string
	err := d.sql.QueryRow(`
		SELECT r.id, s.id FROM runs r
		JOIN step_results s ON s.run_id = r.id
		WHERE r.repo_id = ? AND r.branch = ?
			AND r.review_approved_head_sha IS NOT NULL AND r.review_approved_head_sha != ''
			AND s.step_name = ? AND s.status = ?
		ORDER BY COALESCE(s.completed_at, r.created_at) DESC, r.created_at DESC, r.id DESC
		LIMIT 1`,
		repoID, branch, types.StepReview, types.StepStatusCompleted,
	).Scan(&runID, &stepID)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("find latest approved review: %w", err)
	}
	run, err := d.GetRun(runID)
	if err != nil {
		return nil, nil, err
	}
	step, err := d.GetStepResult(stepID)
	if err != nil {
		return nil, nil, err
	}
	if run == nil || step == nil {
		return nil, nil, fmt.Errorf("find latest approved review: run %s or its Review row disappeared", runID)
	}
	return run, step, nil
}
