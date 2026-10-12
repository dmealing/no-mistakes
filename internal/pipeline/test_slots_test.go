package pipeline

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
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

// envProbeStep records the subprocess environment a step hands to its
// configured commands (StepContext.Env) and to its agent (RunOpts.Env).
type envProbeStep struct {
	name     types.StepName
	stepEnv  []string
	agentEnv []string
}

func (s *envProbeStep) Name() types.StepName { return s.name }

func (s *envProbeStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	s.stepEnv = slices.Clone(sctx.Env)
	if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "probe"}); err != nil {
		return nil, err
	}
	return &StepOutcome{}, nil
}

type envCaptureAgent struct{ env []string }

func (a *envCaptureAgent) Name() string { return "env-capture" }
func (a *envCaptureAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.env = slices.Clone(opts.Env)
	return &agent.Result{}, nil
}
func (a *envCaptureAgent) Close() error { return nil }

// `no-mistakes slot run` skips the pool only when testslots.HeldEnv is set.
// A Test step holding a slot must therefore set it for everything it starts,
// or a configured test command that wraps its suite in `slot run` queues
// behind the slot its own step holds. A step that holds no slot must not set
// it, or `slot run` from that step would run outside the cap.
func TestExecutor_OnlyASlotHoldingStepMarksItsSubprocessEnvironment(t *testing.T) {
	const marker = testslots.HeldEnv + "=1"
	for _, tc := range []struct {
		name      string
		step      types.StepName
		testSlots int
		want      bool
	}{
		{name: "test step holding a slot", step: types.StepTest, testSlots: 1, want: true},
		{name: "lint step holds no slot", step: types.StepLint, testSlots: 1, want: false},
		{name: "unlimited pool holds no slot", step: types.StepTest, testSlots: 0, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := &envProbeStep{name: tc.step}
			capture := &envCaptureAgent{}
			exec := NewExecutor(database, p, &config.Config{TestSlots: tc.testSlots}, capture, []Step{step}, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := exec.Execute(ctx, run, repo, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(step.stepEnv, marker); got != tc.want {
				t.Errorf("configured-command environment %v: has %s = %v, want %v", step.stepEnv, marker, got, tc.want)
			}
			if got := slices.Contains(capture.env, marker); got != tc.want {
				t.Errorf("agent environment %v: has %s = %v, want %v", capture.env, marker, got, tc.want)
			}
		})
	}
}
