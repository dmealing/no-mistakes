package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// Under review.coverage_first_round_only the automatic focused coverage pass
// runs on the first review round only. A post-fix re-review that omits a file
// skips the pass and parks on the named gap instead: the certification rule
// is unchanged, only the extra turn is not spent.
func TestReviewStep_CoveragePassRunsOnTheFirstRoundOnly(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := twoFileRepo(t)
	ag := newCoverageScriptedAgent(t, []coverageScript{
		{onCompletion: false, output: coverageFindingJSON([]string{"feature.txt"})},
		{onCompletion: true, output: coverageFindingJSON([]string{"feature2.txt"})},
		// The re-review's lossy record omits a file; no completion turn follows.
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

	sctx.Fixing = true
	sctx.SkipFixExecution = true
	second, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("rereview Execute() error = %v", err)
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
}
