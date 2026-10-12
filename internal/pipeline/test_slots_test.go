package pipeline

import (
	"context"
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
	pool := testslots.NewPool(p.TestSlotsDir(), 1)
	held, _, err := pool.Acquire(context.Background(), "run other (feature)", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	step := newPassStep(types.StepTest)
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
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
	if got := pool.Holders(); len(got) != 0 {
		t.Fatalf("slot still held after the step finished: %v", got)
	}
}

func TestExecutor_TestStepReleasesSlotOnFailure(t *testing.T) {
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, &config.Config{TestSlots: 1}, nil, []Step{newFailStep(types.StepTest, context.Canceled)}, nil)
	_ = exec.Execute(context.Background(), run, repo, t.TempDir())
	if got := testslots.NewPool(p.TestSlotsDir(), 1).Holders(); len(got) != 0 {
		t.Fatalf("slot leaked after failed step: %v", got)
	}
}

func TestExecutor_NonTestStepsIgnoreSlots(t *testing.T) {
	database, p, run, repo := setupTest(t)
	pool := testslots.NewPool(p.TestSlotsDir(), 1)
	held, _, _ := pool.Acquire(context.Background(), "run other (feature)", nil)
	defer held.Release()
	exec := NewExecutor(database, p, &config.Config{TestSlots: 1}, nil, []Step{newPassStep(types.StepLint)}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.Execute(ctx, run, repo, t.TempDir()); err != nil {
		t.Fatalf("lint step blocked by test slots: %v", err)
	}
}
