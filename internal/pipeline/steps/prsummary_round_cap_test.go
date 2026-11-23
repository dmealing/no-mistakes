package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A Review completed at review.round_cap lists its remaining findings as
// unresolved notes, without the notice row itself, and never as "still open"
// or approved as-is.
func TestBuildPipelineSummary_ReviewRoundCapNotes(t *testing.T) {
	t.Parallel()
	first := `{"findings":[{"id":"review-1","severity":"warning","file":"store.go","line":3,"description":"could be tighter","action":"auto-fix"}],"summary":"1 issue"}`
	capped := `{"findings":[{"id":"review-1","severity":"warning","file":"store.go","line":3,"description":"could be tighter","action":"auto-fix"},{"id":"` + types.FindingIDReviewRoundCapNotes + `","severity":"info","description":"review round cap reached","action":"no-op"}],"summary":"1 issue"}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &capped}}
	rounds := map[string][]*db.StepRound{"s1": {
		{Round: 1, Trigger: "initial", FindingsJSON: &first},
		{Round: 2, Trigger: "auto_fix", FindingsJSON: &capped},
	}}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

	if !strings.Contains(md, "**Review** - 1 warning left as notes at the round cap") {
		t.Fatalf("summary line does not say the review ended at the round cap:\n%s", md)
	}
	if !strings.Contains(md, "Review round cap reached: 1 warning left unresolved as notes (no further fix round):") || !strings.Contains(md, "could be tighter") {
		t.Fatalf("remaining findings not listed as unresolved notes:\n%s", md)
	}
	if strings.Contains(md, "still open") || strings.Contains(md, "- ℹ️ review round cap reached") {
		t.Fatalf("capped notes rendered as still open, or the notice row leaked:\n%s", md)
	}
}

// A capped round kept across a restart at Review is no longer the step's last
// round; it still renders as unresolved notes, never as "still open".
func TestBuildPipelineSummary_ReviewRoundCapNotesOnEarlierRound(t *testing.T) {
	t.Parallel()
	first := `{"findings":[{"id":"review-1","severity":"warning","file":"store.go","line":3,"description":"could be tighter","action":"auto-fix"}],"summary":"1 issue"}`
	capped := `{"findings":[{"id":"review-1","severity":"warning","file":"store.go","line":3,"description":"could be tighter","action":"auto-fix"},{"id":"` + types.FindingIDReviewRoundCapNotes + `","severity":"info","description":"review round cap reached","action":"no-op"}],"summary":"1 issue"}`
	later := `{"findings":[{"id":"review-2","severity":"warning","file":"api.go","line":9,"description":"unrelated later finding","action":"auto-fix"}],"summary":"1 issue"}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &later}}
	rounds := map[string][]*db.StepRound{"s1": {
		{Round: 1, Trigger: "initial", FindingsJSON: &first},
		{Round: 2, Trigger: "auto_fix", FindingsJSON: &capped},
		{Round: 3, Trigger: "initial", FindingsJSON: &later},
	}}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

	if !strings.Contains(md, "Review round cap reached: 1 warning left unresolved as notes (no further fix round):") {
		t.Fatalf("earlier capped round not rendered as notes:\n%s", md)
	}
	if strings.Contains(md, "- ℹ️ review round cap reached") {
		t.Fatalf("notice row leaked as a finding:\n%s", md)
	}
}
