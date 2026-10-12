package pipeline

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/testslots"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// executeWithTestSlot runs one Execute of step. The Test step takes a
// host-wide test slot first (config test_slots), so only a bounded number of
// runs across all repos do heavy test work at once. A run without a slot
// waits and never fails for lack of one.
//
// The step's own clocks must not count the wait: the returned wait duration
// is how far the caller advances its phase clock, and agent invocation
// timeouts start inside Execute, after acquisition. While queued the step
// heartbeats an activity line naming the holders, so the watchdog's stall
// clock stays quiet and axi status shows the run as queued, not wedged. The
// slot is released when Execute returns on any path.
func (e *Executor) executeWithTestSlot(step Step, sctx *StepContext) (*StepOutcome, time.Duration, error) {
	if step.Name() != types.StepTest || e.config == nil || e.config.TestSlots <= 0 || e.paths == nil {
		outcome, err := step.Execute(sctx)
		return outcome, 0, err
	}
	pool := testslots.NewPool(e.paths.TestSlotsDir(), e.config.TestSlots)
	lease, waited, err := pool.Acquire(sctx.Ctx, fmt.Sprintf("run %s (%s)", sctx.Run.ID, sctx.Run.Branch), func(holders []testslots.Holder, changed bool) {
		msg := fmt.Sprintf("queued for a test slot (test_slots=%d); held by %s", e.config.TestSlots, testslots.DescribeHolders(holders))
		if changed {
			sctx.Log(msg)
			return
		}
		if dbErr := e.db.TouchStepActivity(sctx.StepResultID, msg); dbErr != nil {
			slog.Warn("failed to touch step activity in db", "step", step.Name(), "error", dbErr)
		}
	})
	if err != nil {
		return nil, 0, fmt.Errorf("wait for test slot: %w", err)
	}
	defer lease.Release()
	if waited > 0 {
		sctx.Log(fmt.Sprintf("acquired a test slot after waiting %s", waited.Truncate(time.Second)))
	}
	outcome, err := step.Execute(sctx)
	return outcome, waited, err
}
