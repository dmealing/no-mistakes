package daemon

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// Package-manager store variables the daemon sets for a run (local fork patch).
//
// Every run works in a fresh worktree, so every JavaScript run installs a full
// node_modules. pnpm and bun install by hardlinking files out of a shared
// store, but a hardlink cannot cross filesystems: with the store on another
// filesystem than the run worktree (pnpm's default under the user's data
// directory, bun's under ~/.bun), both fall back to copying every file, which
// rewrote hundreds of megabytes to disk per run. Pointing them at a store on
// the worktree's own filesystem turns each install into hardlinks. The store
// itself is shared and persistent - it is a cache, written once per package
// version - so only the per-run copies go away.
//
// npm is not covered: it always extracts its cache into node_modules, so no
// store location makes its install a hardlink.
const (
	pnpmStoreEnv = "npm_config_store_dir"
	bunCacheEnv  = "BUN_INSTALL_CACHE_DIR"
)

// packageStoresDir is where the shared stores live: inside NM_HOME, beside the
// default worktree tree, so a default-placement run shares its filesystem.
func packageStoresDir(p *paths.Paths) string {
	return filepath.Join(p.Root(), "package-stores")
}

// packageStoreEnvironment returns the store variables for a run whose worktree
// is workDir, applied to every agent and step subprocess of that run.
//
// It never overrides a store someone chose on purpose: a variable already in
// the daemon's environment (the caller's choice) or a store the repository's
// own configuration names (.npmrc store-dir, pnpm-workspace.yaml storeDir,
// bunfig.toml [install.cache] dir) leaves that manager alone, because an
// environment variable outranks a project's own file for both managers. A user
// level ~/.npmrc store-dir is deliberately not such a choice: it is the
// machine-wide store whose filesystem is the problem.
//
// A worktree on another filesystem than NM_HOME (a worktree_roots placement on
// another disk) gets nothing, which is today's behaviour rather than a store
// that would make its installs cross-filesystem copies again.
func packageStoreEnvironment(p *paths.Paths, workDir string) runenv.Overlay {
	if !sameFilesystem(p.Root(), workDir) {
		return runenv.Overlay{}
	}
	set := map[string]string{}
	stores := packageStoresDir(p)
	if !envSet(pnpmStoreEnv) && !envSet("pnpm_config_store_dir") && !projectSetsPnpmStore(workDir) {
		set[pnpmStoreEnv] = filepath.Join(stores, "pnpm")
	}
	if !envSet(bunCacheEnv) && !projectSetsBunCache(workDir) {
		set[bunCacheEnv] = filepath.Join(stores, "bun")
	}
	if len(set) == 0 {
		return runenv.Overlay{}
	}
	return runenv.Overlay{Set: set}
}

// withPackageStores layers the store variables under base, so anything base
// sets (a forge profile's environment) wins over them.
func withPackageStores(base, stores runenv.Overlay) runenv.Overlay {
	if stores.Empty() {
		return base
	}
	merged := base.Clone()
	if merged.Set == nil {
		merged.Set = map[string]string{}
	}
	for key, value := range stores.Set {
		if _, taken := merged.Set[key]; taken {
			continue
		}
		merged.Set[key] = value
	}
	return merged
}

// envSet reports whether the daemon's environment names key in any letter
// case: npm-style config variables are read case-insensitively.
func envSet(key string) bool {
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.EqualFold(name, key) && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// projectSetsPnpmStore reports whether the repository's own pnpm configuration
// names a store.
func projectSetsPnpmStore(workDir string) bool {
	found := false
	scanLines(filepath.Join(workDir, ".npmrc"), func(line string) bool {
		key, _, ok := strings.Cut(line, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		found = ok && (key == "store-dir" || key == "store_dir")
		return found
	})
	if found {
		return true
	}
	scanLines(filepath.Join(workDir, "pnpm-workspace.yaml"), func(line string) bool {
		// Top-level key only: an indented storeDir belongs to something else.
		found = strings.HasPrefix(line, "storeDir:")
		return found
	})
	return found
}

// projectSetsBunCache reports whether the repository's bunfig.toml names an
// install cache directory, as an [install.cache] dir, or an inline cache table
// or dotted key under [install].
func projectSetsBunCache(workDir string) bool {
	section := ""
	found := false
	scanLines(filepath.Join(workDir, "bunfig.toml"), func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			section = strings.Trim(strings.ReplaceAll(trimmed, " ", ""), "[]")
			return false
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return false
		}
		key = strings.ReplaceAll(strings.TrimSpace(key), " ", "")
		switch {
		case section == "install.cache" && key == "dir":
			found = true
		case section == "install" && (key == "cache.dir" || (key == "cache" && strings.Contains(value, "dir"))):
			found = true
		}
		return found
	})
	return found
}

// scanLines calls visit with each non-comment line of path until visit returns
// true. A missing or unreadable file has no lines.
func scanLines(path string, visit func(line string) bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if visit(line) {
			return
		}
	}
}
