package pipeline

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const childHeartbeatText = "agent waiting on a process it launched"

// runQuietAgentForHeartbeat runs one agent turn against a recorded step and
// returns the step's last recorded activity. The agent talks once and then
// stays silent while script runs.
func runQuietAgentForHeartbeat(t *testing.T, script string) string {
	t.Helper()
	return runAgentForHeartbeat(t, func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return runLaunchedShell(ctx, opts, script, func(started, _ func()) {
			opts.OnChunk("running the suite\n")
			started()
		})
	})
}

func runAgentForHeartbeat(t *testing.T, runFn func(context.Context, agent.RunOpts) (*agent.Result, error)) string {
	t.Helper()
	database, run := sessionTestDB(t)
	sr, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.TouchStepActivity(sr.ID, "step started"); err != nil {
		t.Fatal(err)
	}
	ag := &hangingAgent{name: "suite-runner", runFn: runFn}
	// No still-working cap: the production default, where the stall budget
	// never sampled children, so only the heartbeat enables the sampler.
	sctx := &StepContext{
		Ctx:          context.Background(),
		Agent:        ag,
		Config:       &config.Config{AgentTimeout: time.Minute},
		DB:           database,
		StepResultID: sr.ID,
	}
	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "work"}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	got, err := database.GetStepResult(sr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastActivity == nil {
		return ""
	}
	return *got.LastActivity
}

// An agent that runs a long test suite through a tool emits nothing while the
// suite runs, and step liveness (`axi status`, the dead-run watchdog) reads
// only the step's activity row. A healthy suite must keep that row fresh, or
// the run reads as stalled and is eventually failed as dead.
func TestRunAgent_QuietAgentWaitingOnALiveChildHeartbeatsTheStep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the live-child probe reads the POSIX process table")
	}
	restore := agentChildHeartbeatInterval
	agentChildHeartbeatInterval = 300 * time.Millisecond
	t.Cleanup(func() { agentChildHeartbeatInterval = restore })

	got := runQuietAgentForHeartbeat(t, liveChildScript)
	if !strings.Contains(got, childHeartbeatText) {
		t.Fatalf("last activity = %q, want the child heartbeat", got)
	}
}

// A quiet agent with no child is not proven to be working, so it must not be
// kept looking alive: the watchdog stays the bound for a wedged agent.
func TestRunAgent_QuietAgentWithoutAChildDoesNotHeartbeat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the live-child probe reads the POSIX process table")
	}
	restore := agentChildHeartbeatInterval
	agentChildHeartbeatInterval = 300 * time.Millisecond
	t.Cleanup(func() { agentChildHeartbeatInterval = restore })

	got := runQuietAgentForHeartbeat(t, "read go; exec sleep 2.5")
	if strings.Contains(got, childHeartbeatText) {
		t.Fatalf("last activity = %q, a childless quiet agent must not heartbeat", got)
	}
}

// A helper alive for the whole turn (a stdio MCP server, an ACP agent under
// acpx) is not a tool call: an agent that talked after it started and then
// went quiet must not be kept looking alive by it.
func TestRunAgent_HelperStartedBeforeTheLastOutputDoesNotHeartbeat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the live-child probe reads the POSIX process table")
	}
	restore := agentChildHeartbeatInterval
	agentChildHeartbeatInterval = 300 * time.Millisecond
	t.Cleanup(func() { agentChildHeartbeatInterval = restore })

	got := runAgentForHeartbeat(t, func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		// Report the launch only once the helper runs, so the child sample
		// taken at launch already holds it (see
		// TestRunAgent_HelperStartedBeforeTheLastOutputDoesNotExtendTheBudget).
		report := opts.OnLifecycle
		var launch agent.LifecycleEvent
		held := opts
		held.OnLifecycle = func(event agent.LifecycleEvent) {
			if event.Phase == agent.LifecyclePhaseStart {
				launch = event
				return
			}
			report(event)
		}
		return runLaunchedShell(ctx, held, "sleep 2.5 & echo ready; read go; wait", func(started, ready func()) {
			ready()
			report(launch)
			time.Sleep(300 * time.Millisecond)
			opts.OnChunk("thinking\n")
			started()
		})
	})
	if strings.Contains(got, childHeartbeatText) {
		t.Fatalf("last activity = %q, a helper started before the last output must not heartbeat", got)
	}
}
