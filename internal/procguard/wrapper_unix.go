//go:build unix

package procguard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// HandleArgs runs a wrapper when args (os.Args[1:]) re-enter the binary as
// one, reporting handled=false otherwise.
func HandleArgs(args []string) (handled bool, exitCode int) {
	if len(args) == 0 || args[0] != Command {
		return false, 0
	}
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "no-mistakes process guard: missing guard directory or tool")
		return true, 2
	}
	g := &guard{
		dir:    args[1],
		token:  os.Getenv(MarkerEnv),
		self:   os.Getpid(),
		stdout: os.Stdout,
		stderr: os.Stderr,
		owned:  processHasMarker,
		parent: parentPID,
		start:  startTime,
		signal: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		run:    runReal,
	}
	return true, g.main(args[2], args[3:])
}

// guard holds one wrapper invocation; its process seams are fields so tests
// can drive it without a process table.
type guard struct {
	dir    string
	token  string
	self   int
	stdout io.Writer
	stderr io.Writer
	owned  func(pid int, marker string) bool
	parent func(pid int) int
	start  func(pid int) uint64
	signal func(pid int, sig syscall.Signal) error
	run    func(path string, args []string, stderr io.Writer) ([]byte, int, error)
}

func (g *guard) main(tool string, args []string) int {
	switch tool {
	case "pgrep":
		return g.pgrep(args)
	case "pidof":
		return g.pidof(args)
	case "pkill":
		return g.pkill(args)
	case "killall":
		return g.killall(args)
	default:
		fmt.Fprintf(g.stderr, "no-mistakes process guard: unknown tool %q\n", tool)
		return 2
	}
}

// mine reports whether pid belongs to this run.
func (g *guard) mine(pid int) bool {
	if g.token == "" || pid == g.self {
		return false
	}
	return g.owned(pid, MarkerEnv+"="+g.token)
}

// realTool finds the real command on PATH, skipping the guard directory and
// any other guard's wrapper. Two guards can share one PATH: a gate run's Test
// agent runs this package's own tests, which install a second guard in front
// of the gate's. Skipping only its own directory made each guard resolve the
// real tool to the other's wrapper, and the two re-entered each other without
// end until the host ran out of memory.
func (g *guard) realTool(tool string) (string, error) {
	guardDir := filepath.Clean(g.dir)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" || filepath.Clean(entry) == guardDir {
			continue
		}
		candidate := filepath.Join(entry, tool)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		if isGuardWrapper(candidate) {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s: not found on PATH", tool)
}

// list runs the real tool and returns its stdout and exit code; a tool that
// cannot be found exits 2, like a usage error.
func (g *guard) list(tool string, args []string) ([]byte, int) {
	path, err := g.realTool(tool)
	if err != nil {
		fmt.Fprintf(g.stderr, "%s: %v\n", tool, err)
		return nil, 2
	}
	out, code, err := g.run(path, args, g.stderr)
	if err != nil {
		fmt.Fprintf(g.stderr, "%s: %v\n", tool, err)
		return nil, 2
	}
	return out, code
}

func runReal(path string, args []string, stderr io.Writer) ([]byte, int, error) {
	cmd := exec.Command(path, args...)
	cmd.Stderr = stderr
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out, exitErr.ExitCode(), nil
	}
	if err != nil {
		return nil, 0, err
	}
	return out, 0, nil
}

// pgrep prints the real pgrep's matches that belong to this run. Like pkill it
// never offers the wrapper's own ancestors: `kill $(pgrep -f pattern)` run
// from a shell whose command line holds the pattern would otherwise kill that
// shell.
func (g *guard) pgrep(args []string) int {
	rest, count, delim, pick := splitPgrepOutputFlags(args)
	out, code := g.list("pgrep", rest)
	if code >= 2 {
		return code
	}
	ancestors := g.ancestors()
	var kept []match
	hidden := 0
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		pid, ok := leadingPID(line)
		if ok && (pid == g.self || ancestors[pid]) {
			continue
		}
		if ok && g.mine(pid) {
			kept = append(kept, match{pid, line})
			continue
		}
		hidden++
	}
	g.noteHidden("pgrep", hidden)
	kept = g.pickOne(kept, pick)
	switch {
	case count:
		fmt.Fprintln(g.stdout, len(kept))
	case len(kept) > 0:
		lines := make([]string, len(kept))
		for i, m := range kept {
			lines[i] = m.line
		}
		fmt.Fprintln(g.stdout, strings.Join(lines, delim))
	}
	if len(kept) == 0 {
		return 1
	}
	return 0
}

// pidof prints the real pidof's matches that belong to this run, never the
// wrapper's own ancestors.
func (g *guard) pidof(args []string) int {
	sep := " "
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "-S" || a == "--separator") && i+1 < len(args):
			sep = args[i+1]
			i++
		case strings.HasPrefix(a, "--separator="):
			sep = strings.TrimPrefix(a, "--separator=")
		case strings.HasPrefix(a, "-S") && len(a) > 2:
			sep = a[2:]
		}
	}
	rest, single := splitPidofSingleShot(args)
	out, code := g.list("pidof", rest)
	if code >= 2 {
		return code
	}
	var kept []string
	hidden := 0
	text := strings.TrimRight(string(out), "\n")
	var fields []string
	if sep == " " {
		fields = strings.Fields(text)
	} else if text != "" {
		fields = strings.Split(text, sep)
	}
	ancestors := g.ancestors()
	for _, field := range fields {
		pid, err := strconv.Atoi(strings.TrimSpace(field))
		if err == nil && (pid == g.self || ancestors[pid]) {
			continue
		}
		if err == nil && g.mine(pid) {
			kept = append(kept, strconv.Itoa(pid))
			continue
		}
		hidden++
	}
	g.noteHidden("pidof", hidden)
	if len(kept) == 0 {
		return 1
	}
	if single {
		kept = kept[:1]
	}
	fmt.Fprintln(g.stdout, strings.Join(kept, sep))
	return 0
}

// pkill signals the real pgrep's matches that belong to this run, never the
// wrapper's own ancestors.
func (g *guard) pkill(args []string) int {
	sig, echo, count, rest, err := splitPkillFlags(args)
	if err != nil {
		fmt.Fprintf(g.stderr, "pkill: %v\n", err)
		return 2
	}
	rest, _, _, pick := splitPgrepOutputFlags(rest)
	out, code := g.list("pgrep", rest)
	if code >= 2 {
		return code
	}
	ancestors := g.ancestors()
	var spared []int
	var targets []match
	signalled := 0
	for _, line := range strings.Split(string(out), "\n") {
		pid, ok := leadingPID(line)
		if !ok || pid == g.self || ancestors[pid] {
			continue
		}
		if !g.mine(pid) {
			spared = append(spared, pid)
			continue
		}
		targets = append(targets, match{pid, line})
	}
	for _, target := range g.pickOne(targets, pick) {
		pid := target.pid
		if err := g.signal(pid, sig); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				fmt.Fprintf(g.stderr, "pkill: killing pid %d failed: %v\n", pid, err)
			}
			continue
		}
		signalled++
		if echo {
			fmt.Fprintf(g.stdout, "killed (pid %d)\n", pid)
		}
	}
	if len(spared) > 0 {
		fmt.Fprintf(g.stderr, "pkill: no-mistakes process guard left %d matching process(es) alone because this run did not start them (pid %s). A gate agent may only signal processes it started; stop your own background work by its process group: kill -TERM -<pgid>.\n", len(spared), pidList(spared))
	}
	if count {
		fmt.Fprintln(g.stdout, signalled)
	}
	if signalled == 0 {
		return 1
	}
	return 0
}

// killall is refused; its informational forms still pass through.
func (g *guard) killall(args []string) int {
	informational := len(args) > 0
	for _, a := range args {
		switch a {
		case "-l", "--list", "-V", "--version", "-h", "--help":
		default:
			informational = false
		}
	}
	if informational {
		out, code := g.list("killall", args)
		_, _ = g.stdout.Write(out)
		return code
	}
	fmt.Fprintln(g.stderr, "killall: refused by the no-mistakes process guard: it matches processes by name, which reaches other runs on this machine. Stop processes you started by PID or process group (kill -TERM -<pgid>); pkill here reaches only this run's processes.")
	return 1
}

func (g *guard) noteHidden(tool string, hidden int) {
	if hidden > 0 {
		fmt.Fprintf(g.stderr, "%s: no-mistakes process guard hid %d matching process(es) that this run did not start.\n", tool, hidden)
	}
}

// ancestors returns the wrapper's parent chain, the shell and agent that ran
// it among them.
func (g *guard) ancestors() map[int]bool {
	found := map[int]bool{}
	for pid := g.parent(g.self); pid > 1 && !found[pid]; pid = g.parent(pid) {
		found[pid] = true
	}
	return found
}

// match is one of this run's processes the real tool listed, with its line.
type match struct {
	pid  int
	line string
}

// pickOne applies pgrep's --newest ('n') or --oldest ('o') to this run's own
// matches. The real tool picks among every process on the machine, so a
// sibling run's newer process would win and the agent's own match would
// vanish.
func (g *guard) pickOne(matches []match, pick byte) []match {
	if pick == 0 || len(matches) < 2 {
		return matches
	}
	best := matches[0]
	for _, m := range matches[1:] {
		bs, ms := g.start(best.pid), g.start(m.pid)
		later := ms > bs || (ms == bs && m.pid > best.pid)
		if (pick == 'n') == later {
			best = m
		}
	}
	return []match{best}
}

func leadingPID(line string) (int, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(fields[0])
	return pid, err == nil && pid > 0
}

func pidList(pids []int) string {
	sort.Ints(pids)
	const shown = 10
	parts := make([]string, 0, shown+1)
	for i, pid := range pids {
		if i == shown {
			parts = append(parts, "...")
			break
		}
		parts = append(parts, strconv.Itoa(pid))
	}
	return strings.Join(parts, ", ")
}

// pgrepValueFlags are pgrep's short options that take a value.
const pgrepValueFlags = "dgGPstuUFr"

// splitPgrepOutputFlags removes the count, delimiter, newest and oldest
// options, which the guard applies itself to the filtered result, and returns
// the selection arguments for the real pgrep. pick is 'n', 'o' or 0.
func splitPgrepOutputFlags(args []string) (rest []string, count bool, delim string, pick byte) {
	delim = "\n"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(rest, args[i:]...), count, delim, pick
		case a == "--count":
			count = true
		case a == "--newest":
			pick = 'n'
		case a == "--oldest":
			pick = 'o'
		case a == "--delimiter" && i+1 < len(args):
			delim = args[i+1]
			i++
		case strings.HasPrefix(a, "--delimiter="):
			delim = strings.TrimPrefix(a, "--delimiter=")
		case strings.HasPrefix(a, "--"):
			rest = append(rest, a)
		case len(a) > 1 && a[0] == '-':
			kept := []byte{'-'}
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 'c' {
					count = true
					continue
				}
				if c == 'n' || c == 'o' {
					pick = c
					continue
				}
				if c == 'd' {
					if j+1 < len(a) {
						delim = a[j+1:]
					} else if i+1 < len(args) {
						delim = args[i+1]
						i++
					}
					break
				}
				kept = append(kept, c)
				if strings.IndexByte(pgrepValueFlags, c) >= 0 {
					kept = append(kept, a[j+1:]...)
					if j+1 == len(a) && i+1 < len(args) {
						rest = append(rest, string(kept))
						kept = nil
						rest = append(rest, args[i+1])
						i++
					}
					break
				}
			}
			if len(kept) > 1 {
				rest = append(rest, string(kept))
			}
		default:
			rest = append(rest, a)
		}
	}
	return rest, count, delim, pick
}

// pidofValueFlags are pidof's short options that take a value.
const pidofValueFlags = "odS"

// splitPidofSingleShot removes -s/--single-shot, which the guard applies
// itself to the filtered result: the real pidof would return one process of
// the whole machine.
func splitPidofSingleShot(args []string) (rest []string, single bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(rest, args[i:]...), single
		case a == "--single-shot":
			single = true
		case strings.HasPrefix(a, "--") || len(a) < 2 || a[0] != '-':
			rest = append(rest, a)
		default:
			kept := []byte{'-'}
			for j := 1; j < len(a); j++ {
				c := a[j]
				if c == 's' {
					single = true
					continue
				}
				kept = append(kept, c)
				if strings.IndexByte(pidofValueFlags, c) >= 0 {
					kept = append(kept, a[j+1:]...)
					if j+1 == len(a) && i+1 < len(args) {
						rest = append(rest, string(kept))
						kept = nil
						rest = append(rest, args[i+1])
						i++
					}
					break
				}
			}
			if len(kept) > 1 {
				rest = append(rest, string(kept))
			}
		}
	}
	return rest, single
}

// splitPkillFlags separates pkill's own options (signal, echo, count) from
// the selection arguments it shares with pgrep.
func splitPkillFlags(args []string) (sig syscall.Signal, echo, count bool, rest []string, err error) {
	sig = syscall.SIGTERM
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return sig, echo, count, append(rest, args[i:]...), nil
		case a == "--signal":
			if i+1 >= len(args) {
				return sig, echo, count, nil, errors.New("option --signal requires an argument")
			}
			if sig, err = parseSignal(args[i+1]); err != nil {
				return sig, echo, count, nil, err
			}
			i++
		case strings.HasPrefix(a, "--signal="):
			if sig, err = parseSignal(strings.TrimPrefix(a, "--signal=")); err != nil {
				return sig, echo, count, nil, err
			}
		case a == "-e" || a == "--echo":
			echo = true
		case a == "-c" || a == "--count":
			count = true
		case len(a) > 1 && a[0] == '-' && isSignalArg(a[1:]):
			sig, _ = parseSignal(a[1:])
		default:
			rest = append(rest, a)
		}
	}
	return sig, echo, count, rest, nil
}

// isSignalArg reports whether a dash option is a signal (-9, -KILL,
// -SIGKILL) rather than a selection option such as -P, -u or -f: pkill only
// takes the numeric and upper-case forms there.
func isSignalArg(s string) bool {
	if s == "" || s != strings.ToUpper(s) {
		return false
	}
	_, err := parseSignal(s)
	return err == nil
}

// parseSignal reads a signal number or name, with or without SIG, in any case.
func parseSignal(s string) (syscall.Signal, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 || n > 64 {
			return 0, fmt.Errorf("invalid signal %q", s)
		}
		return syscall.Signal(n), nil
	}
	name := strings.ToUpper(s)
	if !strings.HasPrefix(name, "SIG") {
		name = "SIG" + name
	}
	if n := unix.SignalNum(name); n != 0 {
		return n, nil
	}
	return 0, fmt.Errorf("invalid signal %q", s)
}

// isGuardWrapper reports whether path is a wrapper script some guard
// installed, recognised by the header Install writes.
func isGuardWrapper(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(scriptHeader))
	n, _ := io.ReadFull(f, head)
	return string(head[:n]) == scriptHeader
}
