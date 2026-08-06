//go:build !windows

package daemon

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reapWorktreeOrphans terminates processes still anchored to the run worktree
// (working directory inside it, or the worktree path in their argv) and
// returns how many it reaped. Each orphan is SIGTERMed individually, given a
// short drain, then SIGKILLed; individual kills are deliberate - killing an
// escaped process's whole group could reach processes with no proven tie to
// the worktree, and on Linux the cwd scan finds every member of an escaped
// chain anyway.
func reapWorktreeOrphans(wtPath string) int {
	root, ok := safeWorktreeReapPath(wtPath)
	if !ok {
		return 0
	}
	procs := findWorktreeProcesses(root)
	if len(procs) == 0 {
		return 0
	}
	for _, proc := range procs {
		slog.Warn("reaping orphaned process anchored to run worktree",
			"pid", proc.pid, "command", truncateReapCommand(proc.command), "worktree", root)
		_ = syscall.Kill(proc.pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		anyAlive := false
		for _, proc := range procs {
			if alive, _ := processRunning(proc.pid); alive {
				anyAlive = true
				break
			}
		}
		if !anyAlive {
			return len(procs)
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, proc := range procs {
		if alive, _ := processRunning(proc.pid); alive {
			_ = syscall.Kill(proc.pid, syscall.SIGKILL)
		}
	}
	return len(procs)
}

// findWorktreeProcesses enumerates processes anchored to the worktree. Two
// complementary detections run:
//
//   - a /proc scan (Linux) matching each process's current working directory
//     against the worktree - this catches shells and servers whose argv never
//     mentions the path;
//   - a portable `ps` argv scan matching command lines that embed the
//     worktree path (node/vite/esbuild style absolute-script invocations),
//     which also works on platforms without /proc.
//
// The current process is always excluded. Matching uses the worktree path and
// its symlink-resolved form (macOS /var vs /private/var).
func findWorktreeProcesses(root string) []worktreeProc {
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		roots = append(roots, resolved)
	}
	self := os.Getpid()
	found := make(map[int]string)

	procScanWorktree(roots, self, found)
	psScanWorktree(roots, self, found)

	procs := make([]worktreeProc, 0, len(found))
	for pid, command := range found {
		procs = append(procs, worktreeProc{pid: pid, command: command})
	}
	return procs
}

// procScanWorktree walks /proc matching process cwd and cmdline. A missing
// /proc (macOS) or unreadable entries (other users' processes) are silently
// skipped; the ps scan still runs.
func procScanWorktree(roots []string, self int, found map[int]string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		cmdline := readProcCmdline(pid)
		cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err == nil && pathWithinAny(cwd, roots) {
			found[pid] = cmdline
			continue
		}
		if cmdline != "" && containsAny(cmdline, roots) {
			found[pid] = cmdline
		}
	}
}

func readProcCmdline(pid int) string {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

// psScanWorktree matches `ps` command lines that embed a worktree root.
func psScanWorktree(roots []string, self int, found map[int]string) {
	cmd := exec.Command(psExecutable(), "-ww", "-eo", "pid=,command=")
	env := upsertEnv(os.Environ(), "LC_ALL", "C")
	cmd.Env = upsertEnv(env, "LANG", "C")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		pid, command, ok := splitUnixProcessLine(line)
		if !ok || pid == self {
			continue
		}
		if _, exists := found[pid]; exists {
			continue
		}
		if containsAny(command, roots) {
			found[pid] = command
		}
	}
}

func pathWithinAny(path string, roots []string) bool {
	cleaned := filepath.Clean(path)
	for _, root := range roots {
		if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func containsAny(text string, roots []string) bool {
	for _, root := range roots {
		if strings.Contains(text, root) {
			return true
		}
	}
	return false
}

func truncateReapCommand(command string) string {
	const max = 160
	if len(command) <= max {
		return command
	}
	return command[:max] + "..."
}
