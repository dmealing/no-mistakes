//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testslots"
)

// slotProbeScript is a command that reports the signals it receives and runs
// until told to stop. $0 is a scratch directory: it creates "started" once its
// traps are installed, appends to "signals", and exits when "stop" appears.
const slotProbeScript = `trap 'echo term >> "$0/signals"; exit 0' TERM
trap 'echo int >> "$0/signals"' INT
: > "$0/started"
while [ ! -e "$0/stop" ]; do sleep 0.05; done`

type slotRunResult struct {
	stderr string
	err    error
}

// startSlotProbe runs the probe script under `slot run` and returns once the
// script has installed its traps, which is after slot run holds its slot.
func startSlotProbe(t *testing.T, extraArgs ...string) (scratch string, done chan slotRunResult) {
	t.Helper()
	scratch = t.TempDir()
	done = make(chan slotRunResult, 1)
	args := append([]string{"run", "--", "sh", "-c", slotProbeScript, scratch}, extraArgs...)
	go func() {
		_, stderr, err := runSlotCmd(t, args...)
		done <- slotRunResult{stderr, err}
	}()
	waitForFile(t, filepath.Join(scratch, "started"))
	return scratch, done
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
	}
	t.Fatalf("%s never appeared", path)
}

func probeSignals(t *testing.T, scratch string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scratch, "signals"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func awaitSlotRun(t *testing.T, done chan slotRunResult) slotRunResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("slot run did not return")
		return slotRunResult{}
	}
}

// The holder label is copied into other repositories' step logs and the state
// database, so it may name the program and directory but never an argument.
func TestSlotRunLabelNamesTheProgramAndNeverItsArguments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	t.Setenv(testslots.HeldEnv, "")
	const secret = "TOKEN=hunter2-not-for-the-label"
	scratch, done := startSlotProbe(t, secret)

	pool := testslots.NewPool(paths.WithRoot(home).TestSlotsDir(), 2)
	holders := pool.Holders()
	if len(holders) != 1 {
		t.Fatalf("holders = %+v, want the one slot run", holders)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := "slot run: sh (in " + cwd + ")"; holders[0].Label != want {
		t.Fatalf("label = %q, want %q", holders[0].Label, want)
	}
	// The persisted slot record is what other processes read.
	lockFile := filepath.Join(paths.WithRoot(home).TestSlotsDir(), "slot-0.lock")
	record, err := os.ReadFile(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{secret, "hunter2", scratch, "trap"} {
		if strings.Contains(string(record), leaked) {
			t.Fatalf("slot record leaks command argument %q: %s", leaked, record)
		}
	}
	info, err := os.Stat(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("slot record mode = %o, want 600", perm)
	}

	if err := os.WriteFile(filepath.Join(scratch, "stop"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := awaitSlotRun(t, done); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestSlotRunTerminatesItsCommandAtMaxHoldAndReleasesTheSlot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	t.Setenv(testslots.HeldEnv, "")
	p := paths.WithRoot(home)
	if err := os.WriteFile(p.ConfigFile(), []byte("test_slots: 1\nslot_run_max_hold: \"1s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	scratch, done := startSlotProbe(t)
	r := awaitSlotRun(t, done)

	var ee *exitError
	if !errors.As(r.err, &ee) || ee.code == 0 {
		t.Fatalf("err = %v, want a non-zero exit", r.err)
	}
	msg := r.err.Error()
	if !strings.Contains(msg, "slot_run_max_hold") || !strings.Contains(msg, "1s") || strings.Contains(msg, "\n") {
		t.Fatalf("message = %q, want one line naming the limit and its value", msg)
	}
	if got := probeSignals(t, scratch); got != "term\n" {
		t.Fatalf("command saw signals %q, want exactly one SIGTERM", got)
	}
	// The only slot is free again: the next caller gets it without waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, _, err := testslots.NewPool(p.TestSlotsDir(), 1).Acquire(ctx, "next", nil)
	if err != nil {
		t.Fatalf("slot not released after the hold limit: %v", err)
	}
	lease.Release()
}

// A wait for a slot is never bounded by slot_run_max_hold: the clock starts
// at acquisition.
func TestSlotRunMaxHoldDoesNotBoundTheWait(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	t.Setenv(testslots.HeldEnv, "")
	p := paths.WithRoot(home)
	if err := os.WriteFile(p.ConfigFile(), []byte("test_slots: 1\nslot_run_max_hold: \"1s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, _, err := testslots.NewPool(p.TestSlotsDir(), 1).Acquire(context.Background(), "run other (feature)", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	done := make(chan slotRunResult, 1)
	go func() {
		_, stderr, err := runSlotCmd(t, "run", "--", "true")
		done <- slotRunResult{stderr, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("slot run gave up waiting after the hold limit: %v", r.err)
	case <-time.After(1500 * time.Millisecond):
	}
	held.Release()
	if r := awaitSlotRun(t, done); r.err != nil {
		t.Fatal(r.err)
	}
}

// A SIGTERM is delivered to slot run alone, so the command only stops if it is
// forwarded, and as the same signal.
func TestSlotRunForwardsSIGTERMAsSIGTERM(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	t.Setenv(testslots.HeldEnv, "")
	scratch, done := startSlotProbe(t)

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if r := awaitSlotRun(t, done); r.err != nil {
		t.Fatalf("command exits 0 from its TERM trap, got %v", r.err)
	}
	if got := probeSignals(t, scratch); got != "term\n" {
		t.Fatalf("command saw signals %q, want exactly one SIGTERM", got)
	}
}

// Ctrl-C reaches the command through the shared process group, so slot run
// must not send an interrupt of its own: a second one makes many test runners
// skip teardown. Here the interrupt is sent to slot run alone, so any
// interrupt the command sees was sent by slot run.
func TestSlotRunNeverSendsItsCommandAnInterrupt(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	t.Setenv(testslots.HeldEnv, "")
	scratch, done := startSlotProbe(t)

	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		t.Fatalf("slot run stopped waiting for its command on an interrupt: %v", r.err)
	case <-time.After(700 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(scratch, "stop"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := awaitSlotRun(t, done); r.err != nil {
		t.Fatal(r.err)
	}
	if got := probeSignals(t, scratch); got != "" {
		t.Fatalf("command saw signals %q, want none", got)
	}
}
