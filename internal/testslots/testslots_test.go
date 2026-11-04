package testslots

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// queue starts an Acquire that must wait and returns once it is in the queue,
// so a test can line waiters up in a known order.
func queue(t *testing.T, p *Pool, limit int, label string) <-chan *Lease {
	t.Helper()
	queued := make(chan struct{})
	var once sync.Once
	got := make(chan *Lease, 1)
	go func() {
		lease, _, err := p.Acquire(context.Background(), limit, label, func([]Holder, bool) {
			once.Do(func() { close(queued) })
		})
		if err != nil {
			t.Errorf("waiter %s: %v", label, err)
		}
		got <- lease
	}()
	select {
	case <-queued:
	case <-time.After(5 * time.Second):
		t.Fatalf("waiter %s never queued", label)
	}
	return got
}

func requireBlocked(t *testing.T, got <-chan *Lease, label string) {
	t.Helper()
	select {
	case <-got:
		t.Fatalf("waiter %s got a slot while none was free", label)
	case <-time.After(50 * time.Millisecond):
	}
}

func requireAdmitted(t *testing.T, got <-chan *Lease, label string) *Lease {
	t.Helper()
	select {
	case lease := <-got:
		return lease
	case <-time.After(5 * time.Second):
		t.Fatalf("waiter %s was not admitted", label)
		return nil
	}
}

// The cap is the point of the pool: with a limit of 2, no more than 2 of many
// concurrent callers may hold a slot at any instant, and all of them finish.
func TestPoolNeverAdmitsMoreThanTheLimit(t *testing.T) {
	const limit, callers = 2, 12
	p := NewPool()
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, _, err := p.Acquire(context.Background(), limit, "caller", nil)
			if err != nil {
				t.Error(err)
				return
			}
			n := active.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			active.Add(-1)
			lease.Release()
		}()
	}
	wg.Wait()
	if got := peak.Load(); got != limit {
		t.Fatalf("peak concurrent holders = %d, want exactly the limit %d", got, limit)
	}
}

func TestFreeSlotIsTakenAtOnceWithNoWait(t *testing.T) {
	p := NewPool()
	for _, label := range []string{"a", "b"} {
		lease, waited, err := p.Acquire(context.Background(), 2, label, func([]Holder, bool) {
			t.Errorf("%s: onWait called although a slot was free", label)
		})
		if err != nil || lease == nil || waited != 0 {
			t.Fatalf("%s: lease %v, waited %v, err %v", label, lease, waited, err)
		}
	}
}

// Waiters are admitted in the order they started waiting, one per released
// slot, so a long-queued run is never overtaken by a later one.
func TestWaitersAreAdmittedInArrivalOrder(t *testing.T) {
	p := NewPool()
	held, _, _ := p.Acquire(context.Background(), 1, "holder", nil)
	first := queue(t, p, 1, "first")
	second := queue(t, p, 1, "second")
	third := queue(t, p, 1, "third")

	held.Release()
	lease := requireAdmitted(t, first, "first")
	requireBlocked(t, second, "second")
	requireBlocked(t, third, "third")

	lease.Release()
	lease = requireAdmitted(t, second, "second")
	requireBlocked(t, third, "third")

	lease.Release()
	requireAdmitted(t, third, "third").Release()
}

// A new caller must not take a freed slot ahead of a run already waiting.
func TestNewCallerQueuesBehindExistingWaiters(t *testing.T) {
	p := NewPool()
	a, _, _ := p.Acquire(context.Background(), 2, "a", nil)
	b, _, _ := p.Acquire(context.Background(), 2, "b", nil)
	defer b.Release()
	// The waiter's own limit of 1 keeps it queued after a slot frees up.
	strict := queue(t, p, 1, "strict")
	a.Release()
	requireBlocked(t, strict, "strict")

	late := queue(t, p, 2, "late")
	requireBlocked(t, late, "late")
}

func TestReleaseIsIdempotentAndNilSafe(t *testing.T) {
	p := NewPool()
	a, _, _ := p.Acquire(context.Background(), 1, "a", nil)
	a.Release()
	a.Release()
	(*Lease)(nil).Release()

	// The second Release must not have freed a slot it no longer held.
	b, _, err := p.Acquire(context.Background(), 1, "b", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Release()
	requireBlocked(t, queue(t, p, 1, "c"), "c")
	b.Release()
}

func TestUnlimitedNeverBlocks(t *testing.T) {
	p := NewPool()
	held, _, _ := p.Acquire(context.Background(), 1, "holder", nil)
	defer held.Release()
	for _, pool := range []*Pool{p, nil} {
		lease, waited, err := pool.Acquire(context.Background(), 0, "unlimited", nil)
		if err != nil || waited != 0 {
			t.Fatalf("unlimited acquire: waited %v, err %v", waited, err)
		}
		lease.Release()
	}
	// An unlimited caller takes no slot: the limited pool is exactly as full.
	requireBlocked(t, queue(t, p, 1, "limited"), "limited")
}

func TestCancelledWaitReportsItsLengthAndLeavesTheQueue(t *testing.T) {
	p := NewPool()
	held, _, _ := p.Acquire(context.Background(), 1, "holder", nil)

	const timeout = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	lease, waited, err := p.Acquire(ctx, 1, "cancelled", nil)
	if err != context.DeadlineExceeded || lease != nil {
		t.Fatalf("lease %v, err %v, want no lease and deadline exceeded", lease, err)
	}
	// The caller subtracts the wait from its own clock, so a wait that ends
	// without a slot must still report how long it lasted.
	if waited < timeout {
		t.Fatalf("waited = %v for a wait that lasted the whole %v timeout", waited, timeout)
	}

	// The cancelled waiter is gone: the freed slot is free for the next caller.
	held.Release()
	next, waited, err := p.Acquire(context.Background(), 1, "next", nil)
	if err != nil || waited != 0 {
		t.Fatalf("slot kept by a cancelled waiter: waited %v, err %v", waited, err)
	}
	next.Release()
}

// A waiter that gives up at the head of the queue must not strand the ones
// behind it.
func TestCancelledHeadWaiterDoesNotBlockTheNext(t *testing.T) {
	p := NewPool()
	held, _, _ := p.Acquire(context.Background(), 1, "holder", nil)

	ctx, cancel := context.WithCancel(context.Background())
	headQueued := make(chan struct{})
	var once sync.Once
	headDone := make(chan error, 1)
	go func() {
		_, _, err := p.Acquire(ctx, 1, "head", func([]Holder, bool) { once.Do(func() { close(headQueued) }) })
		headDone <- err
	}()
	<-headQueued
	next := queue(t, p, 1, "next")

	cancel()
	if err := <-headDone; err != context.Canceled {
		t.Fatalf("head err = %v, want canceled", err)
	}
	requireBlocked(t, next, "next")
	held.Release()
	requireAdmitted(t, next, "next").Release()
}

// The queued line a run shows must name who holds the slots, be refreshed
// when that changes, and be repeated as a heartbeat when it does not.
func TestOnWaitNamesHoldersOnChangeAndHeartbeats(t *testing.T) {
	p := NewPool()
	p.notifyInterval = 20 * time.Millisecond
	held, _, _ := p.Acquire(context.Background(), 1, "run a (one)", nil)
	first := queue(t, p, 1, "run b (two)")

	type call struct {
		labels  []string
		changed bool
	}
	calls := make(chan call, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _, _ = p.Acquire(ctx, 1, "run c (three)", func(holders []Holder, changed bool) {
			labels := make([]string, 0, len(holders))
			for _, h := range holders {
				labels = append(labels, h.Label)
			}
			calls <- call{labels, changed}
		})
	}()
	next := func() call {
		t.Helper()
		select {
		case c := <-calls:
			return c
		case <-time.After(5 * time.Second):
			t.Fatal("onWait was not called")
			return call{}
		}
	}

	if c := next(); !c.changed || len(c.labels) != 1 || c.labels[0] != "run a (one)" {
		t.Fatalf("first call = %+v, want changed and naming run a (one)", c)
	}
	if c := next(); c.changed || len(c.labels) != 1 || c.labels[0] != "run a (one)" {
		t.Fatalf("heartbeat = %+v, want unchanged and still naming run a (one)", c)
	}

	held.Release()
	requireAdmitted(t, first, "run b (two)")
	for {
		c := next()
		if !c.changed {
			continue
		}
		if len(c.labels) != 1 || c.labels[0] != "run b (two)" {
			t.Fatalf("after the slot changed hands = %+v, want run b (two)", c)
		}
		return
	}
}
