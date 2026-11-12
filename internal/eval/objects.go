package eval

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// Every case pins the same four commits. They are named rather than positional
// so a pool ref reads as evidence of what it keeps alive.
const (
	refHead          = "head"
	refSourceHead    = "source-head"
	refBase          = "base"
	refTrustedConfig = "trusted-config"
	caseRefNamespace = "refs/no-mistakes/eval/"
)

func caseRefNames() []string {
	return []string{refHead, refSourceHead, refBase, refTrustedConfig}
}

func caseRefPrefix(caseID string) string { return caseRefNamespace + caseID + "/" }

// poolDir is the shared object store for every case captured from one source
// repository.
//
// Cases used to each carry a self-contained Git bundle, which is a full copy of
// the repository's history per review pass: ~8 MB per case for a small repo, so
// a machine reviewing a few dozen changes a day accumulated hundreds of
// megabytes a day of near-identical objects. A bundle cannot be trimmed to only
// the interesting commits, because a bundle built with negative refs records
// prerequisites and Git refuses to unbundle it into a repository that lacks
// them - and the whole point of a case is that it restores into an empty
// throwaway gate.
//
// Sharing one pool per repository fixes that at the source instead: the first
// case pays for the history once, and every later case transfers only the
// objects its own commits introduced (measured at ~8 KB). The case keeps its
// records; the pool keeps its objects; the case's refs are what tie them
// together and what keeps those objects alive against Git's own maintenance.
func (s *Store) poolDir(repoFingerprint string) string {
	name := strings.TrimSpace(repoFingerprint)
	if name == "" {
		name = "unknown"
	}
	return filepath.Join(s.root, "pools", name+".git")
}

// storeCaseObjects copies the commits one case replays from the source gate
// into that repository's pool and pins them under the case's ref prefix.
//
// It never writes to the gate: capture is read-only against live pipeline
// state. The reviewed commit is frequently not the tip of any gate ref by the
// time a case is captured (fix rounds advance the branch, and branches get
// deleted after a merge), and asking a remote for a bare object id depends on
// upload-pack's want policy, which is off by default and version-dependent. So
// the commits are named in a throwaway bare clone of the gate - local clones
// hardlink their objects, so this costs no meaningful disk or time - and the
// pool fetches them from there by refspec, which is ordinary supported Git.
func initializeObjectPool(ctx context.Context, pool string) error {
	if err := git.InitBare(ctx, pool); err != nil {
		return fmt.Errorf("create eval object pool: %w", err)
	}
	// Case refs intentionally carry both the case ID and a descriptive commit
	// name. In a normal Windows temp or user-data path their lock files can
	// exceed Git for Windows' legacy MAX_PATH handling even though the
	// filesystem supports the path. Keep this repository-local so capture does
	// not depend on or modify the operator's global Git configuration.
	if _, err := git.Run(ctx, pool, "config", "core.longpaths", "true"); err != nil {
		return fmt.Errorf("enable long paths in eval object pool: %w", err)
	}
	// The case refs are the only thing keeping a case's objects alive, so they
	// are hardened like the objects themselves. Git's platform default fsyncs
	// packs but neither references nor loose objects, and an unclean shutdown
	// just after a capture then leaves a zero-byte ref file (or a ref naming a
	// loose object that never reached the disk). One such ref made a pool fail
	// every later fetch and every repack. Set on every capture, so pools created
	// before this setting existed pick it up too.
	if _, err := git.Run(ctx, pool, "config", "core.fsync", "committed,reference"); err != nil {
		return fmt.Errorf("harden eval object pool refs: %w", err)
	}
	return nil
}

func (s *Store) storeCaseObjects(ctx context.Context, gateDir, repoFingerprint, caseID string, refs map[string]string) error {
	pool := s.poolDir(repoFingerprint)
	if err := initializeObjectPool(ctx, pool); err != nil {
		return err
	}
	// A damaged pin anywhere in the pool fails this fetch's connectivity check
	// after the pack has already landed, so without the repair every capture
	// from the repository leaks a pack and records nothing.
	if err := repairObjectPool(ctx, pool); err != nil {
		return err
	}
	// The staging clone goes under the eval root rather than the system temp
	// directory because both live on the same filesystem as the gate, which is
	// what lets Git hardlink its objects instead of copying them. Across
	// filesystems that clone would be a full byte-for-byte copy of the
	// repository on every single capture.
	tmp, err := os.MkdirTemp(s.root, ".objects-")
	if err != nil {
		return fmt.Errorf("create temporary capture directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	clone := filepath.Join(tmp, "source.git")
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", "--quiet", gateDir, clone)
	shellenv.ConfigureShellCommand(cmd)
	if err := shellenv.RunShellCommand(cmd); err != nil {
		// This error can carry Git's own output, and the gate's remote may
		// embed a credential, so it is redacted like every other git-run error
		// before it can reach a log.
		return fmt.Errorf("stage source gate objects for capture: %s", safeurl.RedactText(err.Error()))
	}
	prefix := caseRefPrefix(caseID)
	for _, name := range caseRefNames() {
		sha := strings.TrimSpace(refs[name])
		if sha == "" {
			return fmt.Errorf("case %q is missing its %s commit", caseID, name)
		}
		if _, err := git.Run(ctx, clone, "update-ref", prefix+name, sha); err != nil {
			return fmt.Errorf("pin case %s commit: %w", name, err)
		}
	}
	// --no-tags keeps the pool's refs to exactly the case pins. A tag ref
	// would pin objects no case needs and survive every retention pass,
	// quietly making the pool unreclaimable. --atomic writes the case's four
	// pins together or not at all, so a failed fetch cannot strand a partial
	// case. Fetch's own automatic maintenance is off because reclaimPoolSpace
	// below owns pool maintenance: Git silently skips that built-in pass while
	// a stale objects/maintenance.lock is present.
	_, fetchErr := git.Run(ctx, pool, "-c", "maintenance.auto=false", "fetch", "--atomic", "--no-tags", clone, "+"+prefix+"*:"+prefix+"*")
	// Best effort, and after a failed fetch too: a failure past the pack
	// transfer still leaves the received pack in the pool for gc to reclaim.
	_ = reclaimPoolSpace(ctx, pool)
	if fetchErr != nil {
		return fmt.Errorf("store case objects: %w", fetchErr)
	}
	return nil
}

// restoreCaseObjects fetches one case's pinned commits out of the pool into a
// throwaway gate. The pool is a plain local repository, so this is the same
// ordinary fetch the bundle path used, minus the duplicated history.
func restoreCaseObjects(ctx context.Context, poolPath, gateDir, caseID string) error {
	if err := git.ValidateBareRepository(ctx, poolPath); err != nil {
		return fmt.Errorf("case objects are unavailable: %w", err)
	}
	prefix := caseRefPrefix(caseID)
	if _, err := git.Run(ctx, gateDir, "fetch", "--no-tags", poolPath, "+"+prefix+"*:"+prefix+"*"); err != nil {
		return fmt.Errorf("restore case objects: %w", err)
	}
	return nil
}

// dropCaseObjects releases one case's claim on the pool. Deleting the refs
// makes its objects unreachable while any object another case still pins stays
// alive. It is best effort by design - a pool that is already gone means the
// case's objects are gone too, which is exactly the requested state.
func dropCaseObjects(ctx context.Context, poolPath, caseID string) error {
	if _, err := os.Stat(poolPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect eval object pool: %w", err)
	}
	if err := git.ValidateBareRepository(ctx, poolPath); err != nil {
		return fmt.Errorf("validate eval object pool: %w", err)
	}
	// Git refuses to delete a ref it cannot parse, and any damaged pin fails
	// the gc below, so repair first or retention stays wedged.
	if err := repairObjectPool(ctx, poolPath); err != nil {
		return err
	}
	prefix := caseRefPrefix(caseID)
	var failures []string
	for _, name := range caseRefNames() {
		if _, err := git.Run(ctx, poolPath, "update-ref", "-d", prefix+name); err != nil {
			failures = append(failures, name)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("release case %q objects: %s", caseID, strings.Join(failures, ", "))
	}
	refs, err := git.Run(ctx, poolPath, "for-each-ref", "--format=%(refname)", caseRefNamespace)
	if err != nil {
		return fmt.Errorf("inspect remaining eval pool pins: %w", err)
	}
	if strings.TrimSpace(refs) == "" {
		if err := os.RemoveAll(poolPath); err != nil {
			return fmt.Errorf("remove empty eval object pool: %w", err)
		}
		return nil
	}
	return reclaimPoolSpace(ctx, poolPath)
}

// reclaimPoolSpace runs Git's automatic gc on a pool. --auto respects Git's own
// maintenance thresholds, so it stays cheap: it repacks when the pool has
// actually grown instead of paying a full repack on every call. Git detaches
// it, so a failure surfaces only as the pool's gc.log, which repairObjectPool
// clears once it has removed the damaged pin that caused it.
func reclaimPoolSpace(ctx context.Context, poolPath string) error {
	if _, err := git.Run(ctx, poolPath, "gc", "--auto", "--quiet"); err != nil {
		return fmt.Errorf("reclaim eval pool space: %w", err)
	}
	return nil
}

// repairObjectPool releases every case whose pins Git can no longer use, so
// one damaged case cannot wedge a whole pool.
//
// A pin is damaged when its loose ref file cannot be parsed (a zero-byte file
// is what an unclean shutdown leaves when refs are not fsynced) or when it
// names an object the pool does not have or cannot read. Either one fails every later fetch
// into the pool at its connectivity check and every repack, and Git cannot
// delete an unparseable ref itself, so nothing else would ever clear it. A
// case missing any one pin cannot be restored, so all of that case's pins are
// released rather than leaving the rest to keep dead objects alive.
//
// Callers hold the corpus lock, which makes them the only writer except for a
// detached gc a previous call started, so only files named like loose objects
// are touched and Git's in-flight temporary files are left alone.
//
// The same shutdown can leave a zero-byte loose object, which fails every
// fetch that needs that object and every gc even once no ref names it, so
// those files are removed first. A valid loose object is never empty. Such an
// object can sit anywhere in a case's history, not only at a pin, so after
// removing one every remaining case's history is walked too.
//
// When anything was released or removed, the pool's gc.log is removed too. A
// detached gc that died on the damage wrote it, and Git skips automatic gc
// while a recent one is present; the cause it reports is gone.
func repairObjectPool(ctx context.Context, pool string) error {
	emptied, err := removeEmptyLooseObjects(pool)
	if err != nil {
		return err
	}
	// Walk the loose ref files before asking Git which refs it can read: a ref
	// written in between is then on Git's list and is never mistaken for an
	// unreadable one.
	looseRefs, err := looseCaseRefs(pool)
	if err != nil {
		return err
	}
	listing, err := git.Run(ctx, pool, "for-each-ref", "--format=%(objectname) %(refname)", caseRefNamespace)
	if err != nil {
		return fmt.Errorf("inspect eval pool pins: %w", err)
	}
	readable := map[string]string{}
	var oids []string
	for _, line := range strings.Split(listing, "\n") {
		oid, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		readable[ref] = oid
		oids = append(oids, oid)
	}

	damaged := map[string]bool{}
	var unreadable []string
	for ref, path := range looseRefs {
		if _, ok := readable[ref]; !ok {
			unreadable = append(unreadable, path)
			damaged[caseIDOfRef(ref)] = true
		}
	}
	if len(oids) > 0 {
		missing, err := missingObjects(ctx, pool, oids)
		if err != nil {
			return err
		}
		for ref, oid := range readable {
			if missing[oid] {
				damaged[caseIDOfRef(ref)] = true
			}
		}
	}
	if emptied {
		if err := markIncompleteHistories(ctx, pool, readable, damaged); err != nil {
			return err
		}
	}
	if len(damaged) == 0 && !emptied {
		return nil
	}

	for _, path := range unreadable {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove unreadable eval pool pin: %w", err)
		}
	}
	var deletes strings.Builder
	for ref := range readable {
		if damaged[caseIDOfRef(ref)] {
			deletes.WriteString("delete " + ref + "\n")
		}
	}
	if deletes.Len() > 0 {
		if _, err := git.RunWithInput(ctx, pool, deletes.String(), "update-ref", "--stdin"); err != nil {
			return fmt.Errorf("release damaged eval pool pins: %w", err)
		}
	}
	if err := os.Remove(filepath.Join(pool, "gc.log")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear stale eval pool gc.log: %w", err)
	}
	return nil
}

// markIncompleteHistories marks every case not already damaged whose pinned
// history lacks an object or cannot be walked. A repack walks the same
// history, so it fails on such a case just as on a missing pin, and a fetch
// stops at the existing pins and never re-sends what is gone.
func markIncompleteHistories(ctx context.Context, pool string, readable map[string]string, damaged map[string]bool) error {
	tips := map[string][]string{}
	for ref, oid := range readable {
		if id := caseIDOfRef(ref); !damaged[id] {
			tips[id] = append(tips[id], oid)
		}
	}
	for id, oids := range tips {
		out, err := git.Run(ctx, pool, append([]string{"rev-list", "--objects", "--missing=print"}, oids...)...)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			damaged[id] = true
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "?") {
				damaged[id] = true
				break
			}
		}
	}
	return nil
}

// removeEmptyLooseObjects deletes zero-byte files from the pool's loose object
// directories and reports whether it deleted any.
func removeEmptyLooseObjects(pool string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(pool, "objects", "[0-9a-f][0-9a-f]", "*"))
	if err != nil {
		return false, fmt.Errorf("inspect eval pool loose objects: %w", err)
	}
	removed := false
	for _, path := range paths {
		if !isLooseObjectName(filepath.Base(path)) {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("remove empty eval pool loose object: %w", err)
		}
		removed = true
	}
	return removed, nil
}

// isLooseObjectName reports whether name is the 38 (SHA-1) or 62 (SHA-256)
// hex characters that follow the two-character directory of a loose object.
func isLooseObjectName(name string) bool {
	if len(name) != 38 && len(name) != 62 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// looseCaseRefs maps each loose ref file under the case namespace to its path.
// Lock files are Git's in-flight writes, not refs.
func looseCaseRefs(pool string) (map[string]string, error) {
	root := filepath.Join(pool, filepath.FromSlash(strings.TrimSuffix(caseRefNamespace, "/")))
	refs := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || strings.HasSuffix(d.Name(), ".lock") {
			return nil
		}
		rel, err := filepath.Rel(pool, path)
		if err != nil {
			return err
		}
		refs[filepath.ToSlash(rel)] = path
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect eval pool pin files: %w", err)
	}
	return refs, nil
}

// missingObjects reports which of oids the pool does not contain or cannot
// read. Asking for the type makes Git open each object, so a corrupt loose
// object is reported missing instead of present.
func missingObjects(ctx context.Context, pool string, oids []string) (map[string]bool, error) {
	out, err := git.RunWithInput(ctx, pool, strings.Join(oids, "\n")+"\n", "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, fmt.Errorf("check eval pool pin objects: %w", err)
	}
	missing := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if oid, ok := strings.CutSuffix(strings.TrimSpace(line), " missing"); ok {
			missing[oid] = true
		}
	}
	return missing, nil
}

func caseIDOfRef(ref string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(ref, caseRefNamespace), "/")
	return id
}
