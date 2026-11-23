package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// internalOnlyRepo commits one change under internal/ directly on the
// template base, so base..head touches no docs, CLI, config, schema, or API
// path. Each extra path is committed alongside it.
func internalOnlyRepo(t *testing.T, extra ...string) (string, string, string) {
	t.Helper()
	dir, baseSHA, _ := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", baseSHA)
	for _, name := range extra {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("user-facing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal", "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "store", "store.go"), []byte("package store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "internal change")
	return dir, baseSHA, gitCmd(t, dir, "rev-parse", "HEAD")
}

func TestDocumentStep_SkipsAnInternalOnlyChange(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := internalOnlyRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		calls++
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"noop"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Document.SkipInternal = true

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("an internal-only change must not run the document agent, got %d calls", calls)
	}
	if !outcome.Skipped || outcome.SkipReason != documentInternalOnlySkipReason {
		t.Fatalf("expected a skipped outcome naming the reason, got %+v", outcome)
	}
	if _, ok := sctx.Shared.TakeHousekeepingLint(); ok {
		t.Fatal("a skipped document step must leave no housekeeping lint result, so lint runs its own pass")
	}
}

// With the switch off, or when the change touches a user-facing path, the
// step runs exactly as before.
func TestDocumentStep_KeepsTheStepWithoutACleanInternalOnlySignal(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		skipInternal bool
		extra        []string
	}{
		"switch off":         {skipInternal: false},
		"user-facing change": {skipInternal: true, extra: []string{"README.md"}},
		// git C-quotes a non-ASCII name unless the list is NUL-separated, and
		// the quoted form hides the .md suffix from the docs classifier.
		"non-ASCII doc name": {skipInternal: true, extra: []string{"svc/internal/guide_\u00fc.md"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := internalOnlyRepo(t, tc.extra...)
			calls := 0
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"noop"}`)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Document.SkipInternal = tc.skipInternal

			outcome, err := (&DocumentStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || outcome.Skipped {
				t.Fatalf("expected the document agent to run once without a skip, calls=%d outcome=%+v", calls, outcome)
			}
		})
	}
}
