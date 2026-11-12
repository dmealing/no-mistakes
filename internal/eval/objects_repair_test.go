package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// tornCaseID names a case whose capture was interrupted: its pins never made it
// into the registry, which is exactly the shape a crash mid-capture leaves.
const tornCaseID = "01TORNCAPTURE000000000000-01TORNCAPTURE000000000001"

// writeTornCaseRefs reproduces what an unclean shutdown shortly after a
// capture leaves in a pool when references are not fsynced: the loose ref
// files exist but are zero bytes. Git cannot parse such a ref, refuses to
// delete it with update-ref, and fails every fetch connectivity check and every
// repack while it is present.
func writeTornCaseRefs(t *testing.T, pool, caseID string) {
	t.Helper()
	dir := filepath.Join(pool, "refs", "no-mistakes", "eval", caseID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range caseRefNames() {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// writeFailedGCLog plants the gc.log a detached `git gc --auto` writes when its
// repack dies on a bad ref. While it is present Git skips automatic gc in that
// repository.
func writeFailedGCLog(t *testing.T, pool, caseID string) string {
	t.Helper()
	path := filepath.Join(pool, "gc.log")
	body := "fatal: bad object refs/no-mistakes/eval/" + caseID + "/base\nfatal: failed to run repack\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertNoCasePins(t *testing.T, ctx context.Context, pool, caseID string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(pool, "refs", "no-mistakes", "eval", caseID)); err == nil {
		entries, _ := os.ReadDir(filepath.Join(pool, "refs", "no-mistakes", "eval", caseID))
		if len(entries) > 0 {
			t.Fatalf("damaged case %s still has loose ref files: %v", caseID, entries)
		}
	}
	refs := mustGit(t, ctx, pool, "for-each-ref", "--format=%(refname)", caseRefNamespace)
	if strings.Contains(refs, caseID) {
		t.Fatalf("damaged case %s is still pinned: %s", caseID, refs)
	}
}

func assertPoolRepacks(t *testing.T, ctx context.Context, pool string) {
	t.Helper()
	// Run gc in the foreground so a failure is an error here instead of a
	// gc.log nobody reads, which is how the live failure stayed silent.
	if _, err := git.Run(ctx, pool, "-c", "gc.autoDetach=false", "gc", "--quiet"); err != nil {
		t.Fatalf("pool cannot be repacked after repair: %v", err)
	}
}

// TestCaptureRecoversFromTornCaseRef is the regression test for the eval pool
// that stopped collecting and stopped shrinking: one interrupted capture left
// zero-byte refs behind, after which every later capture fetched a full pack
// into the pool, failed its connectivity check on the torn ref, and leaked that
// pack, while gc failed on the same ref and recorded the failure only in a
// gc.log that then suppressed automatic gc.
func TestCaptureRecoversFromTornCaseRef(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool := store.poolDir(first[0].RepoFingerprint)
	writeTornCaseRefs(t, pool, tornCaseID)
	gcLog := writeFailedGCLog(t, pool, tornCaseID)

	addSecondReviewRound(t, ctx, sourceDB, run.ID, repo.WorkingPath, firstRound)
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatalf("capture is still wedged by a torn pool ref: %v", err)
	}
	if len(cases) != 2 {
		t.Fatalf("captured cases = %d, want 2", len(cases))
	}

	assertNoCasePins(t, ctx, pool, tornCaseID)
	if _, err := os.Stat(gcLog); !os.IsNotExist(err) {
		t.Fatalf("stale gc.log survived the repair and still blocks automatic gc: %v", err)
	}
	for _, c := range cases {
		restored := filepath.Join(t.TempDir(), "restore.git")
		if err := git.InitBare(ctx, restored); err != nil {
			t.Fatal(err)
		}
		if err := restoreCaseObjects(ctx, pool, restored, c.ID); err != nil {
			t.Fatalf("healthy case %s is not restorable after repair: %v", c.ID, err)
		}
	}
	assertPoolRepacks(t, ctx, pool)
}

// TestCaptureDropsCaseWhosePinNamesAMissingObject covers the other way a pool
// ref goes bad: the ref file survives but the object it names does not. A case
// missing any one of its pins cannot be replayed, so every pin of that case is
// released rather than leaving the rest to keep dead objects alive.
func TestCaptureDropsCaseWhosePinNamesAMissingObject(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool := store.poolDir(first[0].RepoFingerprint)
	headSHA := mustGit(t, ctx, pool, "rev-parse", caseRefPrefix(first[0].ID)+refHead)
	const damagedID = "01MISSINGOBJECT0000000000-01MISSINGOBJECT0000000001"
	prefix := caseRefPrefix(damagedID)
	// Three pins are perfectly healthy; one names an object the pool lacks.
	for _, name := range []string{refHead, refSourceHead, refTrustedConfig} {
		mustGit(t, ctx, pool, "update-ref", prefix+name, headSHA)
	}
	missing := strings.Repeat("1", len(headSHA))
	if err := os.WriteFile(filepath.Join(pool, "refs", "no-mistakes", "eval", damagedID, refBase), []byte(missing+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	addSecondReviewRound(t, ctx, sourceDB, run.ID, repo.WorkingPath, firstRound)
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatalf("capture is wedged by a pool ref naming a missing object: %v", err)
	}
	assertNoCasePins(t, ctx, pool, damagedID)
	if got := mustGit(t, ctx, pool, "rev-parse", caseRefPrefix(first[0].ID)+refHead); got != headSHA {
		t.Fatalf("healthy case pin moved: %s, want %s", got, headSHA)
	}
	assertPoolRepacks(t, ctx, pool)
}

// TestDropCaseObjectsRecoversFromTornCaseRef pins the retention side: Git
// refuses to delete a ref it cannot parse, so dropping a case whose own pins
// were torn used to fail every retention pass, and any other torn case wedged
// the gc that retention runs.
func TestDropCaseObjectsRecoversFromTornCaseRef(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, repo, firstRound := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	addSecondReviewRound(t, ctx, sourceDB, run.ID, repo.WorkingPath, firstRound)
	cases, err := Capture(ctx, store, p, sourceDB, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	pool := store.poolDir(cases[0].RepoFingerprint)
	// The case being dropped and an unrelated interrupted capture are both torn.
	writeTornCaseRefs(t, pool, cases[0].ID)
	writeTornCaseRefs(t, pool, tornCaseID)
	gcLog := writeFailedGCLog(t, pool, tornCaseID)

	if err := dropCaseObjects(ctx, pool, cases[0].ID); err != nil {
		t.Fatalf("retention is wedged by a torn pool ref: %v", err)
	}
	assertNoCasePins(t, ctx, pool, cases[0].ID)
	assertNoCasePins(t, ctx, pool, tornCaseID)
	if _, err := os.Stat(gcLog); !os.IsNotExist(err) {
		t.Fatalf("stale gc.log survived the repair and still blocks automatic gc: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "restore.git")
	if err := git.InitBare(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if err := restoreCaseObjects(ctx, pool, restored, cases[1].ID); err != nil {
		t.Fatalf("surviving case is no longer restorable: %v", err)
	}
	assertPoolRepacks(t, ctx, pool)
}

// TestObjectPoolHardensReferences pins the prevention half: Git's platform
// default fsyncs packs but not references or loose objects, so an unclean
// shutdown just after a capture can leave a zero-byte ref, or a ref naming a
// loose object that never reached the disk.
func TestObjectPoolHardensReferences(t *testing.T) {
	ctx := context.Background()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	pool := store.poolDir(strings.Repeat("b", 64))
	if err := initializeObjectPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(mustGit(t, ctx, pool, "config", "core.fsync"))
	components := map[string]bool{}
	for _, c := range strings.Split(got, ",") {
		components[strings.TrimSpace(c)] = true
	}
	if !components["reference"] {
		t.Fatalf("core.fsync = %q does not harden references", got)
	}
	if !components["committed"] && !components["objects"] && !components["loose-object"] {
		t.Fatalf("core.fsync = %q does not harden loose objects", got)
	}
}
