package daemon

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// removeRunWorktree reaps orphaned processes still anchored to a run worktree,
// then removes the worktree. It is the single removal path for run worktrees
// (run-goroutine cleanup, recovered-run cleanup, setup-failure cleanup, and
// startup orphan cleanup).
//
// Why reap first: shellenv's process-group boundary kills the tree an agent
// command spawned, but a grandchild that moves itself into a new process group
// or session (observed: a Claude-run `pnpm run dev &` dev server with vite and
// esbuild children) escapes that boundary and survives the run. Such a
// survivor then keeps writing into the worktree (vite recreating its cache
// directory), which races `git worktree remove --force` into leaving the
// directory behind - so the leak also defeats worktree cleanup. Any process
// still anchored to the worktree at removal time is by definition an orphan:
// the run is over and nothing legitimate runs there anymore.
func removeRunWorktree(ctx context.Context, gateDir, wtPath string) error {
	reapWorktreeOrphans(wtPath)
	return git.WorktreeRemove(ctx, gateDir, wtPath)
}

// worktreeProc is one process anchored to a run worktree.
type worktreeProc struct {
	pid     int
	command string
}

// safeWorktreeReapPath guards against matching or killing against a path that
// is too generic: reaping is keyed on the run worktree path being unique and
// deep (<root>/worktrees/<repo>/<run>), so anything shallower is refused.
func safeWorktreeReapPath(wtPath string) (string, bool) {
	cleaned := filepath.Clean(wtPath)
	if !filepath.IsAbs(cleaned) {
		return "", false
	}
	separators := strings.Count(cleaned, string(filepath.Separator))
	if separators < 3 {
		return "", false
	}
	return cleaned, true
}
