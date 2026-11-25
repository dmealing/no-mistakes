package pipeline

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepContext.PriorRounds is the first-round signal the review step's
// coverage_first_round_only reads. Fixing cannot give it: an answer round and
// an intent-correction rerun both re-execute with Fixing false. Every round
// after the first must therefore see a positive count, whatever response
// started it.
func TestExecutor_PriorRoundsCountsEveryEarlierRound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		respond func(*Executor) error
	}{
		{"answer round", func(e *Executor) error { return e.Respond(types.StepReview, types.ActionAnswer, nil) }},
		{"intent correction", func(e *Executor) error {
			_, err := e.RespondWithOverrides(types.StepReview, types.ActionCorrectIntent, nil, nil, nil, nil, "", ResponseOptions{CorrectedIntent: "the corrected goal"})
			return err
		}},
		{"operator fix", func(e *Executor) error { return e.Respond(types.StepReview, types.ActionFix, []string{"r1"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			type observed struct {
				priorRounds int
				fixing      bool
			}
			rounds := make(chan observed, 4)
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
				calls++
				rounds <- observed{sctx.PriorRounds, sctx.Fixing}
				if calls == 1 {
					return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"r1","severity":"warning","action":"ask-user","file":"a.go","description":"decide this"}]}`}, nil
				}
				return &StepOutcome{Findings: `{"findings":[]}`}, nil
			}}
			exec := NewExecutor(database, p, &config.Config{}, newFakeSessionAgent(), []Step{step}, nil)
			_, cancel := startExecutor(t, exec, run, repo, t.TempDir())
			defer cancel()
			next := func() observed {
				t.Helper()
				select {
				case got := <-rounds:
					return got
				case <-time.After(10 * time.Second):
					t.Fatal("review round never ran")
				}
				return observed{}
			}
			if first := next(); first.priorRounds != 0 {
				t.Fatalf("first round PriorRounds = %d, want 0", first.priorRounds)
			}
			waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
			if err := tc.respond(exec); err != nil {
				t.Fatal(err)
			}
			second := next()
			if second.priorRounds != 1 {
				t.Fatalf("second round PriorRounds = %d, want 1", second.priorRounds)
			}
			if tc.name != "operator fix" && second.fixing {
				t.Fatalf("%s must re-execute with Fixing false, so Fixing cannot be the first-round signal", tc.name)
			}
		})
	}
}
