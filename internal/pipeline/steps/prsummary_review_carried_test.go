package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A Review that carried an earlier approval (review.incremental) says where
// the approval came from, in the summary line and in the round details, and
// never lists the notice as an info finding.
func TestBuildPipelineSummary_ReviewCarriedApproval(t *testing.T) {
	t.Parallel()
	carried := `{"findings":[{"id":"` + types.FindingIDReviewCarried + `","severity":"info","description":"review carried from run 01ABC at 1234567 (no new branch work since the approved head)","action":"no-op"}],"risk_level":"low","risk_rationale":"approval carried from an earlier review"}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &carried}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &carried}}}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

	if !strings.Contains(md, "♻️ **Review** - review carried from run 01ABC at 1234567") {
		t.Fatalf("summary line does not name the carried approval:\n%s", md)
	}
	if strings.Contains(md, "1 info") || strings.Contains(md, "- ℹ️ review carried") {
		t.Fatalf("the carried notice rendered as a finding:\n%s", md)
	}
}
