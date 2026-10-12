package testslots

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestPool(t *testing.T, size int) *Pool {
	t.Helper()
	p := NewPool(t.TempDir(), size)
	p.PollInterval = 5 * time.Millisecond
	return p
}

func TestAcquireUpToSizeThenWait(t *testing.T) {
	p := newTestPool(t, 2)
	ctx := context.Background()
	a, aWaited, err := p.Acquire(ctx, "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, bWaited, err := p.Acquire(ctx, "b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if aWaited != 0 || bWaited != 0 {
		t.Fatalf("free slots reported a wait: %v, %v, want 0", aWaited, bWaited)
	}
	if got := len(p.Holders()); got != 2 {
		t.Fatalf("holders = %d, want 2", got)
	}

	type result struct {
		lease  *Lease
		waited time.Duration
		err    error
	}
	done := make(chan result, 1)
	var waits [][]Holder
	waitSeen := make(chan struct{}, 1)
	go func() {
		l, w, err := p.Acquire(ctx, "c", func(h []Holder, _ bool) {
			waits = append(waits, h)
			select {
			case waitSeen <- struct{}{}:
			default:
			}
		})
		done <- result{l, w, err}
	}()

	select {
	case <-waitSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never reported waiting")
	}
	select {
	case <-done:
		t.Fatal("third acquire returned while both slots were held")
	case <-time.After(50 * time.Millisecond):
	}
	if len(waits[0]) != 2 || !strings.Contains(DescribeHolders(waits[0]), "a pid") {
		t.Fatalf("wait callback holders = %v", waits[0])
	}

	a.Release()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.waited <= 0 {
		t.Fatalf("waited = %v, want > 0", r.waited)
	}
	r.lease.Release()
	b.Release()
	if got := len(p.Holders()); got != 0 {
		t.Fatalf("holders after release = %d, want 0", got)
	}
}

func TestReleaseIsIdempotentAndNilSafe(t *testing.T) {
	p := newTestPool(t, 1)
	l, _, err := p.Acquire(context.Background(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	l.Release()
	(*Lease)(nil).Release()
	l2, _, err := p.Acquire(context.Background(), "y", nil)
	if err != nil {
		t.Fatal(err)
	}
	l2.Release()
}

func TestUnlimitedPoolNeverBlocks(t *testing.T) {
	p := newTestPool(t, 0)
	for i := 0; i < 5; i++ {
		l, waited, err := p.Acquire(context.Background(), "x", nil)
		if err != nil || waited != 0 {
			t.Fatalf("acquire %d: waited=%v err=%v", i, waited, err)
		}
		defer l.Release()
	}
	if _, err := os.Stat(p.Dir + "/slot-0.lock"); err == nil {
		t.Fatal("unlimited pool touched the slot directory")
	}
}

func TestAcquireWaitHonorsContext(t *testing.T) {
	p := newTestPool(t, 1)
	held, _, _ := p.Acquire(context.Background(), "holder", nil)
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := p.Acquire(ctx, "waiter", nil); err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// TestDeadHolderFreesSlot kills a real process that holds a slot and checks
// the slot becomes available with no cleanup step.
func TestDeadHolderFreesSlot(t *testing.T) {
	if os.Getenv("TESTSLOTS_HOLD_DIR") != "" {
		p := NewPool(os.Getenv("TESTSLOTS_HOLD_DIR"), 1)
		if _, _, err := p.Acquire(context.Background(), "doomed holder", nil); err != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("held\n")
		select {}
	}

	p := newTestPool(t, 1)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeadHolderFreesSlot$")
	cmd.Env = append(os.Environ(), "TESTSLOTS_HOLD_DIR="+p.Dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	buf := make([]byte, 16)
	if n, _ := stdout.Read(buf); !strings.HasPrefix(string(buf[:n]), "held") {
		t.Fatalf("holder did not report held: %q", buf[:n])
	}
	holders := p.Holders()
	if len(holders) != 1 || holders[0].Label != "doomed holder" || holders[0].PID != cmd.Process.Pid {
		t.Fatalf("holders = %+v", holders)
	}

	_ = cmd.Process.Kill() // SIGKILL: no deferred cleanup runs.
	_, _ = cmd.Process.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, _, err := p.Acquire(ctx, "survivor", nil)
	if err != nil {
		t.Fatalf("slot stranded by dead holder: %v", err)
	}
	l.Release()
}

// Only "another holder has it" may read as a busy slot. Any other lock failure
// must surface as an error: read as busy, it would look like a pool that is
// full forever.
func TestTryLockFileSeparatesAHeldSlotFromALockFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot-0.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if locked, err := tryLockFile(holder); !locked || err != nil {
		t.Fatalf("free slot: locked=%v err=%v, want true, nil", locked, err)
	}

	contender, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if locked, err := tryLockFile(contender); locked || err != nil {
		t.Fatalf("held slot: locked=%v err=%v, want false, nil", locked, err)
	}

	// A closed file cannot be locked for a reason that is not a live holder.
	contender.Close()
	if locked, err := tryLockFile(contender); locked || err == nil {
		t.Fatalf("unlockable file: locked=%v err=%v, want false and an error", locked, err)
	}
}

// The slot record names a holder's program and working directory, so only the
// owner may read it, including a file an earlier build left world-readable.
func TestSlotRecordIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	p := newTestPool(t, 2)
	if err := os.WriteFile(p.slotPath(0), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.slotPath(0), 0o644); err != nil {
		t.Fatal(err)
	}
	a, _, err := p.Acquire(context.Background(), "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, _, err := p.Acquire(context.Background(), "b", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Release()
	for i := 0; i < 2; i++ {
		info, err := os.Stat(p.slotPath(i))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("slot %d record mode = %o, want 600", i, perm)
		}
	}
}
