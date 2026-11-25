package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// decided is a finding an earlier run's Review left unresolved; a later
// Review that carried that approval records it beside its notice.
const decided = `{"id":"review-1","severity":"warning","file":"feature.txt","line":1,"description":"left as a note","action":"auto-fix"}`

// A Review that carried an earlier approval (review.incremental) says where
// the approval came from, in the summary line and in the round details, keeps
// the earlier run's unresolved findings listed, and never counts them or the
// notice as its own findings.
func TestBuildPipelineSummary_ReviewCarriedApproval(t *testing.T) {
	t.Parallel()
	for name, findings := range map[string]string{
		"with the earlier run's findings":       `{"findings":[{"id":"` + types.FindingIDReviewCarried + `","severity":"info","description":"review carried from run 01ABC at 1234567 (no new branch work since the approved head)","action":"no-op"}],"risk_level":"low","risk_rationale":"approval carried from an earlier review","carried_review":{"source_run_id":"01ABC","findings":[` + decided + `]}}`,
		"recorded before findings were carried": `{"findings":[{"id":"` + types.FindingIDReviewCarried + `","severity":"info","description":"review carried from run 01ABC at 1234567 (no new branch work since the approved head)","action":"no-op"}],"risk_level":"low","risk_rationale":"approval carried from an earlier review"}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
			rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings}}}

			md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

			if !strings.Contains(md, "♻️ **Review** - review carried from run 01ABC at 1234567") {
				t.Fatalf("summary line does not name the carried approval:\n%s", md)
			}
			if strings.Contains(md, "1 info") || strings.Contains(md, "- ℹ️ review carried") {
				t.Fatalf("the carried notice rendered as a finding:\n%s", md)
			}
			listed := strings.Contains(md, "Findings carried from run 01ABC, already decided there:\n\n- ⚠️ `feature.txt:1` - left as a note")
			if listed != strings.Contains(findings, "carried_review") {
				t.Fatalf("the PR body lists the carried findings = %v, want %v:\n%s", listed, strings.Contains(findings, "carried_review"), md)
			}
		})
	}
}

// An incremental Review judged only the changes since an earlier approval.
// Its PR entry says so and lists the findings decided there, beside whatever
// it found itself, and its summary line counts only its own findings.
func TestBuildPipelineSummary_IncrementalReviewShowsWhatItCarried(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		own   string
		line  string
		owned string
	}{
		"with findings of its own": {
			own:   `{"id":"review-9","severity":"warning","file":"new.txt","line":3,"description":"new problem","action":"ask-user"}`,
			line:  "⚠️ **Review** - 1 warning",
			owned: "- ⚠️ `new.txt:3` - new problem",
		},
		"with none of its own": {line: "✅ **Review** - passed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			incremental := `{"findings":[` + tc.own + `],"risk_level":"low","risk_rationale":"ok","review_coverage_scope":{"version":1,"base_sha":"bbbbbbb","incremental":true},"carried_review":{"source_run_id":"01ABC","findings":[` + decided + `]}}`
			steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &incremental}}
			rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &incremental}}}

			md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

			for _, want := range []string{tc.line, "♻️ Reviewed only the branch's changes since run 01ABC approved", "Findings carried from run 01ABC, already decided there:\n\n- ⚠️ `feature.txt:1` - left as a note", tc.owned} {
				if !strings.Contains(md, want) {
					t.Fatalf("the Review entry is missing %q:\n%s", want, md)
				}
			}
			if strings.Contains(md, "2 warnings") {
				t.Fatalf("the summary line counted a carried finding:\n%s", md)
			}
		})
	}
}
