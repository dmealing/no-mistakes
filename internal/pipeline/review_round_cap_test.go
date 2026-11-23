package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	roundCapWarning = `{"findings":[{"id":"R1","severity":"warning","file":"main.go","description":"could be tighter","action":"auto-fix"}]}`
	roundCapError   = `{"findings":[{"id":"R1","severity":"error","file":"main.go","description":"still broken","action":"auto-fix"}]}`
	roundCapAskUser = `{"findings":[{"id":"R1","severity":"warning","file":"main.go","description":"is this wanted?","action":"ask-user"}]}`
)

// roundCapStep returns the same findings every round, with the coverage
// verdict given, and counts how many rounds ran.
func roundCapStep(findings string, coverageComplete bool, calls *int) *adaptiveCallStep {
	return &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		*calls++
		return &StepOutcome{
			NeedsApproval:          true,
			AutoFixable:            true,
			Findings:               findings,
			ReviewedPaths:          []string{"main.go"},
			ReviewablePaths:        []string{"main.go"},
			ReviewCoverageComplete: coverageComplete,
		}, nil
	}}
}

func TestReviewRoundCapReached(t *testing.T) {
	for _, tc := range []struct {
		roundCap, fixRounds int
		want                bool
	}{
		{0, 0, false}, {0, 9, false},
		{1, 0, true},
		{2, 0, false}, {2, 1, true}, {2, 2, true},
		{3, 1, false}, {3, 2, true},
	} {
		if got := reviewRoundCapReached(tc.roundCap, tc.fixRounds); got != tc.want {
			t.Errorf("reviewRoundCapReached(%d, %d) = %v, want %v", tc.roundCap, tc.fixRounds, got, tc.want)
		}
	}
}

// At the default cap of 2 a warning that survives the one fix round is left
// as a note: the step completes after review, fix, re-check, and the notice
// records the remaining finding on the step result.
func TestExecutor_ReviewRoundCapCompletesWithNotes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		roundCap  int
		wantCalls int
	}{
		{"default cap of two", config.DefaultReviewRoundCap, 2},
		{"cap of one spends no fix round", 1, 1},
		{"cap of three", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			cfg := &config.Config{Review: config.Review{RoundCap: tc.roundCap}, AutoFix: config.AutoFix{Review: 5}}
			calls := 0
			exec := NewExecutor(database, p, cfg, nil, []Step{roundCapStep(roundCapWarning, true, &calls)}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := exec.Execute(ctx, run, repo, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("review rounds = %d, want %d", calls, tc.wantCalls)
			}
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if steps[0].Status != types.StepStatusCompleted || steps[0].FindingsJSON == nil {
				t.Fatalf("capped review should complete with its notes recorded: %+v", steps[0])
			}
			findings, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
			if err != nil {
				t.Fatal(err)
			}
			var sawWarning, sawNotice bool
			for _, item := range findings.Items {
				switch item.ID {
				case types.FindingIDReviewRoundCapNotes:
					sawNotice = item.Severity == types.FindingSeverityInfo && strings.Contains(item.Description, "1 remaining finding")
				case "R1":
					sawWarning = true
				}
			}
			if !sawWarning || !sawNotice {
				t.Fatalf("step findings should keep the warning and carry the cap notice: %s", *steps[0].FindingsJSON)
			}
			rounds, err := database.GetRoundsByStep(steps[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if last := rounds[len(rounds)-1]; last.FindingsJSON == nil || !strings.Contains(*last.FindingsJSON, types.FindingIDReviewRoundCapNotes) {
				t.Fatalf("the final round should record the cap notice for the PR body: %+v", last)
			}
		})
	}
}

// The cap never silently passes an error, an ask-user finding, or an
// unverified file: at the cap no further fix round starts and the round parks
// at the existing gate without the notes notice.
func TestExecutor_ReviewRoundCapParksWhatItCannotNote(t *testing.T) {
	for _, tc := range []struct {
		name             string
		findings         string
		coverageComplete bool
		wantStatus       types.StepStatus
	}{
		{"error finding", roundCapError, true, types.StepStatusFixReview},
		{"ask-user finding", roundCapAskUser, true, types.StepStatusAwaitingApproval},
		{"incomplete coverage", roundCapWarning, false, types.StepStatusFixReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			cfg := &config.Config{Review: config.Review{RoundCap: 2}, AutoFix: config.AutoFix{Review: 5}}
			calls := 0
			parks := make(chan string, 4)
			exec := NewExecutor(database, p, cfg, nil, []Step{roundCapStep(tc.findings, tc.coverageComplete, &calls)}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(tc.wantStatus) {
					parks <- *event.Findings
				}
			})
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())
			raw := waitReviewBudgetPark(t, parks, done)
			if strings.Contains(raw, types.FindingIDReviewRoundCapNotes) {
				t.Fatalf("a parked round must not carry the notes notice: %s", raw)
			}
			wantCalls := 2
			if tc.name == "ask-user finding" {
				// ask-user findings never start an automatic fix round.
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("review rounds before the park = %d, want %d", calls, wantCalls)
			}
			if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
				t.Fatal(err)
			}
			waitExecutorDone(t, done)
			if calls != wantCalls {
				t.Fatalf("approval should not run another round: %d", calls)
			}
		})
	}
}

// review.round_cap: 0 turns the cap off: the automatic loop runs to
// auto_fix.review as before and parks on the remaining warning.
func TestExecutor_ReviewRoundCapOff(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{Review: config.Review{RoundCap: 0}, AutoFix: config.AutoFix{Review: 3}}
	calls := 0
	parks := make(chan string, 4)
	exec := NewExecutor(database, p, cfg, nil, []Step{roundCapStep(roundCapWarning, true, &calls)}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil && *event.Status == string(types.StepStatusFixReview) {
			parks <- *event.Findings
		}
	})
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	raw := waitReviewBudgetPark(t, parks, done)
	if calls != 4 || strings.Contains(raw, types.FindingIDReviewRoundCapNotes) {
		t.Fatalf("with the cap off the loop should spend all 3 fix rounds and park: calls=%d findings=%s", calls, raw)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitExecutorDone(t, done)
}
