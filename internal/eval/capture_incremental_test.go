package eval

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// scopedFindingsJSON is a review round's findings carrying the coverage scope
// the Review step records, which names the base the round judged from and
// whether that base came from an incremental Review.
func scopedFindingsJSON(t *testing.T, run *db.Run, stepID, base string, incremental bool, items []types.Finding) string {
	t.Helper()
	payload, err := json.Marshal(types.Findings{
		Items: items, RiskLevel: "low", RiskRationale: "clean", RiskScope: types.FindingsRiskScopeSourceOrExternal,
		ReviewCoverageScope: &types.ReviewCoverageScope{Version: 1, RunID: run.ID, StepResultID: stepID, HeadSHA: run.HeadSHA, RunBaseSHA: run.BaseSHA, BaseSHA: base, TrustedConfigSHA: run.BaseSHA, Paths: []string{"main.go"}, Incremental: incremental},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// completedReviewStep marks the run's Review step completed and returns its ID.
func completedReviewStep(t *testing.T, database *db.DB, run *db.Run) string {
	t.Helper()
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(steps[0].ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	return steps[0].ID
}

func assertNoCases(t *testing.T, store *Store) {
	t.Helper()
	cases, err := store.ListCases("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 0 {
		t.Fatalf("the corpus registered %d case(s) from a review a replay cannot reproduce", len(cases))
	}
}

// A replay rebuilds one review turn over the branch diff and has no earlier
// approval to start from. A Review that carried an approval ran no turn, and
// an incremental Review judged only the branch's work since its own base, so
// either would grade a whole-branch replay against gold drawn from part of it.
func TestCaptureRefusesACarriedOrIncrementalReview(t *testing.T) {
	ctx := context.Background()
	bug := types.Finding{ID: "real-bug", Severity: "error", File: "main.go", Line: 3, Description: "bug", Action: types.ActionAskUser}
	bugJSON, err := json.Marshal(bug)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("carried approval", func(t *testing.T) {
		p, database, run, _, _ := setupCapturedRunWithFindings(t, ctx, `{"findings":[{"id":"`+types.FindingIDReviewCarried+`","severity":"info","description":"review carried from run r0 at 1234567 (no new branch work since the approved head)","action":"no-op"}],"risk_level":"low","risk_rationale":"approval carried from an earlier review"}`)
		defer database.Close()
		completedReviewStep(t, database, run)
		store := mustOpenEval(t, p)

		if _, err := Capture(ctx, store, p, database, run.ID); !errors.Is(err, ErrNoCapturableReview) {
			t.Fatalf("capture error = %v, want no capturable review", err)
		}
		miss := []FindingGold{{ID: "missed-bug", Description: "a bug the carried review never looked for", Kind: GoldFalseNegative, Source: goldSourcePostPRMiss}}
		if _, err := IngestPostPRMiss(ctx, store, p, database, run.ID, miss); !errors.Is(err, ErrNoCapturableReview) {
			t.Fatalf("ingest error = %v, want no case for a carried review", err)
		}
		assertNoCases(t, store)
	})

	t.Run("incremental round and a later round that reviewed nothing", func(t *testing.T) {
		p, database, run, _, firstRound := setupCapturedRunWithRound(t, ctx, 0, func(run *db.Run, stepID string) string {
			return scopedFindingsJSON(t, run, stepID, run.HeadSHA, true, []types.Finding{bug})
		})
		defer database.Close()
		nothingLeft := `{"findings":[` + string(bugJSON) + `],"risk_level":"low","risk_rationale":"no reviewable changes"}`
		if _, err := database.InsertReviewStepRoundWithProvenance(firstRound.StepResultID, 2, "auto_fix", &nothingLeft, nil, run.HeadSHA, stringValue(firstRound.ReviewedHeadSHA), stringValue(firstRound.TrustedConfigSHA), firstRound.GlobalConfigYAML, firstRound.RepoConfigYAML, 5); err != nil {
			t.Fatal(err)
		}
		completedReviewStep(t, database, run)
		store := mustOpenEval(t, p)

		if _, err := Capture(ctx, store, p, database, run.ID); !errors.Is(err, ErrNoCapturableReview) {
			t.Fatalf("capture error = %v, want no capturable review", err)
		}
		assertNoCases(t, store)
	})

	// A Review that was neither carried nor incremental is captured whatever
	// base it recorded: a run whose PR base branch is not the default branch
	// records that branch's merge base, while replay derives its own.
	fullReviews := map[string]func(run *db.Run) string{
		"replay base":            func(run *db.Run) string { return run.BaseSHA },
		"another PR base branch": func(run *db.Run) string { return run.HeadSHA },
	}
	for name, recordedBase := range fullReviews {
		t.Run("full review from the "+name, func(t *testing.T) {
			p, database, run, _, firstRound := setupCapturedRunWithRound(t, ctx, 0, func(run *db.Run, stepID string) string {
				return scopedFindingsJSON(t, run, stepID, recordedBase(run), false, []types.Finding{bug})
			})
			defer database.Close()

			cases := captureAll(t, ctx, p, database, run.ID)
			if len(cases) != 1 || cases[0].SourceRoundID != firstRound.ID || !cases[0].Labels.HasGold() {
				t.Fatalf("captured = %#v, want the full review's round with its gold", cases)
			}
		})
	}
}
