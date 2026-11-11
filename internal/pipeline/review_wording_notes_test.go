package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	roundOneBug     = `{"findings":[{"severity":"error","file":"store.go","line":3,"description":"nil dereference","action":"auto-fix"}],"summary":"1 issue"}`
	wordingNote     = `{"severity":"info","file":"main.go","line":7,"description":"the log message says 'retrys'","action":"auto-fix","info_kind":"wording"}`
	commentNote     = `{"severity":"info","file":"main.go","line":9,"description":"move the comment above the loop","action":"auto-fix","info_kind":"comment-placement"}`
	weakTestNote    = `{"severity":"info","file":"main_test.go","line":12,"description":"the test never asserts the error path","action":"auto-fix","info_kind":"other"}`
	unclassifiedNit = `{"severity":"info","file":"main.go","line":5,"description":"could simplify","action":"auto-fix"}`
)

// reviewRoundsStep returns rounds[i] as the findings of its i-th call (and a
// clean review after them), reviewing every changed file every time. Round 1's
// bug is in store.go and the later notes elsewhere: a rereview that still
// reports anything in the fixed file keeps the fixed defect outstanding.
func reviewRoundsStep(calls *int, rounds ...string) *adaptiveCallStep {
	return &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			*calls++
			paths := []string{"main.go", "main_test.go", "store.go"}
			out := &StepOutcome{ReviewedPaths: paths, ReviewablePaths: paths}
			if *calls <= len(rounds) && rounds[*calls-1] != "" {
				out.Findings = rounds[*calls-1]
				out.AutoFixable = true
				out.NeedsApproval = strings.Contains(out.Findings, `"error"`) || strings.Contains(out.Findings, `"warning"`)
			}
			return out, nil
		},
	}
}

func runReviewRounds(t *testing.T, rounds ...string) (calls int, status types.RunStatus, lastRoundFindings string) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	step := reviewRoundsStep(&calls, rounds...)
	exec := NewExecutor(database, p, &config.Config{AutoFix: config.AutoFix{Review: 5}}, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	updated, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps = %v, %v", steps, err)
	}
	rs, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil || len(rs) == 0 {
		t.Fatalf("rounds = %v, %v", rs, err)
	}
	if last := rs[len(rs)-1]; last.FindingsJSON != nil {
		lastRoundFindings = *last.FindingsJSON
	}
	return calls, updated.Status, lastRoundFindings
}

func findingsOf(items ...string) string {
	return `{"findings":[` + strings.Join(items, ",") + `],"summary":"notes"}`
}

// A wording note in the file whose defect a fix round just addressed keeps
// that defect outstanding (the coverage rule cannot tell the note from the
// defect restated), so the skip never clears an unverified fix.
func TestReview_WordingNoteInTheFixedFileDoesNotClearTheFix(t *testing.T) {
	sameFileNote := strings.Replace(wordingNote, `"main.go"`, `"store.go"`, 1)
	calls, _, _ := runReviewRounds(t, roundOneBug, findingsOf(sameFileNote))
	if calls != 3 {
		t.Fatalf("review ran %d times, want 3 (the fixed defect is not verified yet)", calls)
	}
}

// After round 1 a Review whose only findings left are wording and
// comment-placement notes completes without another fix round; the notes stay
// on the round for the PR body.
func TestReview_OnlyWordingNotesAfterRoundOneSkipTheFixRound(t *testing.T) {
	calls, status, last := runReviewRounds(t, roundOneBug, findingsOf(wordingNote, commentNote))
	if calls != 2 {
		t.Fatalf("review ran %d times, want 2 (round 1, its fix round, and no fix round for the notes)", calls)
	}
	if status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", status)
	}
	if !strings.Contains(last, "retrys") || !strings.Contains(last, "move the comment") {
		t.Fatalf("the notes were not kept on the last round: %s", last)
	}
}

// A weak test is an info finding too, but not a wording note: it still gets
// its fix round.
func TestReview_WeakTestInfoFindingIsStillFixed(t *testing.T) {
	calls, status, _ := runReviewRounds(t, roundOneBug, findingsOf(wordingNote, weakTestNote))
	if calls != 3 {
		t.Fatalf("review ran %d times, want 3 (the weak test needs a fix round)", calls)
	}
	if status != types.RunCompleted {
		t.Fatalf("run status = %s, want completed", status)
	}
}

// An info finding the reviewer did not classify is not a wording note.
func TestReview_UnclassifiedInfoFindingIsStillFixed(t *testing.T) {
	calls, _, _ := runReviewRounds(t, roundOneBug, findingsOf(unclassifiedNit))
	if calls != 3 {
		t.Fatalf("review ran %d times, want 3", calls)
	}
}

// Round 1's wording notes are still fixed: the skip applies only after it.
func TestReview_RoundOneWordingNotesAreStillFixed(t *testing.T) {
	calls, _, _ := runReviewRounds(t, findingsOf(wordingNote))
	if calls != 2 {
		t.Fatalf("review ran %d times, want 2 (round 1 notes get a fix round)", calls)
	}
}

func TestOnlyWordingNotesJSON(t *testing.T) {
	askUser := strings.Replace(wordingNote, `"auto-fix"`, `"ask-user"`, 1)
	warning := strings.Replace(wordingNote, `"info"`, `"warning"`, 1)
	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{findingsOf(), false},
		{findingsOf(wordingNote, commentNote), true},
		{findingsOf(wordingNote, weakTestNote), false},
		{findingsOf(unclassifiedNit), false},
		{findingsOf(askUser), false},
		{findingsOf(warning), false},
		{"not json", false},
	}
	for _, c := range cases {
		if got := onlyWordingNotesJSON(c.raw); got != c.want {
			t.Errorf("onlyWordingNotesJSON(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}
