package daemon

import (
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// startCIModeRun launches a run with no skip steps and returns the CI step's
// execution count plus its recorded step result once the run is terminal.
func startCIModeRun(t *testing.T, globalYAML string) (int32, *db.StepResult) {
	t.Helper()
	review := &mockPassStep{name: types.StepReview}
	ci := &mockPassStep{name: types.StepCI}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{review, ci}
	})
	appendGlobalConfig(t, p, globalYAML)
	_, headSHA := setupTestGitRepo(t, p, d, "ci-mode-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("ci-mode-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}
	if got := review.execCnt.Load(); got != 1 {
		t.Fatalf("review executed %d times, want 1", got)
	}
	steps, err := d.GetStepsByRun(result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step.StepName == types.StepCI {
			return ci.execCnt.Load(), step
		}
	}
	t.Fatal("run has no ci step result")
	return 0, nil
}

func appendGlobalConfig(t *testing.T, p *paths.Paths, extra string) {
	t.Helper()
	existing, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), append(existing, extra...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunWithoutSkipFlagsSkipsCIUnderDefaultLocalMode(t *testing.T) {
	executed, ci := startCIModeRun(t, "")
	if executed != 0 {
		t.Fatalf("ci executed %d times, want 0", executed)
	}
	if ci.Status != types.StepStatusSkipped {
		t.Fatalf("ci status = %s, want %s", ci.Status, types.StepStatusSkipped)
	}
	if ci.SkipReason == nil || *ci.SkipReason != config.LocalCISkipReason {
		t.Fatalf("ci skip reason = %v, want %q", ci.SkipReason, config.LocalCISkipReason)
	}
}

func TestRunInGitHubCIModeRunsCIStep(t *testing.T) {
	executed, ci := startCIModeRun(t, "ci_mode: github\n")
	if executed != 1 {
		t.Fatalf("ci executed %d times, want 1", executed)
	}
	if ci.Status != types.StepStatusCompleted {
		t.Fatalf("ci status = %s, want %s", ci.Status, types.StepStatusCompleted)
	}
}
