package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The dead-run watchdog is the daemon's only in-flight liveness authority.
// Startup recovery (recoverOnStartup) resolves runs orphaned by a daemon
// crash, but while the daemon itself stays alive nothing else re-examines a
// `running` run: a wedged agent subprocess, a worktree deleted out from under
// a run, or an executor whose terminal DB write was lost would previously
// leave the run `running` forever, indistinguishable from real progress
// (observed as a 12+ hour agent poll loop against a run that could never
// finish). The watchdog periodically sweeps active runs and declares a run
// dead - cancelling its executor with a cause the pipeline persists as the
// terminal `failed` error, prefixed with types.RunDeadReasonPrefix - when:
//
//   - the run's worktree directory no longer exists (the run cannot fix,
//     commit, or push anything ever again);
//   - the active step's recorded agent process is gone but the step never
//     observed the exit (stale agent_pid past a short grace);
//   - a running/fixing step other than CI has produced no log or agent
//     lifecycle activity for step_stall_timeout (the CI monitor is exempt:
//     its idle lifetime is owned by ci_timeout, and it deliberately
//     deduplicates unchanged poll logs, so long silences are legitimate);
//   - an active DB row has no live executor in this daemon at all (a lost
//     terminal write), past a startup grace.
//
// Runs parked at an approval/fix-review gate (awaiting_agent_since non-nil)
// are exempt: parking is agent-paced by design and already surfaced via
// `awaiting_agent` in axi status. step_quiet_warning stays display-only; this
// watchdog is the only actor.
const (
	watchdogInterval = time.Minute
	// watchdogOrphanRowGrace is how old (by updated_at) an active run row with
	// no live executor must be before the watchdog fails it directly. A run in
	// startRun setup is tracked separately (RunManager.settingUp) and never
	// counts as orphaned, however long checkout/fetch takes; the grace guards
	// the remaining races (a lost terminal write racing tracking removal).
	watchdogOrphanRowGrace = 5 * time.Minute
	// watchdogAgentGoneGrace is how long a step must additionally be quiet
	// before a recorded-but-dead agent_pid is treated as evidence of a wedged
	// step. On a normal exit the adapter observes the exit and clears the pid
	// within moments; the grace keeps the watchdog from racing that write.
	watchdogAgentGoneGrace = 2 * time.Minute
)

type runWatchdog struct {
	db           *db.DB
	paths        *paths.Paths
	mgr          *RunManager
	stallTimeout time.Duration
	interval     time.Duration
	now          func() time.Time
	processAlive func(pid int) (bool, error)
}

func newRunWatchdog(d *db.DB, p *paths.Paths, mgr *RunManager, stallTimeout time.Duration) *runWatchdog {
	return &runWatchdog{
		db:           d,
		paths:        p,
		mgr:          mgr,
		stallTimeout: stallTimeout,
		interval:     watchdogInterval,
		now:          time.Now,
		processAlive: processRunning,
	}
}

// run sweeps until ctx is cancelled. It is started as a daemon-lifetime
// goroutine after startup recovery completes, so every run it ever sees was
// either started or deliberately preserved by this daemon.
func (w *runWatchdog) run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.sweep()
		}
	}
}

// sweep inspects every active run once and declares dead runs dead.
func (w *runWatchdog) sweep() {
	runs, err := w.db.GetActiveRuns()
	if err != nil {
		slog.Warn("dead-run watchdog: list active runs failed", "error", err)
		return
	}
	for _, run := range runs {
		w.inspect(run)
	}
}

func (w *runWatchdog) inspect(run *db.Run) {
	if run.AwaitingAgentSince != nil {
		return // parked at a gate: agent-paced by design, never a watchdog target
	}
	now := w.now()

	if !w.mgr.tracksRun(run.ID) {
		// Active row with no live executor in this daemon. Normal briefly
		// during startRun setup; past the grace it is a dead row that no
		// goroutine will ever finalize.
		if now.Unix()-run.UpdatedAt < int64(watchdogOrphanRowGrace/time.Second) {
			return
		}
		w.failUntrackedRun(run, "no live pipeline executor exists for this run")
		return
	}

	// Worktree existence: startRun creates the worktree before the executor is
	// registered, so a tracked running run whose worktree directory is gone
	// has lost it out-of-band and can never complete its remaining steps.
	if run.Status == types.RunRunning {
		wtDir := w.paths.WorktreeDir(run.RepoID, run.ID)
		if _, err := os.Stat(wtDir); err != nil && os.IsNotExist(err) {
			w.declareDead(run, fmt.Sprintf("run worktree no longer exists at %s", wtDir))
			return
		}
	}

	steps, err := w.db.GetStepsByRun(run.ID)
	if err != nil {
		slog.Warn("dead-run watchdog: load steps failed", "run_id", run.ID, "error", err)
		return
	}
	active := activeWatchdogStep(steps)
	if active == nil {
		return
	}
	quietFor := time.Duration(now.Unix()-lastStepActivityUnix(run, active)) * time.Second
	if quietFor < 0 {
		quietFor = 0
	}

	// A recorded agent pid whose process is gone, still unobserved after the
	// grace, means the step will never see its agent finish. PID reuse fails
	// safe: a recycled pid looks alive and defers to the stall timeout.
	if active.AgentPID != nil && *active.AgentPID > 0 && quietFor >= watchdogAgentGoneGrace {
		if alive, aliveErr := w.processAlive(*active.AgentPID); aliveErr == nil && !alive {
			w.declareDead(run, fmt.Sprintf("step %s agent process %d is gone and its exit was never observed", active.StepName, *active.AgentPID))
			return
		}
	}

	if w.stallTimeout > 0 && active.StepName != types.StepCI && quietFor >= w.stallTimeout {
		w.declareDead(run, fmt.Sprintf("step %s has had no activity for %s (step_stall_timeout %s)", active.StepName, quietFor.Truncate(time.Second), w.stallTimeout))
	}
}

// declareDead cancels the run's executor with a dead-run cause. The executor's
// normal failure path persists the cause as the terminal `failed` error, kills
// the agent process tree, closes subscribers, and removes the worktree.
func (w *runWatchdog) declareDead(run *db.Run, detail string) {
	reason := types.RunDeadReasonPrefix + detail
	if w.mgr.cancelRunWithCause(run.ID, errors.New(reason)) {
		slog.Warn("dead-run watchdog: declared run dead", "run_id", run.ID, "reason", detail)
	}
}

// failUntrackedRun finalizes an active DB row that has no executor goroutine
// left to finalize it. There is no context to cancel, so it writes the
// terminal state directly and closes any lingering subscribers.
func (w *runWatchdog) failUntrackedRun(run *db.Run, detail string) {
	reason := types.RunDeadReasonPrefix + detail
	if err := w.db.UpdateRunErrorStatus(run.ID, reason, types.RunFailed); err != nil {
		slog.Error("dead-run watchdog: failed to finalize untracked run", "run_id", run.ID, "error", err)
		return
	}
	slog.Warn("dead-run watchdog: finalized untracked run", "run_id", run.ID, "reason", detail)
	status := string(types.RunFailed)
	w.mgr.broadcast(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		RepoID: run.RepoID,
		Status: &status,
		Branch: &run.Branch,
		Error:  &reason,
	})
	w.mgr.closeSubscribers(run.ID)
}

// activeWatchdogStep returns the step currently doing work, if any. Steps at
// an approval gate are excluded via the run-level awaiting_agent exemption.
func activeWatchdogStep(steps []*db.StepResult) *db.StepResult {
	for _, step := range steps {
		if step.Status == types.StepStatusRunning || step.Status == types.StepStatusFixing {
			return step
		}
	}
	return nil
}

// lastStepActivityUnix is the newest liveness evidence for the active step:
// its last activity, else its start time, else the run row's own update time.
func lastStepActivityUnix(run *db.Run, step *db.StepResult) int64 {
	newest := run.UpdatedAt
	if step.StartedAt != nil && *step.StartedAt > newest {
		newest = *step.StartedAt
	}
	if step.LastActivityAt != nil && *step.LastActivityAt > newest {
		newest = *step.LastActivityAt
	}
	return newest
}
