package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/testslots"
)

func runSlotCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"slot"}, args...))
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestSlotRunPassesThroughExitCodeAndOutput(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	t.Setenv(slotHeldEnv, "")
	out, _, err := runSlotCmd(t, "run", "--", "sh", "-c", "echo hi; exit 3")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("err = %v, want exit code 3", err)
	}
	if strings.TrimSpace(out) != "hi" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestSlotRunWaitsPrintsOneLineAndReleases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	t.Setenv(slotHeldEnv, "")
	p := paths.WithRoot(home)
	pool := testslots.NewPool(p.TestSlotsDir(), 2)
	a, _, _ := pool.Acquire(context.Background(), "run a (x)", nil)
	b, _, _ := pool.Acquire(context.Background(), "run b (y)", nil)
	defer a.Release()
	defer b.Release()

	type res struct {
		stderr string
		err    error
	}
	done := make(chan res, 1)
	go func() {
		_, se, err := runSlotCmd(t, "run", "true")
		done <- res{se, err}
	}()
	select {
	case <-done:
		t.Fatal("slot run finished while the pool was full")
	case <-time.After(600 * time.Millisecond):
	}
	a.Release()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n := strings.Count(r.stderr, "\n"); n != 1 || !strings.Contains(r.stderr, "waiting for a test slot") || !strings.Contains(r.stderr, "run b (y)") {
		t.Fatalf("stderr = %q, want one waiting line naming a holder", r.stderr)
	}
	if got := pool.Holders(); len(got) != 1 {
		t.Fatalf("holders after slot run = %v, want only b", got)
	}
}

func TestSlotRunInsideHeldSlotDoesNotQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	t.Setenv(slotHeldEnv, "1")
	pool := testslots.NewPool(paths.WithRoot(home).TestSlotsDir(), 2)
	a, _, _ := pool.Acquire(context.Background(), "a", nil)
	b, _, _ := pool.Acquire(context.Background(), "b", nil)
	defer a.Release()
	defer b.Release()
	if _, _, err := runSlotCmd(t, "run", "true"); err != nil {
		t.Fatal(err)
	}
}

func TestSlotStatusListsHolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("NM_HOME", home)
	pool := testslots.NewPool(paths.WithRoot(home).TestSlotsDir(), 2)
	a, _, _ := pool.Acquire(context.Background(), "run a (x)", nil)
	defer a.Release()
	out, _, err := runSlotCmd(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "test_slots: 2, in use: 1") || !strings.Contains(out, "run a (x)") {
		t.Fatalf("status = %q", out)
	}
}
