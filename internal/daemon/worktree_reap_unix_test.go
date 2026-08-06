//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// fakeWorktreePath returns a directory shaped like a real run worktree
// (<root>/worktrees/<repo>/<run>) so safeWorktreeReapPath accepts it.
func fakeWorktreePath(t *testing.T) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "worktrees", "repo-id", "01RUNID")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	return wt
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := processRunning(pid); !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d still running after reap", pid)
}

// A process whose working directory is inside the worktree is an orphan even
// when its argv never mentions the path (the observed leak: an agent's
// backgrounded `pnpm run dev` shell chain living in the worktree).
func TestReapWorktreeOrphans_KillsProcessAnchoredByCwd(t *testing.T) {
	if _, err := os.Stat("/proc"); err != nil {
		t.Skip("cwd-based detection needs /proc")
	}
	wt := fakeWorktreePath(t)
	cmd := exec.Command("sleep", "300")
	cmd.Dir = wt
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if n := reapWorktreeOrphans(wt); n == 0 {
		t.Fatal("reap found no orphans; expected the cwd-anchored process")
	}
	waitForProcessGone(t, pid)
}

// A process whose argv embeds the worktree path (node/vite/esbuild style
// absolute-script invocations) is an orphan even when its cwd is elsewhere.
func TestReapWorktreeOrphans_KillsProcessAnchoredByArgv(t *testing.T) {
	wt := fakeWorktreePath(t)
	// Two shell commands keep the shell itself alive (a single command may be
	// exec'd in place, dropping the worktree path from the surviving argv).
	cmd := exec.Command("/bin/sh", "-c", "sleep 300; sleep 0", filepath.Join(wt, "vite.js"))
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if n := reapWorktreeOrphans(wt); n == 0 {
		t.Fatal("reap found no orphans; expected the argv-anchored process")
	}
	waitForProcessGone(t, pid)
}

// The reaper must refuse shallow or relative paths outright: matching against
// something like "/" or "/tmp" could kill unrelated processes.
func TestReapWorktreeOrphans_RefusesUnsafePaths(t *testing.T) {
	for _, path := range []string{"", "/", "/tmp", "/tmp/x", "relative/worktree/path/deep"} {
		if n := reapWorktreeOrphans(path); n != 0 {
			t.Fatalf("reap(%q) = %d, want 0 (unsafe path must be refused)", path, n)
		}
	}
}

func TestReapWorktreeOrphans_NeverTargetsSelf(t *testing.T) {
	wt := fakeWorktreePath(t)
	// The test binary's own pid must be excluded even if evidence matched.
	self := os.Getpid()
	procs := findWorktreeProcesses(wt)
	for _, proc := range procs {
		if proc.pid == self {
			t.Fatalf("findWorktreeProcesses matched the current process (pid %d)", self)
		}
	}
}
