package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const zeroSHA = "0000000000000000000000000000000000000000"

// newWatchdogTestEnv builds paths, DB, and an in-process RunManager without
// starting a daemon, so tests can drive runs and sweep the watchdog directly.
func newWatchdogTestEnv(t *testing.T, sf StepFactory) (*paths.Paths, *db.DB, *RunManager) {
	t.Helper()
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	mockClaude := writeMockClaude(t, t.TempDir())
	configYAML := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	mgr := NewRunManager(d, p, sf)
	t.Cleanup(mgr.Shutdown)
	return p, d, mgr
}

func newTestWatchdog(p *paths.Paths, d *db.DB, mgr *RunManager, stallTimeout time.Duration, skew time.Duration) *runWatchdog {
	w := newRunWatchdog(d, p, mgr, stallTimeout)
	w.now = func() time.Time { return time.Now().Add(skew) }
	return w
}

// startBlockedRun pushes a run whose single step blocks until cancelled and
// waits for the step to start.
func startBlockedRun(t *testing.T, p *paths.Paths, d *db.DB, mgr *RunManager, repoID string, started chan struct{}) string {
	t.Helper()
	_, headSHA := setupTestGitRepo(t, p, d, repoID)
	runID, err := mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: p.RepoDir(repoID),
		Ref:  "refs/heads/main",
		Old:  zeroSHA,
		New:  headSHA,
	})
	if err != nil {
		t.Fatalf("push received: %v", err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("step did not start")
	}
	return runID
}

func TestWatchdogSweep_FailsStalledRunWithDeadReason(t *testing.T) {
	started := make(chan struct{})
	slow := &mockSlowStep{name: types.StepReview, started: started}
	p, d, mgr := newWatchdogTestEnv(t, func() []pipeline.Step { return []pipeline.Step{slow} })
	runID := startBlockedRun(t, p, d, mgr, "watchdog-stall-repo", started)

	// Two hours of silence against a one-hour stall timeout.
	w := newTestWatchdog(p, d, mgr, time.Hour, 2*time.Hour)
	w.sweep()

	run := waitForRunTerminalState(t, d, runID)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunFailed)
	}
	if run.Error == nil || !strings.HasPrefix(*run.Error, types.RunDeadReasonPrefix) {
		t.Fatalf("run error = %v, want %q prefix", run.Error, types.RunDeadReasonPrefix)
	}
	if !strings.Contains(*run.Error, "step_stall_timeout") {
		t.Fatalf("run error = %q, want stall-timeout attribution", *run.Error)
	}
}

func TestWatchdogSweep_StallTimeoutZeroDisablesInactivityKill(t *testing.T) {
	started := make(chan struct{})
	slow := &mockSlowStep{name: types.StepReview, started: started}
	p, d, mgr := newWatchdogTestEnv(t, func() []pipeline.Step { return []pipeline.Step{slow} })
	runID := startBlockedRun(t, p, d, mgr, "watchdog-disabled-repo", started)

	w := newTestWatchdog(p, d, mgr, 0, 48*time.Hour)
	w.sweep()

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunRunning)
	}
}

func TestWatchdogSweep_CIMonitorQuietIsExempt(t *testing.T) {
	started := make(chan struct{})
	slow := &mockSlowStep{name: types.StepCI, started: started}
	p, d, mgr := newWatchdogTestEnv(t, func() []pipeline.Step { return []pipeline.Step{slow} })
	runID := startBlockedRun(t, p, d, mgr, "watchdog-ci-repo", started)

	// A CI monitor may legitimately be silent for days (ci_timeout owns it).
	w := newTestWatchdog(p, d, mgr, time.Hour, 72*time.Hour)
	w.sweep()

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status = %q, want %q (CI must be exempt from the stall kill)", run.Status, types.RunRunning)
	}
}

func TestWatchdogSweep_ParkedAwaitingAgentRunIsExempt(t *testing.T) {
	p, d, mgr := newWatchdogTestEnv(t, func() []pipeline.Step {
		return []pipeline.Step{&mockApprovalStep{name: types.StepReview}}
	})
	_, headSHA := setupTestGitRepo(t, p, d, "watchdog-parked-repo")
	runID, err := mgr.HandlePushReceived(context.Background(), &ipc.PushReceivedParams{
		Gate: p.RepoDir("watchdog-parked-repo"),
		Ref:  "refs/heads/main",
		Old:  zeroSHA,
		New:  headSHA,
	})
	if err != nil {
		t.Fatalf("push received: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		if run != nil && run.AwaitingAgentSince != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run never parked awaiting agent")
		}
		time.Sleep(20 * time.Millisecond)
	}

	w := newTestWatchdog(p, d, mgr, time.Hour, 72*time.Hour)
	w.sweep()

	run, err := d.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunRunning {
		t.Fatalf("run status = %q, want %q (parked runs are agent-paced)", run.Status, types.RunRunning)
	}
}

func TestWatchdogSweep_FailsRunWhoseWorktreeWasRemoved(t *testing.T) {
	started := make(chan struct{})
	slow := &mockSlowStep{name: types.StepReview, started: started}
	p, d, mgr := newWatchdogTestEnv(t, func() []pipeline.Step { return []pipeline.Step{slow} })
	runID := startBlockedRun(t, p, d, mgr, "watchdog-worktree-repo", started)

	if err := os.RemoveAll(p.WorktreeDir("watchdog-worktree-repo", runID)); err != nil {
		t.Fatal(err)
	}

	// No clock skew: worktree loss is detected regardless of activity age.
	w := newTestWatchdog(p, d, mgr, time.Hour, 0)
	w.sweep()

	run := waitForRunTerminalState(t, d, runID)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunFailed)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "worktree no longer exists") {
		t.Fatalf("run error = %v, want missing-worktree reason", run.Error)
	}
}

func TestWatchdogSweep_FailsRunWhoseAgentProcessIsGone(t *testing.T) {
	started := make(chan struct{})
	slow := &mockSlowStep{name: types.StepReview, started: started}
	p, d, mgr := newWatchdogTestEnv(t, func() []pipeline.Step { return []pipeline.Step{slow} })
	runID := startBlockedRun(t, p, d, mgr, "watchdog-agent-repo", started)

	steps, err := d.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	stalePID := 999999
	if err := d.SetStepAgentActivity(steps[0].ID, "claude started", &stalePID); err != nil {
		t.Fatal(err)
	}

	// Quiet past the agent-gone grace but well inside the stall timeout, so
	// only the dead-agent evidence can be the trigger.
	w := newTestWatchdog(p, d, mgr, time.Hour, 5*time.Minute)
	w.processAlive = func(pid int) (bool, error) { return pid != stalePID, nil }
	w.sweep()

	run := waitForRunTerminalState(t, d, runID)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunFailed)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "agent process") {
		t.Fatalf("run error = %v, want dead-agent reason", run.Error)
	}
}

func TestWatchdogSweep_FinalizesUntrackedActiveRowAfterGrace(t *testing.T) {
	p, d, mgr := newWatchdogTestEnv(t, nil)
	if _, err := d.InsertRepoWithID("watchdog-orphan-repo", t.TempDir(), "https://example.invalid/repo.git", "main"); err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun("watchdog-orphan-repo", "main", "headsha", "basesha")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	// Fresh row: inside the grace, nothing happens (startRun setup window).
	w := newTestWatchdog(p, d, mgr, time.Hour, 0)
	w.sweep()
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning {
		t.Fatalf("run status = %q, want %q inside the orphan grace", got.Status, types.RunRunning)
	}

	// A row still inside startRun setup is owned, not orphaned: worktree
	// checkout plus the trusted default-branch fetch can legitimately outlast
	// the grace on a large repo, and failing the row would race the executor
	// registration that follows.
	mgr.beginRunSetup(run.ID)
	w = newTestWatchdog(p, d, mgr, time.Hour, 10*time.Minute)
	w.sweep()
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning {
		t.Fatalf("run status = %q, want %q while startRun setup owns the row", got.Status, types.RunRunning)
	}
	mgr.endRunSetup(run.ID)

	// Past the grace with no owner the row is finalized directly: there is no
	// executor goroutine left that could ever write a terminal state.
	w.sweep()
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("run status = %q, want %q", got.Status, types.RunFailed)
	}
	if got.Error == nil || !strings.HasPrefix(*got.Error, types.RunDeadReasonPrefix) {
		t.Fatalf("run error = %v, want %q prefix", got.Error, types.RunDeadReasonPrefix)
	}
}
