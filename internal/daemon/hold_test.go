package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/triage"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A hold refuses every new run at the daemon, so a plain push (which never
// passes through `axi run`'s check) cannot start one during an install; the
// run already in flight is not touched, and removing the hold lets runs start
// again.
func TestHoldRefusesNewRunsAndLeavesRunsInFlight(t *testing.T) {
	started := make(chan struct{})
	slow := &mockSlowStep{name: types.StepReview, started: started}
	p, d, mgr := newWatchdogTestEnv(t, func() []pipeline.Step { return []pipeline.Step{slow} })
	inFlight := startBlockedRun(t, p, d, mgr, "hold-repo-a", started)

	if err := os.WriteFile(triage.HoldPath(p.Root()), []byte("who: installer\nwhy: install a new build\nuntil: the install finishes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, headSHA := setupTestGitRepo(t, p, d, "hold-repo-b")
	push := &ipc.PushReceivedParams{Gate: p.RepoDir("hold-repo-b"), Ref: "refs/heads/main", Old: zeroSHA, New: headSHA}
	_, err := mgr.HandlePushReceived(context.Background(), push)
	if err == nil || !strings.Contains(err.Error(), "on hold") || !strings.Contains(err.Error(), "install a new build") {
		t.Fatalf("push under a hold: err = %v, want a hold refusal naming the reason", err)
	}
	runs, err := d.GetRunsByRepo("hold-repo-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("a run was created under the hold: %+v", runs[0])
	}
	if run, err := d.GetRun(inFlight); err != nil || run.Status != types.RunRunning {
		t.Fatalf("the run in flight changed under the hold: %+v, %v", run, err)
	}

	if err := os.Remove(triage.HoldPath(p.Root())); err != nil {
		t.Fatal(err)
	}
	slow.started = make(chan struct{})
	runID, err := mgr.HandlePushReceived(context.Background(), push)
	if err != nil || runID == "" {
		t.Fatalf("push after the hold was removed: %q, %v", runID, err)
	}
	select {
	case <-slow.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run after the hold did not start")
	}
}
