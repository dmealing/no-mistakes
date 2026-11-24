// Package procguard keeps a gate agent's process kills inside its own run
// (local fork patch).
//
// Several gate runs share one machine, and they run the same commands: two
// runs of a bun project both have `bun test` processes alive at once. A Test
// agent that stops "its" suite with `pkill -f "bun test"`, or with
// `kill $(pgrep -x bun)`, selects processes by name and so also kills the
// other run's suite, which then loses its results. That happened: one run's
// Test agent killed six `bun test --shard=N/6` processes of another run.
//
// The daemon therefore starts every agent of a run with two things in its
// environment: MarkerEnv set to the run's ID, which every process the agent
// starts inherits, and a guard directory first on PATH holding pkill, killall,
// pgrep and pidof wrappers that re-enter this binary (Command). The wrappers
// still select with the real tools, but act only on processes whose initial
// environment carries the same marker value:
//
//   - pgrep and pidof print only this run's processes and say on stderr how
//     many matches they hid, so `kill $(pgrep ...)` can only reach this run.
//   - pkill signals only this run's processes and names the processes it left
//     alone.
//   - None of them offers the wrapper's own ancestors: an agent's
//     `pkill -f pattern` otherwise matches the very shell that runs it.
//   - killall is refused outright: its exact-name matching and process-group
//     option have no narrower form worth reproducing, and pkill covers it.
//
// A process whose environment cannot be read, or that was started with a
// scrubbed environment, counts as not this run's: the guard errs toward
// sparing a process, and the agent can still stop its own work by PID or
// process group. Bash's builtin kill and `ps | grep` pipelines are out of a
// PATH wrapper's reach; the Test prompt's process rule covers those.
//
// The guard is installed only where a process's environment is readable
// (Linux /proc); elsewhere the daemon installs nothing and agents behave as
// before.
package procguard

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// MarkerEnv carries the run identity every process of a run inherits.
	MarkerEnv = "NO_MISTAKES_PROC_GUARD"

	// Command is the hidden argv[1] the wrapper scripts re-enter this binary
	// with: `<exe> __proc-guard <guard-dir> <tool> args...`.
	Command = "__proc-guard"
)

// Tools are the wrapped commands, one script each in the guard directory.
var Tools = []string{"pkill", "killall", "pgrep", "pidof"}

// Supported reports whether this platform can tell a run's processes apart.
func Supported() bool {
	return runtime.GOOS == "linux"
}

// Install writes the wrapper scripts into dir, each re-entering exe. A script
// whose bytes already match is left untouched.
func Install(dir, exe string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, tool := range Tools {
		path := filepath.Join(dir, tool)
		content := []byte(script(exe, dir, tool))
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, content) {
			if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o111 != 0 {
				continue
			}
		}
		tmp, err := os.CreateTemp(dir, "."+tool+"-*")
		if err != nil {
			return err
		}
		_, werr := tmp.Write(content)
		cerr := tmp.Close()
		if werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Chmod(tmp.Name(), 0o755)
		}
		if werr == nil {
			werr = os.Rename(tmp.Name(), path)
		}
		if werr != nil {
			_ = os.Remove(tmp.Name())
			return werr
		}
	}
	return nil
}

// scriptHeader opens every wrapper script. realTool recognises a wrapper by
// it, so a wrapper never resolves the "real" tool to another guard's wrapper.
const scriptHeader = "#!/bin/sh\n# no-mistakes process guard: "

func script(exe, dir, tool string) string {
	return scriptHeader + tool + " reaches only this run's processes.\n" +
		"exec " + shellQuote(exe) + " " + Command + " " + shellQuote(dir) + " " + tool + " \"$@\"\n"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
