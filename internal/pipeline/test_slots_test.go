package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/testslots"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_TestStepQueuesForSlotAndDoesNotCountWait(t *testing.T) {
	database, p, run, repo := setupTest(t)
	cfg := &config.Config{TestSlots: 1}
	pool := testslots.NewPool()
	held, _, err := pool.Acquire(context.Background(), 1, "run other (feature)", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	step := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	exec.SetTestSlotPool(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()

	// While queued: the step has not executed and the record says why.
	var activity string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		steps, _ := database.GetStepsByRun(run.ID)
		if len(steps) == 1 && steps[0].LastActivity != nil && strings.Contains(*steps[0].LastActivity, "queued for a test slot") {
			activity = *steps[0].LastActivity
			break
		}
	}
	if !strings.Contains(activity, "run other (feature)") {
		t.Fatalf("queued activity = %q, want it to name the holder", activity)
	}
	if step.callCount() != 0 {
		t.Fatal("test step executed while no slot was free")
	}

	const wait = 1500 * time.Millisecond
	time.Sleep(wait)
	held.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if step.callCount() != 1 {
		t.Fatalf("test step calls = %d, want 1", step.callCount())
	}
	steps, _ := database.GetStepsByRun(run.ID)
	if steps[0].DurationMS == nil || time.Duration(*steps[0].DurationMS)*time.Millisecond >= wait {
		t.Fatalf("step duration %v counted the slot wait (>= %v)", steps[0].DurationMS, wait)
	}
	requireSlotFree(t, pool)
	if log := readTestStepLog(t, p.RunLogDir(run.ID)); !strings.Contains(log, "acquired a test slot after waiting") {
		t.Fatalf("step log does not record the end of the wait:\n%s", log)
	}
}

// requireSlotFree fails unless the pool's only slot can be taken at once,
// which is how a caller observes that the previous holder released it.
func requireSlotFree(t *testing.T, pool *testslots.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	lease, waited, err := pool.Acquire(ctx, 1, "probe", nil)
	if err != nil || waited != 0 {
		t.Fatalf("slot still held: waited %v, err %v", waited, err)
	}
	lease.Release()
}

func readTestStepLog(t *testing.T, logDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, "test.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The step log is what `axi logs` shows an operator. A run that found a slot
// free never queued, so its log must not report a wait.
func TestExecutor_TestStepWithAFreeSlotLogsNoWait(t *testing.T) {
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, &config.Config{TestSlots: 1}, nil, []Step{newPassStep(types.StepTest)}, nil)
	exec.SetTestSlotPool(testslots.NewPool())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.Execute(ctx, run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	log := readTestStepLog(t, p.RunLogDir(run.ID))
	if !strings.Contains(log, "executing test") {
		t.Fatalf("step log does not show the step ran:\n%s", log)
	}
	for _, line := range []string{"queued for a test slot", "acquired a test slot"} {
		if strings.Contains(log, line) {
			t.Fatalf("step log reports %q for a run that never waited:\n%s", line, log)
		}
	}
}

// Queue time is never Test step time (global-config.md, test_slots). A run
// cancelled while still queued - aborted, or superseded by a new push - never
// executed its Test step, so the failed step must not be recorded as having
// run for as long as it waited.
func TestExecutor_CancelledQueueWaitIsNotRecordedAsTestStepTime(t *testing.T) {
	database, p, run, repo := setupTest(t)
	pool := testslots.NewPool()
	held, _, err := pool.Acquire(context.Background(), 1, "run other (feature)", nil)
	if err != nil {
		t.Fatal(err)
	}

	step := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, &config.Config{TestSlots: 1}, nil, []Step{step}, nil)
	exec.SetTestSlotPool(pool)
	const wait = 1500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := exec.Execute(ctx, run, repo, t.TempDir()); err == nil {
		t.Fatal("run cancelled while queued reported success")
	}
	if step.callCount() != 0 {
		t.Fatal("test step executed without a slot")
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps = %v, err %v", steps, err)
	}
	if steps[0].Status != types.StepStatusFailed {
		t.Fatalf("step status = %s, want failed", steps[0].Status)
	}
	if steps[0].DurationMS == nil || time.Duration(*steps[0].DurationMS)*time.Millisecond >= wait/2 {
		t.Fatalf("step duration %v ms counted the %v queue wait", steps[0].DurationMS, wait)
	}
	// The cancelled run left the queue: it takes no slot once one frees up.
	held.Release()
	requireSlotFree(t, pool)
}

// A slot is released however the Test step ends. Each case runs one Test step
// to that end and then takes the pool's only slot.
func TestExecutor_TestStepReleasesItsSlotOnEveryExit(t *testing.T) {
	for _, tc := range []struct {
		name string
		step func(cancel context.CancelFunc) Step
	}{
		{name: "pass", step: func(context.CancelFunc) Step { return newPassStep(types.StepTest) }},
		{name: "fail", step: func(context.CancelFunc) Step { return newFailStep(types.StepTest, errors.New("suite failed")) }},
		{name: "abort", step: func(cancel context.CancelFunc) Step { return &abortedStep{cancel: cancel} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			pool := testslots.NewPool()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			exec := NewExecutor(database, p, &config.Config{TestSlots: 1}, nil, []Step{tc.step(cancel)}, nil)
			exec.SetTestSlotPool(pool)
			_ = exec.Execute(ctx, run, repo, t.TempDir())
			requireSlotFree(t, pool)
		})
	}
}

// abortedStep is a Test step whose run is cancelled while it executes, the
// shape of an abort or a superseding push.
type abortedStep struct{ cancel context.CancelFunc }

func (s *abortedStep) Name() types.StepName { return types.StepTest }

func (s *abortedStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	s.cancel()
	<-sctx.Ctx.Done()
	return nil, sctx.Ctx.Err()
}

func TestExecutor_NonTestStepsIgnoreSlots(t *testing.T) {
	database, p, run, repo := setupTest(t)
	pool := testslots.NewPool()
	held, _, _ := pool.Acquire(context.Background(), 1, "run other (feature)", nil)
	defer held.Release()
	exec := NewExecutor(database, p, &config.Config{TestSlots: 1}, nil, []Step{newPassStep(types.StepLint)}, nil)
	exec.SetTestSlotPool(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.Execute(ctx, run, repo, t.TempDir()); err != nil {
		t.Fatalf("lint step blocked by test slots: %v", err)
	}
}

// test_slots: 0 disables the cap, so a Test step runs even while the pool it
// shares is full for runs that do have a limit.
func TestExecutor_TestSlotsZeroIsUnlimited(t *testing.T) {
	database, p, run, repo := setupTest(t)
	pool := testslots.NewPool()
	held, _, _ := pool.Acquire(context.Background(), 1, "run other (feature)", nil)
	defer held.Release()
	step := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, &config.Config{TestSlots: 0}, nil, []Step{step}, nil)
	exec.SetTestSlotPool(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.Execute(ctx, run, repo, t.TempDir()); err != nil || step.callCount() != 1 {
		t.Fatalf("test step did not run with the cap disabled: calls %d, err %v", step.callCount(), err)
	}
}
