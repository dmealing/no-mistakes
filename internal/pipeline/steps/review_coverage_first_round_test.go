package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// Under review.coverage_first_round_only the automatic focused coverage pass
// runs on the step's first review round only (PriorRounds == 0). A later round
// that omits a file skips the pass and parks on the named gap instead,
// whether it is a post-fix rereview or a round that re-executes with Fixing
// false (an answer round or an intent-correction rerun): the certification
// rule is unchanged, only the extra turn is not spent.
func TestReviewStep_CoveragePassRunsOnTheFirstRoundOnly(t *testing.T) {
	t.Parallel()
	for name, later := range map[string]func(*pipeline.StepContext){
		"post-fix rereview": func(sctx *pipeline.StepContext) {
			sctx.Fixing = true
			sctx.SkipFixExecution = true
		},
		"answer or intent rerun": func(sctx *pipeline.StepContext) {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := twoFileRepo(t)
			ag := newCoverageScriptedAgent(t, []coverageScript{
				{onCompletion: false, output: coverageFindingJSON([]string{"feature.txt"})},
				{onCompletion: true, output: coverageFindingJSON([]string{"feature2.txt"})},
				// The later round's lossy record omits a file; no completion turn follows.
				{onCompletion: false, output: coverageFindingJSON([]string{"feature2.txt"})},
			})
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Review.CoverageFirstRoundOnly = true
			var logs []string
			sctx.Log = func(msg string) { logs = append(logs, msg) }

			first, err := (&ReviewStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("initial Execute() error = %v", err)
			}
			if first.NeedsApproval || !first.ReviewCoverageComplete {
				t.Fatalf("round 1 must still close its gap with the focused pass: %+v", first)
			}

			sctx.PriorRounds = 1
			later(sctx)
			second, err := (&ReviewStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("later Execute() error = %v", err)
			}
			if len(ag.calls) != 3 {
				t.Fatalf("review invocations = %d, want 3 (two review turns + one focused pass on round 1)", len(ag.calls))
			}
			if !second.NeedsApproval || second.ReviewCoverageComplete {
				t.Fatalf("a later round with a coverage gap must park, not certify: %+v", second)
			}
			joined := strings.Join(logs, "\n")
			if !strings.Contains(joined, "skipping the focused coverage pass: it runs on the first review round only") {
				t.Fatalf("the skipped pass must be logged; logs:\n%s", joined)
			}
			if !strings.Contains(joined, "feature.txt") {
				t.Fatalf("the park must name the uncovered file; logs:\n%s", joined)
			}
		})
	}
}
