package steps

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newHeartbeatStepContext(t *testing.T) (*pipeline.StepContext, *db.DB, string) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.InsertRepoWithID("hb-repo", t.TempDir(), "https://example.invalid/repo.git", "main"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun("hb-repo", "feature/hb", "headsha", "basesha")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatalf("start step: %v", err)
	}
	sctx := &pipeline.StepContext{
		Ctx:          context.Background(),
		WorkDir:      t.TempDir(),
		DB:           database,
		StepResultID: step.ID,
	}
	return sctx, database, step.ID
}

func waitForHeartbeatActivity(t *testing.T, database *db.DB, stepID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		step, err := database.GetStepResult(stepID)
		if err != nil {
			t.Fatalf("get step: %v", err)
		}
		if step.LastActivity != nil && strings.Contains(*step.LastActivity, "waiting on configured command") {
			return *step.LastActivity
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("configured-command heartbeat never touched step activity")
	return ""
}

// A configured commands.* invocation captures buffered output, so the daemon
// sees zero step activity for the command's whole runtime. The heartbeat is
// what keeps a legitimately long (1h+) test suite from being stall-killed by
// the dead-run watchdog: while the daemon is synchronously waiting on the live
// command it launched, that IS activity, and it must be recorded as such.
func TestRunStepShellCommand_HeartbeatsStepActivityWhileCommandRuns(t *testing.T) {
	sctx, database, stepID := newHeartbeatStepContext(t)
	restore := configuredCommandHeartbeatInterval
	configuredCommandHeartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { configuredCommandHeartbeatInterval = restore })

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, _, err := runStepShellCommand(sctx, "sleep 1"); err != nil {
			t.Errorf("run command: %v", err)
		}
	}()

	activity := waitForHeartbeatActivity(t, database, stepID)
	if !strings.Contains(activity, "sleep 1") {
		t.Fatalf("heartbeat should name the command it is waiting on, got %q", activity)
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("command did not finish")
	}
}

// The heartbeat must stop with the command: a finished step may sit at a gate
// for hours, and a heartbeat that outlived its command would mask a genuinely
// wedged later round from the watchdog.
func TestConfiguredCommandHeartbeat_StopsWhenCommandFinishes(t *testing.T) {
	sctx, database, stepID := newHeartbeatStepContext(t)

	stop := startConfiguredCommandHeartbeat(sctx, "true", 20*time.Millisecond)
	waitForHeartbeatActivity(t, database, stepID)
	stop()
	stop() // idempotent

	if err := database.TouchStepActivity(stepID, "post-command marker"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	step, err := database.GetStepResult(stepID)
	if err != nil {
		t.Fatalf("get step: %v", err)
	}
	if step.LastActivity == nil || *step.LastActivity != "post-command marker" {
		t.Fatalf("heartbeat kept writing after stop: %v", step.LastActivity)
	}
}

// A nil DB or missing step id (some callers run outside a recorded step) must
// not heartbeat or panic.
func TestConfiguredCommandHeartbeat_NoOpWithoutStepIdentity(t *testing.T) {
	stop := startConfiguredCommandHeartbeat(&pipeline.StepContext{}, "true", time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	stop()
}
