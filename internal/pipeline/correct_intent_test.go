package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const intentGateFindings = `{"findings":[` +
	`{"id":"r1","severity":"warning","action":"ask-user","category":"intent-conformance","file":"a.go","description":"intent asks for X; the change does Y"},` +
	`{"id":"r2","severity":"warning","action":"ask-user","file":"b.go","description":"no intent requirement needs the new flag"}` +
	`],"risk_level":"medium"}`

// A corrected intent at a parked review reviews the change again in the SAME
// run: the run's intent is replaced durably, the rerun sees the corrected
// intent as an explicit one, findings judged against the old intent do not
// survive into the rerun, and the round is recorded as "intent", not as a fix
// round. Before this, the only way to change the intent was to abort and start
// a new run, which threw away the finished review.
func TestExecutor_CorrectIntentReviewsAgainInTheSameRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	oldIntent := "add flag X"
	if err := database.UpdateRunIntent(run.ID, db.RunIntent{Summary: oldIntent, Source: db.RunIntentSourceAgent, Score: 1}); err != nil {
		t.Fatal(err)
	}
	run.Intent = &oldIntent
	corrected := "add flag X and flag Y, which the old intent left out"

	call := 0
	review := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			call++
			if call == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: intentGateFindings}, nil
			}
			if sctx.UserIntent != corrected || sctx.IntentSource != db.RunIntentSourceAgent {
				return nil, fmt.Errorf("rerun intent = %q (%s), want the corrected explicit intent", sctx.UserIntent, sctx.IntentSource)
			}
			if sctx.Fixing || sctx.FinalizingAnswers || sctx.SkipFixExecution || sctx.PreviousFindings != "" {
				return nil, fmt.Errorf("a corrected intent is a plain review, not a fix or answer round")
			}
			return &StepOutcome{Findings: `{"findings":[],"risk_level":"low"}`}, nil
		},
	}
	var laterIntent string
	later := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			laterIntent = sctx.UserIntent
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{review, later}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionCorrectIntent, nil, nil, nil, nil, "", ResponseOptions{CorrectedIntent: corrected}); err != nil {
		t.Fatalf("correct-intent: %v", err)
	}
	waitExecutorDone(t, done)

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed in the same run", got.Status)
	}
	if got.Intent == nil || *got.Intent != corrected || got.IntentSource == nil || *got.IntentSource != db.RunIntentSourceAgent {
		t.Fatalf("persisted intent = %v/%v, want the corrected explicit intent", got.Intent, got.IntentSource)
	}
	if laterIntent != corrected {
		t.Fatalf("a later step saw intent %q, want the corrected intent", laterIntent)
	}
	if call != 2 {
		t.Fatalf("review ran %d times, want the parked round plus one rerun", call)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sr *db.StepResult
	for _, s := range steps {
		if s.StepName == types.StepReview {
			sr = s
		}
	}
	if sr == nil {
		t.Fatal("no review step result")
	}
	if sr.Status != types.StepStatusCompleted {
		t.Fatalf("review status = %s, want completed", sr.Status)
	}
	if sr.FindingsJSON != nil && strings.Contains(*sr.FindingsJSON, "r1") {
		t.Fatalf("a finding judged against the old intent survived the rerun: %s", *sr.FindingsJSON)
	}
	rounds, err := database.GetRoundsByStep(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[1].Trigger != "intent" || rounds[1].IsFixRound() {
		t.Fatalf("rounds = %+v, want a second round triggered by \"intent\" that is not a fix round", rounds)
	}
}

// A corrected intent is refused, with the gate left parked, when it is sent to
// any step but Review, carries no text, carries a finding selection, matches a
// pinned reject rule, or rides on any other action.
func TestExecutor_CorrectIntentRefusals(t *testing.T) {
	t.Run("not review", func(t *testing.T) {
		database, p, run, repo := setupTest(t)
		step := newApprovalStep(types.StepDocument, `{"findings":[{"id":"d-1","severity":"warning","description":"waiting","action":"ask-user"}],"summary":"1 issue"}`)
		exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
		done, _ := startExecutor(t, exec, run, repo, t.TempDir())
		waitForStepStatus(t, database, run.ID, types.StepDocument, types.StepStatusAwaitingApproval)
		_, err := exec.RespondWithOverrides(types.StepDocument, types.ActionCorrectIntent, nil, nil, nil, nil, "", ResponseOptions{CorrectedIntent: "new"})
		if err == nil || !strings.Contains(err.Error(), "only to a parked Review") {
			t.Fatalf("correct-intent at document = %v, want a review-only refusal", err)
		}
		if err := exec.Respond(types.StepDocument, types.ActionApprove, nil); err != nil {
			t.Fatalf("gate did not stay parked: %v", err)
		}
		waitExecutorDone(t, done)
	})

	reject := &config.Config{}
	reject.Intent.Check = config.MergeIntentChecks(config.IntentCheckRaw{Mode: config.IntentCheckReject, Terms: []string{"forbidden"}})
	cases := []struct {
		name    string
		cfg     *config.Config
		action  types.ApprovalAction
		ids     []string
		intent  string
		wantErr string
	}{
		{"empty intent", &config.Config{}, types.ActionCorrectIntent, nil, "  ", "requires the corrected intent"},
		{"with findings", &config.Config{}, types.ActionCorrectIntent, []string{"r1"}, "new", "cannot select"},
		{"reject rule", reject, types.ActionCorrectIntent, nil, "a forbidden intent", "rejected by configured rules"},
		{"other action", &config.Config{}, types.ActionAbort, nil, "new", "only to correct-intent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := newApprovalStep(types.StepReview, intentGateFindings)
			exec := NewExecutor(database, p, tc.cfg, nil, []Step{step}, nil)
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			reason := ""
			if tc.action == types.ActionAbort {
				reason = "abort reason"
			}
			_, err := exec.RespondWithOverrides(types.StepReview, tc.action, tc.ids, nil, nil, nil, reason, ResponseOptions{CorrectedIntent: tc.intent})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want a refusal containing %q", err, tc.wantErr)
			}
			got, _ := database.GetRun(run.ID)
			if got.Intent != nil && *got.Intent == tc.intent {
				t.Fatal("a refused correction was persisted")
			}
			if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionAbort, nil, nil, nil, nil, "done", ResponseOptions{}); err != nil {
				t.Fatalf("gate did not stay parked: %v", err)
			}
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("executor timed out")
			}
		})
	}
}

// A daemon restart while the gate is parked must not change what a corrected
// intent does: Resume re-enters the review with the corrected intent and
// records the round as "intent".
func TestExecutor_RecoveredCorrectIntentReviewsAgain(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := intentGateFindings
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(stepResult.ID, 1, "initial", &findings, nil, "", 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	corrected := "the corrected intent"
	reviewed := make(chan string, 1)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			reviewed <- sctx.UserIntent
			return &StepOutcome{Findings: `{"findings":[],"risk_level":"low"}`}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, nil, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionCorrectIntent, nil, nil, nil, nil, "", ResponseOptions{CorrectedIntent: corrected}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate never accepted a corrected intent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case got := <-reviewed:
		if got != corrected {
			t.Fatalf("recovered rerun saw intent %q, want %q", got, corrected)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered review never reran")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovered executor timed out")
	}
	rounds, err := database.GetRoundsByStep(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || rounds[1].Trigger != "intent" {
		t.Fatalf("rounds = %+v, want the parked round plus an \"intent\" round", rounds)
	}
	got, _ := database.GetRun(run.ID)
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", got.Status)
	}
}

// Only a dispatched fix awaiting verification survives a corrected intent, and
// never an intent-conformance finding, which the correction itself decides.
func TestDropFindingsSupersededByIntent(t *testing.T) {
	raw := `{"findings":[` +
		`{"id":"r1","severity":"warning","action":"ask-user","category":"intent-conformance","file":"a.go","description":"off intent"},` +
		`{"id":"r2","severity":"error","action":"auto-fix","file":"b.go","description":"nil deref the fixer was sent to fix"},` +
		`{"id":"r3","severity":"warning","action":"ask-user","file":"c.go","description":"unrequired component"}` +
		`]}`
	got := dropFindingsSupersededByIntent(raw, []string{"r1", "r2"})
	ids := findingIDList(got)
	if len(ids) != 1 || ids[0] != "r2" {
		t.Fatalf("kept %v, want only the dispatched fix r2", ids)
	}
	if dropFindingsSupersededByIntent(raw, nil) != "" {
		t.Fatal("with nothing dispatched, every finding is reviewed again")
	}
}
