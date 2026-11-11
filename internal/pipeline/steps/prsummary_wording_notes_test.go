package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A Review that completed on wording notes after its first round says they
// were left for the author with no fix round spent, never "still open".
func TestBuildPipelineSummary_ReviewWordingNotesLeftForTheAuthor(t *testing.T) {
	t.Parallel()
	bug := `{"findings":[{"id":"review-1","severity":"error","file":"store.go","line":3,"description":"nil dereference","action":"auto-fix"}],"summary":"1 issue"}`
	notes := `{"findings":[{"id":"review-2","severity":"info","file":"main.go","line":7,"description":"the log message says retrys","action":"auto-fix","info_kind":"wording"}],"summary":"1 note"}`
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepReview, Status: types.StepStatusCompleted, FindingsJSON: &notes}}
	rounds := map[string][]*db.StepRound{"s1": {
		{Round: 1, Trigger: "initial", FindingsJSON: &bug},
		{Round: 2, Trigger: "auto_fix", FindingsJSON: &notes},
	}}

	md, _ := BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)

	if !strings.Contains(md, "1 wording or comment-placement note left for the author (no fix round spent):") || !strings.Contains(md, "the log message says retrys") {
		t.Fatalf("wording notes not listed as left for the author:\n%s", md)
	}
	if strings.Contains(md, "still open") {
		t.Fatalf("wording notes rendered as still open:\n%s", md)
	}

	// A weak-test note in the same position is still open.
	weak := strings.Replace(notes, `"wording"`, `"other"`, 1)
	steps[0].FindingsJSON = &weak
	rounds["s1"][1].FindingsJSON = &weak
	md, _ = BuildPipelineSummary(steps, rounds, testPipelineHeadSHA)
	if strings.Contains(md, "left for the author") {
		t.Fatalf("a non-wording note was labelled as a wording note:\n%s", md)
	}
}
