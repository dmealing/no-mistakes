package pipeline

import (
	"context"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ErrAgentInvocationFailed marks an error an agent invocation itself
// returned while its context was still live: the provider, the transport, or
// the agent process failed. A timeout, a cancellation, and anything a step
// concluded from a successful invocation's output never carry it.
var ErrAgentInvocationFailed = errors.New("agent invocation failed")

// BaseFetchError marks a step that failed because fetching the run's base
// branch failed after its retries, before the step judged anything. Like an
// agent invocation failure it says nothing about the change, so a run that
// failed on it may continue from the failed step.
type BaseFetchError struct{ Err error }

func (e *BaseFetchError) Error() string { return e.Err.Error() }

func (e *BaseFetchError) Unwrap() error { return e.Err }

type agentInvocationFailure struct{ err error }

func (f *agentInvocationFailure) Error() string { return f.err.Error() }

func (f *agentInvocationFailure) Unwrap() error { return f.err }

func (f *agentInvocationFailure) Is(target error) bool { return target == ErrAgentInvocationFailed }

// invocationFailureAgent is the innermost step-agent decorator, so it sees the
// adapter's own error under the invocation's own context: a deadline or a
// cancellation has already ended that context by the time its error returns,
// which is how both stay unmarked.
type invocationFailureAgent struct {
	inner agent.Agent
}

func (a *invocationFailureAgent) Name() string { return a.inner.Name() }

func (a *invocationFailureAgent) Close() error { return a.inner.Close() }

func (a *invocationFailureAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	result, err := a.inner.Run(ctx, opts)
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}
	return result, &agentInvocationFailure{err: err}
}

func (a *invocationFailureAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *invocationFailureAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *invocationFailureAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *invocationFailureAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

// RetryCheckpoint is the boundary a failed run continues from: the step at
// Index restarts from the beginning, and the steps before it are carried. That
// step is the failed one, or Test when the run failed after Test.
type RetryCheckpoint struct {
	// Index is the position of the step the continuation restarts.
	Index int
	// FailedIndex is the failed step's position in the pipeline.
	FailedIndex int
	// Steps holds every step record of the failed run, in pipeline order.
	Steps []*db.StepResult
	// Round is the failed step's round that was in flight when it failed.
	Round int
}

// Failed is the failed step's record.
func (c *RetryCheckpoint) Failed() *db.StepResult { return c.Steps[c.FailedIndex] }

// Retry proofs PlanRetryCheckpoint can fail.
const (
	RetryProofCheckpoint = "durable checkpoint"
	RetryProofCause      = "eligible failure"
)

// RetryRefusal is a failed retry proof: which one, and why.
type RetryRefusal struct {
	Proof  string
	Reason string
}

func (r *RetryRefusal) Error() string { return r.Reason }

func refuseCheckpoint(proof, format string, args ...any) error {
	return &RetryRefusal{Proof: proof, Reason: fmt.Sprintf(format, args...)}
}

// PlanRetryCheckpoint proves that run's record names exactly one safe
// boundary to continue from, against the step sequence steps, and refuses
// with a *RetryRefusal naming the first proof that fails. The record must hold one row per step in
// that order: completed or skipped rows, then exactly one failed row whose
// recorded cause is an agent invocation failure or a base-branch fetch
// failure, then rows that never ran.
// No carried row may be Test or a later step, because Test's evidence and
// every later step's work are not kept after a run ends: a run that failed
// after Test restarts from Test.
func PlanRetryCheckpoint(database *db.DB, run *db.Run, steps []Step) (*RetryCheckpoint, error) {
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		return nil, fmt.Errorf("read the run's step records: %w", err)
	}
	if len(rows) != len(steps) {
		return nil, refuseCheckpoint(RetryProofCheckpoint, "the run recorded %d steps but its pipeline has %d", len(rows), len(steps))
	}
	failed := -1
	for index, row := range rows {
		if row.StepName != steps[index].Name() {
			return nil, refuseCheckpoint(RetryProofCheckpoint, "the run's step %d is %q, but the pipeline's is %q", index, row.StepName, steps[index].Name())
		}
		switch {
		case row.Status == types.StepStatusFailed:
			if failed >= 0 {
				return nil, refuseCheckpoint(RetryProofCheckpoint, "steps %s and %s both failed", rows[failed].StepName, row.StepName)
			}
			failed = index
		case failed < 0 && row.Status != types.StepStatusCompleted && row.Status != types.StepStatusSkipped:
			return nil, refuseCheckpoint(RetryProofCheckpoint, "step %s is %s before the failed step", row.StepName, row.Status)
		case failed >= 0 && row.Status != types.StepStatusPending && row.Status != types.StepStatusSkipped:
			return nil, refuseCheckpoint(RetryProofCheckpoint, "step %s is %s after the failed step", row.StepName, row.Status)
		}
	}
	if failed < 0 {
		return nil, refuseCheckpoint(RetryProofCheckpoint, "no step failed")
	}
	row := rows[failed]
	if row.FailureCause == nil || (*row.FailureCause != db.StepFailureCauseAgentInvocation && *row.FailureCause != db.StepFailureCauseBaseFetch) {
		return nil, refuseCheckpoint(RetryProofCause, "step %s did not fail on an agent invocation error (provider, transport, or agent process) or a base-branch fetch, so the failure may be about the change itself", row.StepName)
	}
	restart := failed
	for index, carried := range rows[:failed] {
		if carried.StepName.Order() >= types.StepTest.Order() {
			restart = index
			break
		}
	}
	rounds, err := database.GetRoundsByStep(row.ID)
	if err != nil {
		return nil, fmt.Errorf("read step %s rounds: %w", row.StepName, err)
	}
	return &RetryCheckpoint{Index: restart, FailedIndex: failed, Steps: rows, Round: len(rounds) + 1}, nil
}
