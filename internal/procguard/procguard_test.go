package procguard

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the installed wrapper scripts re-enter this test binary the
// way they re-enter the no-mistakes binary in production. Only the few
// end-to-end tests below spawn it.
func TestMain(m *testing.M) {
	if handled, code := HandleArgs(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// fakeGuard drives a guard against a scripted process table: owner maps a PID
// to its marker value, parents is the wrapper's ancestry, and pgrepOut is what
// the real pgrep printed.
type fakeGuard struct {
	*guard
	signalled []int
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	gotArgs   []string
}

func newFakeGuard(t *testing.T, owner map[int]string, parents map[int]int, pgrepOut string, pgrepCode int) *fakeGuard {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range Tools {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	f := &fakeGuard{}
	f.guard = &guard{
		dir:    t.TempDir(), // the guard's own directory, skipped on PATH
		token:  "run-own",
		self:   100,
		stdout: &f.stdout,
		stderr: &f.stderr,
		owned: func(pid int, marker string) bool {
			return MarkerEnv+"="+owner[pid] == marker
		},
		parent: func(pid int) int { return parents[pid] },
		signal: func(pid int, sig syscall.Signal) error {
			f.signalled = append(f.signalled, pid)
			return nil
		},
		run: func(path string, args []string, stderr io.Writer) ([]byte, int, error) {
			f.gotArgs = args
			return []byte(pgrepOut), pgrepCode, nil
		},
	}
	return f
}

func TestPkillSignalsOnlyThisRunsProcesses(t *testing.T) {
	owner := map[int]string{200: "run-own", 201: "run-own", 300: "run-sibling", 301: "run-sibling"}
	f := newFakeGuard(t, owner, nil, "200\n300\n201\n301\n", 0)

	code := f.main("pkill", []string{"-9", "-f", "bun test"})

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, f.stderr.String())
	}
	if !reflect.DeepEqual(f.signalled, []int{200, 201}) {
		t.Fatalf("signalled %v, want only this run's [200 201]", f.signalled)
	}
	if !reflect.DeepEqual(f.gotArgs, []string{"-f", "bun test"}) {
		t.Fatalf("real pgrep got %q, want the selection without the signal", f.gotArgs)
	}
	if !strings.Contains(f.stderr.String(), "left 2 matching process(es) alone") || !strings.Contains(f.stderr.String(), "300, 301") {
		t.Fatalf("stderr does not name the spared processes: %s", f.stderr.String())
	}
}

func TestPkillNeverSignalsItsOwnAncestors(t *testing.T) {
	// `pkill -f "bun test"` run from `bash -c 'pkill -f "bun test"; ...'`
	// matches that bash (and the agent above it), which carry this run's
	// marker too: real pkill killed the agent's own shell (exit 144).
	owner := map[int]string{90: "run-own", 80: "run-own", 200: "run-own"}
	parents := map[int]int{100: 90, 90: 80, 80: 1}
	f := newFakeGuard(t, owner, parents, "80\n90\n200\n", 0)

	if code := f.main("pkill", []string{"-f", "bun test"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !reflect.DeepEqual(f.signalled, []int{200}) {
		t.Fatalf("signalled %v, want [200] and never the ancestors 80, 90", f.signalled)
	}
}

func TestPkillWithOnlyForeignMatchesSignalsNothingAndFails(t *testing.T) {
	owner := map[int]string{300: "run-sibling", 400: ""}
	f := newFakeGuard(t, owner, nil, "300\n400\n", 0)

	if code := f.main("pkill", []string{"bun"}); code != 1 {
		t.Fatalf("exit = %d, want 1 (nothing of this run matched)", code)
	}
	if len(f.signalled) != 0 {
		t.Fatalf("signalled %v, want nothing", f.signalled)
	}
}

func TestPkillWithoutARunIdentitySignalsNothing(t *testing.T) {
	owner := map[int]string{200: ""}
	f := newFakeGuard(t, owner, nil, "200\n", 0)
	f.token = ""

	if code := f.main("pkill", []string{"bun"}); code != 1 || len(f.signalled) != 0 {
		t.Fatalf("exit = %d signalled %v, want 1 and nothing", code, f.signalled)
	}
}

func TestPkillPassesPgrepUsageErrorsThrough(t *testing.T) {
	f := newFakeGuard(t, nil, nil, "", 2)
	if code := f.main("pkill", []string{"--bogus"}); code != 2 {
		t.Fatalf("exit = %d, want pgrep's usage code 2", code)
	}
}

func TestPgrepPrintsOnlyThisRunsProcesses(t *testing.T) {
	owner := map[int]string{200: "run-own", 300: "run-sibling", 100: "run-own"}
	f := newFakeGuard(t, owner, nil, "100 no-mistakes\n200 bun\n300 bun\n", 0)

	if code := f.main("pgrep", []string{"-l", "bun"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if got := f.stdout.String(); got != "200 bun\n" {
		t.Fatalf("stdout = %q, want only this run's process (and never the wrapper itself)", got)
	}
	if !strings.Contains(f.stderr.String(), "hid 1 matching process(es)") {
		t.Fatalf("stderr does not say matches were hidden: %s", f.stderr.String())
	}
}

func TestPgrepAppliesCountAndDelimiterToTheFilteredResult(t *testing.T) {
	owner := map[int]string{200: "run-own", 201: "run-own", 300: "run-sibling"}

	f := newFakeGuard(t, owner, nil, "200\n300\n201\n", 0)
	if code := f.main("pgrep", []string{"-fc", "bun test"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.stdout.String() != "2\n" {
		t.Fatalf("count = %q, want 2", f.stdout.String())
	}
	if !reflect.DeepEqual(f.gotArgs, []string{"-f", "bun test"}) {
		t.Fatalf("real pgrep got %q, want -c removed", f.gotArgs)
	}

	f = newFakeGuard(t, owner, nil, "200\n300\n201\n", 0)
	if code := f.main("pgrep", []string{"-d", ",", "bun"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.stdout.String() != "200,201\n" {
		t.Fatalf("delimited = %q, want 200,201", f.stdout.String())
	}
}

func TestPgrepWithOnlyForeignMatchesFails(t *testing.T) {
	f := newFakeGuard(t, map[int]string{300: "run-sibling"}, nil, "300\n", 0)
	if code := f.main("pgrep", []string{"bun"}); code != 1 || f.stdout.Len() != 0 {
		t.Fatalf("exit = %d stdout %q, want 1 and nothing", code, f.stdout.String())
	}
}

func TestPidofPrintsOnlyThisRunsProcesses(t *testing.T) {
	owner := map[int]string{200: "run-own", 300: "run-sibling"}
	f := newFakeGuard(t, owner, nil, "300 200\n", 0)
	if code := f.main("pidof", []string{"bun"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if f.stdout.String() != "200\n" {
		t.Fatalf("stdout = %q, want 200", f.stdout.String())
	}
}

func TestKillallIsRefused(t *testing.T) {
	f := newFakeGuard(t, map[int]string{200: "run-own"}, nil, "200\n", 0)
	if code := f.main("killall", []string{"bun"}); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if f.gotArgs != nil || len(f.signalled) != 0 {
		t.Fatalf("killall ran or signalled: args %q signalled %v", f.gotArgs, f.signalled)
	}
	if !strings.Contains(f.stderr.String(), "refused by the no-mistakes process guard") {
		t.Fatalf("stderr = %s", f.stderr.String())
	}
}

func TestSplitPkillFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		sig  syscall.Signal
		rest []string
	}{
		{[]string{"-f", "bun test"}, syscall.SIGTERM, []string{"-f", "bun test"}},
		{[]string{"-9", "-f", "x"}, syscall.SIGKILL, []string{"-f", "x"}},
		{[]string{"-KILL", "x"}, syscall.SIGKILL, []string{"x"}},
		{[]string{"-SIGINT", "x"}, syscall.SIGINT, []string{"x"}},
		{[]string{"--signal", "hup", "x"}, syscall.SIGHUP, []string{"x"}},
		{[]string{"--signal=USR1", "x"}, syscall.SIGUSR1, []string{"x"}},
		{[]string{"-P", "123"}, syscall.SIGTERM, []string{"-P", "123"}},
		{[]string{"-u", "doug", "-x", "bun"}, syscall.SIGTERM, []string{"-u", "doug", "-x", "bun"}},
	} {
		sig, _, _, rest, err := splitPkillFlags(tc.args)
		if err != nil || sig != tc.sig || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("splitPkillFlags(%q) = %v %q %v, want %v %q", tc.args, sig, rest, err, tc.sig, tc.rest)
		}
	}
	if _, _, _, _, err := splitPkillFlags([]string{"--signal", "NOPE", "x"}); err == nil {
		t.Error("an unknown --signal must be a usage error")
	}
}

func TestInstallWritesExecutableWrappersAndLeavesUnchangedOnesAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	if err := Install(dir, "/opt/no mistakes/bin/no-mistakes"); err != nil {
		t.Fatal(err)
	}
	for _, tool := range Tools {
		info, err := os.Stat(filepath.Join(dir, tool))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable", tool)
		}
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pkill"))
	if !strings.Contains(string(data), "exec '/opt/no mistakes/bin/no-mistakes' "+Command+" '"+dir+"' pkill \"$@\"") {
		t.Fatalf("pkill wrapper = %s", data)
	}

	old := time.Now().Add(-time.Hour)
	for _, tool := range Tools {
		_ = os.Chtimes(filepath.Join(dir, tool), old, old)
	}
	if err := Install(dir, "/opt/no mistakes/bin/no-mistakes"); err != nil {
		t.Fatal(err)
	}
	for _, tool := range Tools {
		info, _ := os.Stat(filepath.Join(dir, tool))
		if !info.ModTime().Equal(old) {
			t.Fatalf("%s was rewritten although its bytes were unchanged", tool)
		}
	}
}
