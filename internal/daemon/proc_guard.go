package daemon

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procguard"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// procGuardDir holds the pkill/killall/pgrep/pidof wrappers every run's
// agents find first on PATH (local fork patch). The scripts carry no run
// identity, so one directory serves every run.
func procGuardDir(p *paths.Paths) string {
	return filepath.Join(p.Root(), "proc-guard", "bin")
}

// procGuardEnvironment returns the overlay that keeps runID's agents from
// signalling another run's processes (see internal/procguard): the run marker
// every process the agent starts inherits, and the guard directory first on
// PATH. It is rebuilt whenever a run's agent is (start and recovery), so it
// survives a daemon restart.
//
// A platform that cannot read another process's environment gets nothing,
// and so does a failed install: that is the behaviour before the guard, logged
// rather than failing the run over a helper script.
func procGuardEnvironment(p *paths.Paths, runID string) runenv.Overlay {
	if !procguard.Supported() || runID == "" {
		return runenv.Overlay{}
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("process guard not installed: cannot resolve the no-mistakes executable", "run", runID, "error", err)
		return runenv.Overlay{}
	}
	dir := procGuardDir(p)
	if err := procguard.Install(dir, exe); err != nil {
		slog.Warn("process guard not installed", "run", runID, "dir", dir, "error", err)
		return runenv.Overlay{}
	}
	path := dir
	if current := os.Getenv("PATH"); current != "" {
		path += string(os.PathListSeparator) + current
	}
	return runenv.Overlay{Set: map[string]string{
		procguard.MarkerEnv: runID,
		"PATH":              path,
	}}
}

// withProcGuard layers the guard over base. The guard wins: an overlay that
// set PATH or the marker would otherwise switch it off.
func withProcGuard(base, guard runenv.Overlay) runenv.Overlay {
	if guard.Empty() {
		return base
	}
	merged := base.Clone()
	if merged.Set == nil {
		merged.Set = map[string]string{}
	}
	for key, value := range guard.Set {
		merged.Set[key] = value
	}
	return merged
}
