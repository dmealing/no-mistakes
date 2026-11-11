package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// continueFixture serves a fake daemon whose retry method answers with
// answer. It records each retry request, and counts the fresh-run launches
// that began after one: a fresh launch starts by listing the runs already
// recorded for the head it is about to push.
func continueFixture(t *testing.T, answer func(ipc.RetryRunParams) (interface{}, error)) (*[]ipc.RetryRunParams, func() int) {
	t.Helper()
	var mu sync.Mutex
	var requests []ipc.RetryRunParams
	freshAfterRetry := 0
	olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil }, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodRetryRun, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			var p ipc.RetryRunParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			mu.Lock()
			requests = append(requests, p)
			mu.Unlock()
			return answer(p)
		})
		srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
			mu.Lock()
			freshAfterRetry++
			mu.Unlock()
			return &ipc.GetRunsResult{}, nil
		})
	})
	return &requests, func() int {
		mu.Lock()
		defer mu.Unlock()
		return freshAfterRetry
	}
}

func runAxiRunForContinueTest(t *testing.T, skip []types.StepName, intent, base string, omit bool) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	_ = runAxiRunWithLaunchProof(cmd, false, skip, intent, base, omit, "", "", 2*time.Second)
	return out.String()
}

// TestAxiRunContinuesTheBranchsFailedRunInsteadOfStartingOver: a fresh `axi
// run` first asks the daemon to continue the branch's latest run with exactly
// this request, and when the daemon accepts, no fresh run is launched - the
// continuation, which carries the finished review, is the run it follows.
func TestAxiRunContinuesTheBranchsFailedRunInsteadOfStartingOver(t *testing.T) {
	requests, fresh := continueFixture(t, func(ipc.RetryRunParams) (interface{}, error) {
		return &ipc.RetryRunResult{RunID: "run-continued", SourceRunID: "run-failed", Step: types.StepTest, SourceRound: 1}, nil
	})

	runAxiRunForContinueTest(t, nil, "ship the widget", "", true)

	if len(*requests) != 1 {
		t.Fatalf("expected one continuation request, got %d", len(*requests))
	}
	got := (*requests)[0]
	if !got.Automatic || got.RunID != "" || got.Intent != "ship the widget" || !got.OmitIntent || got.CallerHeadSHA == "" {
		t.Fatalf("continuation request = %+v, want automatic, no run named, the request's intent and publication, and the clean caller head", got)
	}
	if n := fresh(); n != 0 {
		t.Fatalf("an accepted continuation must not also launch a fresh run (%d launches began)", n)
	}
}

// TestAxiRunStartsFreshWhenTheContinuationIsRefused: a refusal (or a daemon
// that predates the request) changes nothing about today's behavior - the
// fresh run is launched.
func TestAxiRunStartsFreshWhenTheContinuationIsRefused(t *testing.T) {
	requests, fresh := continueFixture(t, func(ipc.RetryRunParams) (interface{}, error) {
		return nil, errors.New("refusing retry of run run-failed: same request proof failed: the intent differs from the failed run's")
	})

	runAxiRunForContinueTest(t, nil, "ship the widget", "", false)

	if len(*requests) != 1 {
		t.Fatalf("expected one continuation request, got %d", len(*requests))
	}
	if fresh() == 0 {
		t.Fatal("a refused continuation must fall back to a fresh run")
	}
}

// TestAxiRunWithSkipNeverContinues: a continuation would not honor --skip, so
// a request that skips steps always starts fresh.
func TestAxiRunWithSkipNeverContinues(t *testing.T) {
	requests, fresh := continueFixture(t, func(ipc.RetryRunParams) (interface{}, error) {
		return &ipc.RetryRunResult{RunID: "run-continued"}, nil
	})

	runAxiRunForContinueTest(t, []types.StepName{types.StepLint}, "ship the widget", "", false)

	if len(*requests) != 0 {
		t.Fatalf("a --skip request must not ask to continue, got %+v", *requests)
	}
	if fresh() == 0 {
		t.Fatal("a --skip request must start a fresh run")
	}
}
