package procguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// forkRefusedText is how a refused fork (EAGAIN) reads in a tool's output.
const forkRefusedText = "resource temporarily unavailable"

// refusedOutput reports whether a tool's output shows a refused fork: the
// failed fork/exec the wrapper prints, the Go runtime's report that the
// re-entered guard binary could not start an OS thread (errno=11, EAGAIN), or
// the agent shell's own refusal (dash: "Cannot fork"; bash: "fork: Resource
// temporarily unavailable"). The match is case-insensitive.
func refusedOutput(out string) bool {
	out = strings.ToLower(out)
	return strings.Contains(out, forkRefusedText) ||
		strings.Contains(out, "failed to create new os thread") ||
		strings.Contains(out, "cannot fork")
}

// forkRefusedFatal fails the test naming the real cause. A fork is refused
// when the cgroup's pids.max (a service's TasksMax) or the user's process limit
// is reached, which says nothing about the guard under test; report that with
// the limit's numbers instead of a misleading assertion about pgrep or pkill.
func forkRefusedFatal(t *testing.T, what string, detail any) {
	t.Helper()
	text := fmt.Sprint(detail)
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	t.Fatalf("%s: the host refused a fork (EAGAIN, %q): the task/process limit was reached while these tests ran, so this is an environment limit, not a procguard failure. %s\ndetail: %s",
		what, forkRefusedText, cgroupPidsSummary(), text)
}

// cgroupPidsSummary reads this process's cgroup task counters, best effort.
func cgroupPidsSummary() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "cgroup task counters unavailable"
	}
	_, rel, ok := strings.Cut(strings.TrimSpace(string(data)), "::")
	if !ok {
		return "cgroup task counters unavailable"
	}
	dir := filepath.Join("/sys/fs/cgroup", strings.TrimSpace(rel))
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "?"
		}
		return strings.TrimSpace(string(b))
	}
	return fmt.Sprintf("cgroup %s: pids.current=%s pids.max=%s", rel, read("pids.current"), read("pids.max"))
}

// retryRefusedFork runs op until its fork is no longer refused. A refused fork
// is momentary under load (other processes exit), so retry for a few seconds;
// when it persists, fail with the real cause. op reports refused=true when the
// fork failed before the tool did anything, so retrying is safe.
func retryRefusedFork(t *testing.T, what string, op func() (refused bool, detail any)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	delay := 10 * time.Millisecond
	for {
		refused, detail := op()
		if !refused {
			return
		}
		if time.Now().After(deadline) {
			forkRefusedFatal(t, what, detail)
		}
		time.Sleep(delay)
		if delay < 250*time.Millisecond {
			delay *= 2
		}
	}
}

// runAgentShell runs script in sh with env, returning stdout and the combined
// output. A refused fork (the test's own, or the guard's real-tool fork whose
// failure the wrapper prints) is retried rather than surfacing as a failed
// assertion.
func runAgentShell(t *testing.T, env []string, script string) (stdout, combined string, err error) {
	t.Helper()
	retryRefusedFork(t, "agent shell "+script, func() (bool, any) {
		var so, se bytes.Buffer
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = env
		cmd.Stdout = &so
		cmd.Stderr = &se
		err = cmd.Run()
		stdout, combined = so.String(), so.String()+se.String()
		if errors.Is(err, syscall.EAGAIN) || refusedOutput(combined) {
			return true, fmt.Sprintf("err=%v output=%q", err, combined)
		}
		return false, nil
	})
	return stdout, combined, err
}

// startTagged starts a long sleep whose command line carries tag, with the
// run marker set to run (or no marker when run is empty), and reaps it when
// the test ends.
func startTagged(t *testing.T, tag, run string) *exec.Cmd {
	t.Helper()
	env := []string{"PATH=" + os.Getenv("PATH")}
	if run != "" {
		env = append(env, MarkerEnv+"="+run)
	}
	var cmd *exec.Cmd
	retryRefusedFork(t, "start sleep "+tag, func() (bool, any) {
		cmd = exec.Command("sleep", tag)
		cmd.Env = env
		err := cmd.Start()
		if errors.Is(err, syscall.EAGAIN) {
			return true, err
		}
		if err != nil {
			t.Fatal(err)
		}
		return false, nil
	})
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

	_, out, err := runAgentShell(t, env, "pkill -f 'sleep "+tag+"'; echo pkill-exit=$?")
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

	_, out, err := runAgentShell(t, env, "for p in $(pgrep -f 'sleep "+tag+"'); do kill $p; done; echo done")
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

// TestGuardedPgrepNewestIgnoresANewerSiblingRunProcess: the sibling's process
// started after this run's own match, so the real `pgrep -n` answers with the
// sibling's PID; the guard must still return this run's own match.
func TestGuardedPgrepNewestIgnoresANewerSiblingRunProcess(t *testing.T) {
	env := installedGuard(t)
	tag := fmt.Sprintf("%d.%d", 7000+os.Getpid()%1000, time.Now().UnixNano()%1000000)
	own := startTagged(t, tag, "run-own")
	time.Sleep(50 * time.Millisecond)
	_ = startTagged(t, tag, "run-sibling")

	out, combined, err := runAgentShell(t, env, "pgrep -n -f 'sleep "+tag+"'")
	if err != nil {
		t.Fatalf("pgrep -n failed: %v\n%s", err, combined)
	}
	if got := strings.TrimSpace(out); got != fmt.Sprint(own.Process.Pid) {
		t.Fatalf("pgrep -n = %q, want this run's own process %d", got, own.Process.Pid)
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

// TestStackedGuardsResolveTheRealToolInsteadOfEachOther reproduces the gate
// incident: a gate run's Test agent already has its run's guard on PATH when
// it runs this package's tests, which install a second guard in front. Each
// wrapper used to skip only its own directory, so the inner guard resolved
// the "real" pgrep to the outer guard's wrapper and the outer one back to the
// inner's, re-entering each other until the host ran out of memory. The
// oracle is the real pgrep's answer: this run's own PID, printed promptly.
func TestStackedGuardsResolveTheRealToolInsteadOfEachOther(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(t.TempDir(), "inner-guard")
	outer := filepath.Join(t.TempDir(), "outer-guard")
	for _, dir := range []string{inner, outer} {
		if err := Install(dir, exe); err != nil {
			t.Fatal(err)
		}
	}
	tag := fmt.Sprintf("%d.%d", 9000+os.Getpid()%1000, time.Now().UnixNano()%1000000)
	own := startTagged(t, tag, "run-own")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	shell := exec.CommandContext(ctx, "sh", "-c", "pgrep -f 'sleep "+tag+"'")
	shell.Env = []string{
		"PATH=" + inner + string(os.PathListSeparator) + outer + string(os.PathListSeparator) + os.Getenv("PATH"),
		MarkerEnv + "=run-own",
	}
	out, err := shell.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("stacked guards never reached the real pgrep (re-entered each other):\n%s", out)
	}
	if err != nil {
		t.Fatalf("pgrep through stacked guards failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), fmt.Sprint(own.Process.Pid)) {
		t.Fatalf("pgrep through stacked guards = %q, want this run's PID %d", out, own.Process.Pid)
	}
}
