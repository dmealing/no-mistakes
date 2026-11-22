//go:build unix

package procguard

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func refused() error {
	return fmt.Errorf("fork/exec /usr/bin/pgrep: %w", syscall.EAGAIN)
}

func TestRetryForkRetriesARefusedForkUntilItSucceeds(t *testing.T) {
	var slept []time.Duration
	calls := 0
	out, code, err := retryFork(time.Second, func(d time.Duration) { slept = append(slept, d) }, func() ([]byte, int, error) {
		calls++
		if calls < 4 {
			return nil, 0, refused()
		}
		return []byte("42\n"), 0, nil
	})
	if err != nil || code != 0 || string(out) != "42\n" {
		t.Fatalf("retryFork = %q, %d, %v; want the fourth attempt's result", out, code, err)
	}
	if calls != 4 || len(slept) != 3 {
		t.Fatalf("calls=%d sleeps=%v, want 4 calls with 3 backoff sleeps", calls, slept)
	}
	for i := 1; i < len(slept); i++ {
		if slept[i] < slept[i-1] {
			t.Fatalf("backoff shrank: %v", slept)
		}
	}
}

func TestRetryForkGivesUpWithinItsBudgetAndKeepsTheCause(t *testing.T) {
	var total time.Duration
	calls := 0
	_, _, err := retryFork(300*time.Millisecond, func(d time.Duration) { total += d }, func() ([]byte, int, error) {
		calls++
		return nil, 0, refused()
	})
	if !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("err = %v, want it to keep EAGAIN as the cause", err)
	}
	if total != 300*time.Millisecond {
		t.Fatalf("slept %v in total, want exactly the 300ms budget", total)
	}
	if calls < 2 {
		t.Fatalf("calls = %d, want the fork retried", calls)
	}
}

func TestRetryForkDoesNotRetryOtherFailuresOrExitCodes(t *testing.T) {
	calls := 0
	sleep := func(time.Duration) { t.Fatal("slept for a failure that is not a refused fork") }
	notFound := &exec.Error{Name: "pgrep", Err: exec.ErrNotFound}
	_, _, err := retryFork(time.Second, sleep, func() ([]byte, int, error) {
		calls++
		return nil, 0, notFound
	})
	if err != notFound || calls != 1 {
		t.Fatalf("err=%v calls=%d, want the first failure returned untouched", err, calls)
	}
	_, code, err := retryFork(time.Second, sleep, func() ([]byte, int, error) {
		calls++
		return nil, 1, nil
	})
	if err != nil || code != 1 || calls != 2 {
		t.Fatalf("code=%d err=%v, want the tool's own exit code returned without a retry", code, err)
	}
}
