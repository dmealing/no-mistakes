package daemon

import (
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func (f *retryFixture) automaticRetry(t *testing.T, params ipc.RetryRunParams) (ipc.RetryRunResult, error) {
	t.Helper()
	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	params.RepoID, params.Branch, params.Automatic = f.repo.ID, "main", true
	if params.CallerHeadSHA == "" {
		params.CallerHeadSHA = f.callerHead
	}
	var result ipc.RetryRunResult
	err = client.Call(ipc.MethodRetryRun, &params, &result)
	return result, err
}

// failTestOnBaseFetch rewrites the source's failed Test row to the cause a
// base-branch fetch that failed after its retries records.
func (f *retryFixture) failTestOnBaseFetch(t *testing.T) {
	t.Helper()
	row := f.stepRow(t, types.StepTest)
	if err := f.d.FailStepWithCause(row.ID, `fetch default branch "main" to resolve branch base: signal: killed`, 1, db.StepFailureCauseBaseFetch); err != nil {
		t.Fatal(err)
	}
}

// TestAutomaticRetryContinuesAFailedTestAndReusesTheReview: a fresh `axi run`
// for the same request on the same head, after a run failed in Test on the
// base-branch fetch, continues that run from Test. The finished Review is
// carried, not paid for again: only Test executes.
func TestAutomaticRetryContinuesAFailedTestAndReusesTheReview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	f := newRetryFixture(t, retryFixtureOptions{failInTest: true, beforeSource: func(t *testing.T, f *retryFixture) {
		writeFakeAgents(t, f.p, "claude", "agent_config:\n  claude:\n    model: opus\n", "claude")
	}})
	f.failTestOnBaseFetch(t)

	result, err := f.automaticRetry(t, ipc.RetryRunParams{Intent: "keep it"})
	if err != nil {
		t.Fatalf("automatic retry: %v", err)
	}
	if result.SourceRunID != f.source.ID || result.Step != types.StepTest {
		t.Fatalf("result = %+v, want Test of %s continued", result, f.source.ID)
	}
	if run := waitForRunTerminalState(t, f.d, result.RunID); run.Status != types.RunCompleted {
		t.Fatalf("continuation = %s, want completed", run.Status)
	}
	if got := f.executions.Load(); got != 1 {
		t.Fatalf("executed %d steps, want Test only (Rebase and Review carried)", got)
	}
	rows, err := f.d.GetStepsByRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == types.StepReview && row.CarriedFrom == nil {
			t.Fatalf("the continuation's Review was not carried: %+v", row)
		}
	}
}

// TestAutomaticRetryRefusesUnlessTheRequestAndReviewMatch: the automatic
// continuation is only a cheaper way to give a fresh request the run it would
// have got, so a different request, a run with no finished review, or a
// different head starts over instead, and a refusal launches nothing.
func TestAutomaticRetryRefusesUnlessTheRequestAndReviewMatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the agent stand-ins are POSIX shell scripts")
	}
	for _, tc := range []struct {
		name       string
		failInTest bool
		params     ipc.RetryRunParams
		want       string
	}{
		{name: "corrected intent", failInTest: true, params: ipc.RetryRunParams{Intent: "keep it, and more"}, want: "the intent differs"},
		{name: "different base branch", failInTest: true, params: ipc.RetryRunParams{Intent: "keep it", PRBaseBranch: "release"}, want: "PR base branch differs"},
		{name: "different intent publication", failInTest: true, params: ipc.RetryRunParams{Intent: "keep it", OmitIntent: true}, want: "intent publication differs"},
		{name: "added closing reference", failInTest: true, params: ipc.RetryRunParams{Intent: "keep it", ClosingIssueRefs: []string{"#42"}}, want: "closing references differ"},
		{name: "review never finished", params: ipc.RetryRunParams{Intent: "keep it"}, want: "no review to reuse"},
		{name: "different head", failInTest: true, params: ipc.RetryRunParams{Intent: "keep it", CallerHeadSHA: strings.Repeat("1", 40)}, want: "matching head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetryFixture(t, retryFixtureOptions{failInTest: tc.failInTest, beforeSource: func(t *testing.T, f *retryFixture) {
				writeFakeAgents(t, f.p, "claude", "agent_config:\n  claude:\n    model: opus\n", "claude")
			}})
			_, err := f.automaticRetry(t, tc.params)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a refusal containing %q", err, tc.want)
			}
			f.requireNothingLaunched(t)
		})
	}
}

// TestManualRetryStillRequiresTheRunID: the automatic selection of the
// branch's latest run is only for the automatic request; a manual retry with
// no run named is refused as before.
func TestManualRetryStillRequiresTheRunID(t *testing.T) {
	f := newRetryFixture(t, retryFixtureOptions{failInTest: true})
	if _, err := f.retry(t, ""); err == nil {
		t.Fatal("a manual retry naming no run must be refused")
	}
	f.requireNothingLaunched(t)
}
