//go:build windows

package daemon

// reapWorktreeOrphans is a no-op on Windows: console children are contained by
// job objects via winproc/shellenv, an open handle inside the worktree makes
// the removal itself fail loudly rather than silently leak, and there is no
// /proc-style cwd evidence to identify escaped processes safely. Detection and
// reaping are deliberately not implemented until there is a safe identity
// signal.
func reapWorktreeOrphans(string) int { return 0 }
