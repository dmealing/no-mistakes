package steps

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// overrideBaseFetchRetry shortens the base-fetch retry schedule for one
// (non-parallel) test.
func overrideBaseFetchRetry(t *testing.T, attempts int, delay time.Duration) {
	t.Helper()
	origAttempts, origDelay := baseFetchAttempts, baseFetchRetryDelay
	baseFetchAttempts, baseFetchRetryDelay = attempts, delay
	t.Cleanup(func() { baseFetchAttempts, baseFetchRetryDelay = origAttempts, origDelay })
}

// baseFetchRetryContext is a worktree whose origin names a repository that
// does not exist yet, so its base-branch fetch fails until onRetry makes it.
func baseFetchRetryContext(t *testing.T, onRetry func(upstream, workDir string)) (*pipeline.StepContext, *[]string) {
	t.Helper()
	dir, _, _ := setupGitRepo(t)
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	var mu sync.Mutex
	var logs []string
	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: dir,
		Repo:    &db.Repo{UpstreamURL: upstream},
		Log: func(line string) {
			mu.Lock()
			logs = append(logs, line)
			mu.Unlock()
			if strings.Contains(line, "retrying in") && onRetry != nil {
				onRetry(upstream, dir)
			}
		},
	}
	return sctx, &logs
}

// TestResolveBranchBaseSHA_RetriesATransientBaseFetchFailure: the recorded
// base-fetch failures that failed finished reviews were transient, so a fetch
// that fails once and then succeeds resolves the base instead of failing the
// step.
func TestResolveBranchBaseSHA_RetriesATransientBaseFetchFailure(t *testing.T) {
	if testGitExecutable == "" {
		t.Skip("git not available")
	}
	overrideBaseFetchRetry(t, 3, time.Millisecond)
	sctx, logs := baseFetchRetryContext(t, func(upstream, workDir string) {
		if out, err := exec.Command(testGitExecutable, "clone", "-q", "--bare", workDir, upstream).CombinedOutput(); err != nil {
			t.Errorf("create upstream: %v: %s", err, out)
		}
	})

	base, err := resolveBranchBaseSHA(context.Background(), sctx, "", "main")
	if err != nil {
		t.Fatalf("a fetch that succeeds on retry must resolve the base, got %v", err)
	}
	if base == "" {
		t.Fatal("expected a resolved base commit")
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], `fetch of base branch "main" failed (attempt 1 of 3), retrying in`) {
		t.Fatalf("expected exactly one logged retry, got %q", *logs)
	}
}

// TestResolveBranchBaseSHA_ExhaustedBaseFetchIsMarked: a fetch that keeps
// failing still fails closed after its attempts, and the error is marked as a
// base-fetch failure so the executor records the cause a later run may
// continue from.
func TestResolveBranchBaseSHA_ExhaustedBaseFetchIsMarked(t *testing.T) {
	if testGitExecutable == "" {
		t.Skip("git not available")
	}
	overrideBaseFetchRetry(t, 3, time.Millisecond)
	sctx, logs := baseFetchRetryContext(t, nil)

	_, err := resolveBranchBaseSHA(context.Background(), sctx, "", "main")
	if err == nil {
		t.Fatal("a fetch that never succeeds must fail the step")
	}
	var marked *pipeline.BaseFetchError
	if !errors.As(err, &marked) {
		t.Fatalf("error = %v, want a *pipeline.BaseFetchError", err)
	}
	if !strings.Contains(err.Error(), `fetch default branch "main" to resolve branch base`) {
		t.Fatalf("error lost its context: %v", err)
	}
	if len(*logs) != 2 {
		t.Fatalf("expected two retries before giving up, got %q", *logs)
	}
}

// TestFetchBaseBranchWithRetry_CancelledContextIsNotRetried: a cancelled run
// is not a transient failure.
func TestFetchBaseBranchWithRetry_CancelledContextIsNotRetried(t *testing.T) {
	if testGitExecutable == "" {
		t.Skip("git not available")
	}
	overrideBaseFetchRetry(t, 3, time.Millisecond)
	sctx, logs := baseFetchRetryContext(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sctx.Ctx = ctx

	if err := fetchBaseBranchWithRetry(ctx, sctx, "main"); err == nil {
		t.Fatal("expected the cancelled fetch to fail")
	}
	if len(*logs) != 0 {
		t.Fatalf("a cancelled fetch must not be retried, got %q", *logs)
	}
}
