package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// test_slots caps Test steps across runs, so the daemon's runs must draw
// from one pool: with test_slots: 1, a second run's Test step waits, shows
// who holds the slot, and starts once the first run is aborted.
func TestRunsShareTheTestSlotCap(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepTest, started: started}}
	})
	cfg, err := os.ReadFile(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), append(cfg, []byte("test_slots: 1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, headSHA := setupTestGitRepo(t, p, d, "test-slot-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	push := func(branch string) string {
		t.Helper()
		var result ipc.PushReceivedResult
		if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
			Gate: p.RepoDir("test-slot-repo"),
			Ref:  "refs/heads/" + branch,
			Old:  "0000000000000000000000000000000000000000",
			New:  headSHA,
		}, &result); err != nil {
			t.Fatal(err)
		}
		return result.RunID
	}

	first := push("feature/one")
	waitForStartedBranch(t, started, "feature/one")
	second := push("feature/two")

	var activity string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		steps, _ := d.GetStepsByRun(second)
		if len(steps) == 1 && steps[0].LastActivity != nil && strings.Contains(*steps[0].LastActivity, "queued for a test slot") {
			activity = *steps[0].LastActivity
			break
		}
	}
	if !strings.Contains(activity, first) || !strings.Contains(activity, "feature/one") {
		t.Fatalf("second run's activity = %q, want it queued behind run %s (feature/one)", activity, first)
	}
	select {
	case branch := <-started:
		t.Fatalf("Test step for %s started while the only slot was held", branch)
	case <-time.After(300 * time.Millisecond):
	}

	if err := client.Call(ipc.MethodCancelRun, &ipc.CancelRunParams{RunID: first}, nil); err != nil {
		t.Fatal(err)
	}
	waitForStartedBranch(t, started, "feature/two")
}
