package steps

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// coldFixerHarness runs one review fix turn against a fixer session that was
// last used idle ago, after an earlier fix round of the same step recorded
// earlierSummary. It returns the fixer's call and the step log.
func coldFixerHarness(t *testing.T, idle time.Duration, earlierSummary string) (agent.RunOpts, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	mock := &sessionMockAgent{}
	mock.respond = func(opts agent.RunOpts) *agent.Result {
		if opts.Purpose == "review-fix" {
			return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}
		}
		j, _ := json.Marshal(cleanReviewFindings())
		return &agent.Result{Output: j}
	}
	sctx := newTestContextWithDBRecords(t, mock, dir, baseSHA, headSHA, config.Commands{})
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	if _, err := sctx.DB.InsertStepRound(sr.ID, 1, "initial", nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.InsertStepRound(sr.ID, 2, "auto_fix", nil, &earlierSummary, 0); err != nil {
		t.Fatal(err)
	}

	start := time.Unix(1_800_000_000, 0)
	clock := start
	sessions := pipeline.NewRunSessions(sctx.DB, sctx.Run.ID, mock, true)
	sessions.SetClock(func() time.Time { return clock })
	// The earlier fix round's turn minted the fixer session.
	if _, err := sessions.Run(context.Background(), mock, pipeline.SessionRoleFixer, agent.RunOpts{Purpose: "review-fix"}, nil); err != nil {
		t.Fatal(err)
	}
	clock = start.Add(idle)
	sctx.Sessions = sessions

	var logs strings.Builder
	sctx.Log = func(s string) { logs.WriteString(s + "\n") }
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"error","file":"main.go","line":3,"description":"nil deref","action":"auto-fix"}],"summary":"1 issue"}`
	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	fixes := fixCalls(mock.snapshot())
	if len(fixes) != 2 {
		t.Fatalf("expected the seeding turn plus one fix turn, got %d", len(fixes))
	}
	return fixes[1], logs.String()
}

// TestReviewFixer_ColdSessionStartsFreshWithASummary: a fixer session idle past
// the prompt-cache window is not resumed, because resuming re-sends its whole
// history uncached. The turn starts a fresh session, and the prompt carries a
// short summary of the earlier fix rounds so the new session is not blind.
func TestReviewFixer_ColdSessionStartsFreshWithASummary(t *testing.T) {
	t.Parallel()
	call, logs := coldFixerHarness(t, pipeline.SessionColdAfter+time.Minute, "guard the nil map in Parse")

	if call.Session == nil || call.Session.ID != "" {
		t.Fatalf("a cold fixer session must not be resumed, got %+v", call.Session)
	}
	for _, want := range []string{
		"Earlier fix rounds in this run (you are a new fixer session",
		"- Round 2: guard the nil map in Parse",
	} {
		if !strings.Contains(call.Prompt, want) {
			t.Errorf("fresh fixer prompt missing %q:\n%s", want, call.Prompt)
		}
	}
	if !strings.Contains(logs, "past the 5m0s prompt-cache window; starting a fresh fixer session") {
		t.Errorf("expected the cold restart to be logged, got:\n%s", logs)
	}
}

// TestReviewFixer_WarmSessionResumesWithoutASummary: inside the cache window
// the fixer resumes its session, which already holds the earlier rounds, so
// no summary is added.
func TestReviewFixer_WarmSessionResumesWithoutASummary(t *testing.T) {
	t.Parallel()
	call, logs := coldFixerHarness(t, time.Minute, "guard the nil map in Parse")

	if call.Session == nil || call.Session.ID != "sess-1" {
		t.Fatalf("a warm fixer session must be resumed, got %+v", call.Session)
	}
	if strings.Contains(call.Prompt, "Earlier fix rounds in this run") {
		t.Errorf("a resumed fixer must not get the fresh-session summary:\n%s", call.Prompt)
	}
	if strings.Contains(logs, "prompt-cache window") {
		t.Errorf("a warm session must not log a cold restart:\n%s", logs)
	}
}
