package procguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startTagged starts a long sleep whose command line carries tag, with the
// run marker set to run (or no marker when run is empty), and reaps it when
// the test ends.
func startTagged(t *testing.T, tag, run string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", tag)
	env := []string{"PATH=" + os.Getenv("PATH")}
	if run != "" {
		env = append(env, MarkerEnv+"="+run)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func exited(cmd *exec.Cmd, within time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// installedGuard installs the wrappers re-entering this test binary and
// returns the environment an agent of run "run-own" gets.
func installedGuard(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not installed")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "proc-guard", "bin")
	if err := Install(dir, exe); err != nil {
		t.Fatal(err)
	}
	return []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		MarkerEnv + "=run-own",
	}
}

// TestGuardedPkillStopsThisRunsSuiteAndSparesASiblingRuns reproduces the
// incident end to end: two runs both have a process with the same command
// line, and this run's agent stops "its" suite with a pattern kill from a
// shell whose own command line also matches the pattern.
func TestGuardedPkillStopsThisRunsSuiteAndSparesASiblingRuns(t *testing.T) {
	env := installedGuard(t)
	tag := fmt.Sprintf("%d.%d", 7000+os.Getpid()%1000, time.Now().UnixNano()%1000000)
	own := startTagged(t, tag, "run-own")
	sibling := startTagged(t, tag, "run-sibling")
	unmarked := startTagged(t, tag, "")

	shell := exec.Command("sh", "-c", "pkill -f 'sleep "+tag+"'; echo pkill-exit=$?")
	shell.Env = env
	out, err := shell.CombinedOutput()
	if err != nil {
		t.Fatalf("agent shell failed (a pattern kill must never reach its own shell): %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "pkill-exit=0") {
		t.Fatalf("pkill did not report stopping this run's process:\n%s", out)
	}
	if !exited(own, 5*time.Second) {
		t.Fatal("this run's own process is still running after pkill")
	}
	if exited(sibling, 300*time.Millisecond) {
		t.Fatal("pkill killed the sibling run's process")
	}
	if exited(unmarked, 100*time.Millisecond) {
		t.Fatal("pkill killed a process no run started")
	}
	if !strings.Contains(string(out), "left 2 matching process(es) alone") {
		t.Fatalf("pkill did not say which processes it spared:\n%s", out)
	}
}

// TestGuardedPgrepFeedsKillOnlyThisRunsProcesses covers the second half of
// the incident: `kill $(pgrep ...)` after pkill failed.
func TestGuardedPgrepFeedsKillOnlyThisRunsProcesses(t *testing.T) {
	env := installedGuard(t)
	tag := fmt.Sprintf("%d.%d", 8000+os.Getpid()%1000, time.Now().UnixNano()%1000000)
	own := startTagged(t, tag, "run-own")
	sibling := startTagged(t, tag, "run-sibling")

	shell := exec.Command("sh", "-c", "for p in $(pgrep -f 'sleep "+tag+"'); do kill $p; done; echo done")
	shell.Env = env
	out, err := shell.CombinedOutput()
	if err != nil {
		t.Fatalf("agent shell failed: %v\n%s", err, out)
	}
	if !exited(own, 5*time.Second) {
		t.Fatal("this run's own process is still running")
	}
	if exited(sibling, 300*time.Millisecond) {
		t.Fatal("kill on pgrep output reached the sibling run's process")
	}
}

// TestProcessStopRuleCommandStopsTheWholeGroup proves the command the Test
// prompt prescribes: a suite started under setsid is stopped, children and
// all, by signalling its process group.
func TestProcessStopRuleCommandStopsTheWholeGroup(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "suite.pid")
	childPID := filepath.Join(dir, "child.pid")
	start := exec.Command("sh", "-c", "setsid sh -c 'sleep 600 & echo $! > "+childPID+"; wait' > /dev/null 2>&1 & echo $! > "+pidfile)
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	child := waitPID(t, childPID)
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })

	stop := exec.Command("sh", "-c", `kill -TERM -"$(cat `+pidfile+`)"`)
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("stop command failed: %v\n%s", err, out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(child, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("the suite's child survived the process-group kill")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(path)
		var pid int
		if err == nil {
			if _, scanErr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); scanErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
