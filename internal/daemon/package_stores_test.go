package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// clearPackageStoreEnv removes any store the developer's own environment sets,
// so each test decides what the caller chose.
func clearPackageStoreEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{pnpmStoreEnv, "NPM_CONFIG_STORE_DIR", "pnpm_config_store_dir", bunCacheEnv} {
		t.Setenv(key, "")
	}
}

// storeFixture is an NM_HOME and a run worktree on the same filesystem, which
// two temporary directories always are.
func storeFixture(t *testing.T) (*paths.Paths, string) {
	t.Helper()
	clearPackageStoreEnv(t)
	root := t.TempDir()
	workDir := filepath.Join(root, "worktrees", "repo", "run")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return paths.WithRoot(root), workDir
}

func writeProjectFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPackageStoreEnvironmentPointsBothManagersAtNMHome is the fix itself:
// with nothing chosen explicitly, pnpm and bun both get a store inside NM_HOME,
// on the run worktree's filesystem, so their installs hardlink.
func TestPackageStoreEnvironmentPointsBothManagersAtNMHome(t *testing.T) {
	p, workDir := storeFixture(t)

	got := packageStoreEnvironment(p, workDir)

	if want := filepath.Join(p.Root(), "package-stores", "pnpm"); got.Set[pnpmStoreEnv] != want {
		t.Errorf("%s = %q, want %q", pnpmStoreEnv, got.Set[pnpmStoreEnv], want)
	}
	if want := filepath.Join(p.Root(), "package-stores", "bun"); got.Set[bunCacheEnv] != want {
		t.Errorf("%s = %q, want %q", bunCacheEnv, got.Set[bunCacheEnv], want)
	}
	if len(got.Set) != 2 || len(got.Unset) != 0 {
		t.Errorf("overlay = %v, want exactly the two store variables", got)
	}
}

// TestPackageStoreEnvironmentNeverOverridesAnExplicitStore covers both owners
// of an explicit choice: the caller, through the daemon's own environment, and
// the repository, through its package-manager configuration. Either one leaves
// that manager alone and only that manager.
func TestPackageStoreEnvironmentNeverOverridesAnExplicitStore(t *testing.T) {
	cases := []struct {
		name     string
		env      map[string]string
		files    map[string]string
		wantPnpm bool
		wantBun  bool
	}{
		{name: "caller pnpm store", env: map[string]string{pnpmStoreEnv: "/mine"}, wantBun: true},
		{name: "caller pnpm store in upper case", env: map[string]string{"NPM_CONFIG_STORE_DIR": "/mine"}, wantBun: true},
		{name: "caller bun cache", env: map[string]string{bunCacheEnv: "/mine"}, wantPnpm: true},
		{name: "project .npmrc store-dir", files: map[string]string{".npmrc": "# shared\nauto-install-peers=true\nstore-dir = ../.pnpm-store\n"}, wantBun: true},
		{name: "project pnpm-workspace storeDir", files: map[string]string{"pnpm-workspace.yaml": "packages:\n  - apps/*\nstoreDir: ../.pnpm-store\n"}, wantBun: true},
		{name: "project bunfig cache dir", files: map[string]string{"bunfig.toml": "[install]\nexact = true\n\n[install.cache]\ndir = \".bun-cache\"\n"}, wantPnpm: true},
		{name: "project bunfig inline cache table", files: map[string]string{"bunfig.toml": "[install]\ncache = { dir = \".bun-cache\" }\n"}, wantPnpm: true},
		{name: "unrelated project settings", files: map[string]string{
			".npmrc":              "auto-install-peers=true\n",
			"pnpm-workspace.yaml": "packages:\n  - apps/*\ncatalog:\n  storeDir: not-top-level\n",
			"bunfig.toml":         "[install]\nexact = true\n# [install.cache]\n# dir = \"x\"\n",
		}, wantPnpm: true, wantBun: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, workDir := storeFixture(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			for name, content := range tc.files {
				writeProjectFile(t, workDir, name, content)
			}

			got := packageStoreEnvironment(p, workDir)

			if _, ok := got.Set[pnpmStoreEnv]; ok != tc.wantPnpm {
				t.Errorf("pnpm store set = %v, want %v (overlay %v)", ok, tc.wantPnpm, got)
			}
			if _, ok := got.Set[bunCacheEnv]; ok != tc.wantBun {
				t.Errorf("bun cache set = %v, want %v (overlay %v)", ok, tc.wantBun, got)
			}
		})
	}
}

// TestPackageStoreEnvironmentLeavesAnotherFilesystemAlone keeps today's
// behaviour for a worktree that is not on NM_HOME's filesystem (a worktree_roots
// placement on another disk): a store in NM_HOME would be one more
// cross-filesystem copy, not a hardlink.
func TestPackageStoreEnvironmentLeavesAnotherFilesystemAlone(t *testing.T) {
	p, _ := storeFixture(t)
	other := "/dev/shm"
	if info, err := os.Stat(other); err != nil || !info.IsDir() || sameFilesystem(p.Root(), other) {
		t.Skip("no second filesystem to place a worktree on")
	}
	if got := packageStoreEnvironment(p, other); !got.Empty() {
		t.Fatalf("overlay = %v for a worktree on another filesystem, want none", got)
	}
	if got := packageStoreEnvironment(p, filepath.Join(p.Root(), "missing")); !got.Empty() {
		t.Fatalf("overlay = %v for a worktree that cannot be inspected, want none", got)
	}
}

// TestWithPackageStoresKeepsTheForgeProfilesValues: a forge profile's
// environment is the run's own explicit choice, so it outranks the default
// store on a shared key and loses nothing it unsets.
func TestWithPackageStoresKeepsTheForgeProfilesValues(t *testing.T) {
	forge := runenv.Overlay{Set: map[string]string{"GH_CONFIG_DIR": "/profile", bunCacheEnv: "/profile/bun"}, Unset: []string{"GH_TOKEN"}}
	stores := runenv.Overlay{Set: map[string]string{pnpmStoreEnv: "/nm/pnpm", bunCacheEnv: "/nm/bun"}}

	got := withPackageStores(forge, stores)

	if got.Set[bunCacheEnv] != "/profile/bun" || got.Set[pnpmStoreEnv] != "/nm/pnpm" || got.Set["GH_CONFIG_DIR"] != "/profile" {
		t.Fatalf("merged overlay = %#v", got.Set)
	}
	if len(got.Unset) != 1 || got.Unset[0] != "GH_TOKEN" {
		t.Fatalf("merged unset = %v, want [GH_TOKEN]", got.Unset)
	}
	if forge.Set[pnpmStoreEnv] != "" {
		t.Fatal("withPackageStores mutated the forge overlay")
	}
	if empty := withPackageStores(runenv.Overlay{}, runenv.Overlay{}); !empty.Empty() {
		t.Fatalf("merging two empty overlays = %v, want empty", empty)
	}
}

// runEnvStep records the run environment its step context carried.
type runEnvStep struct {
	name types.StepName
	env  chan runenv.Overlay
}

func (s *runEnvStep) Name() types.StepName { return s.name }
func (s *runEnvStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	select {
	case s.env <- sctx.RunEnvironment:
	default:
	}
	return &pipeline.StepOutcome{}, nil
}

// TestRunStepsReceiveThePackageStores is the wiring end to end: a run the
// daemon starts hands its steps the stores for its own worktree.
func TestRunStepsReceiveThePackageStores(t *testing.T) {
	clearPackageStoreEnv(t)
	step := &runEnvStep{name: types.StepReview, env: make(chan runenv.Overlay, 1)}
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
	_, headSHA := setupTestGitRepo(t, p, d, "stores-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("stores-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}
	if run := waitForRunTerminalState(t, d, result.RunID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %q, want %q", run.Status, types.RunCompleted)
	}

	var got runenv.Overlay
	select {
	case got = <-step.env:
	default:
		t.Fatal("step never ran")
	}
	if want := filepath.Join(p.Root(), "package-stores", "pnpm"); got.Set[pnpmStoreEnv] != want {
		t.Errorf("step %s = %q, want %q", pnpmStoreEnv, got.Set[pnpmStoreEnv], want)
	}
	if want := filepath.Join(p.Root(), "package-stores", "bun"); got.Set[bunCacheEnv] != want {
		t.Errorf("step %s = %q, want %q", bunCacheEnv, got.Set[bunCacheEnv], want)
	}
}
