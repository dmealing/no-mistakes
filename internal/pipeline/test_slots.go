package pipeline

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/testslots"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// executeWithTestSlot runs one Execute of step. The Test step takes a
// host-wide test slot first (config test_slots), so only a bounded number of
// runs across all repos do heavy test work at once. A run without a slot
// waits and never fails for lack of one.
//
// The step's own clocks must not count the wait: phaseStart, which feeds the
// recorded step duration, moves forward by the time waited, and agent
// invocation timeouts start inside Execute, after acquisition. While queued
// the step heartbeats an activity line naming the holders, so the watchdog's
// stall clock stays quiet and axi status shows the run as queued, not wedged.
// The slot is released when Execute returns on any path.
func (e *Executor) executeWithTestSlot(step Step, sctx *StepContext, stepResultID string, run *db.Run, log func(string), phaseStart *time.Time) (*StepOutcome, error) {
	if step.Name() != types.StepTest || e.config == nil || e.config.TestSlots <= 0 || e.paths == nil {
		return step.Execute(sctx)
	}
	pool := testslots.NewPool(e.paths.TestSlotsDir(), e.config.TestSlots)
	lastHolders := ""
	lease, waited, err := pool.Acquire(sctx.Ctx, fmt.Sprintf("run %s (%s)", run.ID, run.Branch), func(holders []testslots.Holder) {
		msg := fmt.Sprintf("queued for a test slot (test_slots=%d); held by %s", e.config.TestSlots, testslots.DescribeHolders(holders))
		var key string
		for _, h := range holders {
			key += fmt.Sprintf("%d/%s;", h.PID, h.Label)
		}
		if key != lastHolders {
			lastHolders = key
			log(msg)
			return
		}
		if dbErr := e.db.TouchStepActivity(stepResultID, msg); dbErr != nil {
			slog.Warn("failed to touch step activity in db", "step", step.Name(), "error", dbErr)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("wait for test slot: %w", err)
	}
	defer lease.Release()
	if waited > 0 {
		*phaseStart = phaseStart.Add(waited)
		log(fmt.Sprintf("acquired a test slot after waiting %s", waited.Truncate(time.Second)))
	}
	return step.Execute(sctx)
}
