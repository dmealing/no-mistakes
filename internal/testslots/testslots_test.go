package testslots

import (
	"context"
	"slices"
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
		lease, _, err := p.Acquire(context.Background(), limit, label, func(WaitState, bool) {
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
		lease, waited, err := p.Acquire(context.Background(), 2, label, func(WaitState, bool) {
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
	deadline, _ := ctx.Deadline()
	var queuedAt time.Time
	called := time.Now()
	lease, waited, err := p.Acquire(ctx, 1, "cancelled", func(WaitState, bool) {
		if queuedAt.IsZero() {
			queuedAt = time.Now()
		}
	})
	if err != context.DeadlineExceeded || lease != nil {
		t.Fatalf("lease %v, err %v, want no lease and deadline exceeded", lease, err)
	}
	// The caller subtracts the wait from its own clock, so a wait that ends
	// without a slot must still report how long it lasted. The timeout's
	// clock started before Acquire did, so the exact bounds are: from no
	// later than the first onWait call to the deadline, and no more than the
	// whole call.
	if minWait := deadline.Sub(queuedAt); waited < minWait || waited > time.Since(called) {
		t.Fatalf("waited = %v, want between %v (queued to deadline) and the whole call", waited, minWait)
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
		_, _, err := p.Acquire(ctx, 1, "head", func(WaitState, bool) { once.Do(func() { close(headQueued) }) })
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

// The queued line a run shows must name who holds the slots and where the
// run stands in the queue, be refreshed when either changes, and be repeated
// as a heartbeat when neither does.
func TestOnWaitNamesHoldersOnChangeAndHeartbeats(t *testing.T) {
	p := NewPool()
	p.notifyInterval = 20 * time.Millisecond
	held, _, _ := p.Acquire(context.Background(), 1, "run a (one)", nil)
	first := queue(t, p, 1, "run b (two)")

	type call struct {
		labels           []string
		position, queued int
		changed          bool
	}
	calls := make(chan call, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _, _ = p.Acquire(ctx, 1, "run c (three)", func(state WaitState, changed bool) {
			labels := make([]string, 0, len(state.Holders))
			for _, h := range state.Holders {
				labels = append(labels, h.Label)
			}
			calls <- call{labels, state.Position, state.Queued, changed}
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

	if c := next(); !c.changed || len(c.labels) != 1 || c.labels[0] != "run a (one)" || c.position != 2 || c.queued != 2 {
		t.Fatalf("first call = %+v, want changed, naming run a (one), position 2 of 2", c)
	}
	if c := next(); c.changed || len(c.labels) != 1 || c.labels[0] != "run a (one)" || c.position != 2 {
		t.Fatalf("heartbeat = %+v, want unchanged and still naming run a (one) at position 2", c)
	}

	held.Release()
	requireAdmitted(t, first, "run b (two)")
	for {
		c := next()
		if !c.changed {
			continue
		}
		if len(c.labels) != 1 || c.labels[0] != "run b (two)" || c.position != 1 || c.queued != 1 {
			t.Fatalf("after the slot changed hands = %+v, want run b (two) holding and position 1 of 1", c)
		}
		return
	}
}

type acquired struct {
	lease *Lease
	err   error
}

// enqueue is queue with a caller-owned context and the Acquire error kept, so
// a test can cancel one waiter and see how its wait ended.
func enqueue(t *testing.T, p *Pool, ctx context.Context, limit int, label string) <-chan acquired {
	t.Helper()
	queued := make(chan struct{})
	var once sync.Once
	got := make(chan acquired, 1)
	go func() {
		lease, _, err := p.Acquire(ctx, limit, label, func(WaitState, bool) {
			once.Do(func() { close(queued) })
		})
		got <- acquired{lease, err}
	}()
	select {
	case <-queued:
	case <-time.After(5 * time.Second):
		t.Fatalf("waiter %s never queued", label)
	}
	return got
}

// queueState reads who holds a slot and who waits, in queue order, straight
// from the pool, so an ordering test asserts the grant Release made rather
// than racing the waiters' goroutines to observe it.
func queueState(p *Pool) (holders, waiters []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.holders {
		holders = append(holders, l.holder.Label)
	}
	for _, w := range p.waiters {
		waiters = append(waiters, w.label)
	}
	return holders, waiters
}

func requireQueueState(t *testing.T, p *Pool, wantHolders, wantWaiters []string) {
	t.Helper()
	holders, waiters := queueState(p)
	if !slices.Equal(holders, wantHolders) || !slices.Equal(waiters, wantWaiters) {
		t.Fatalf("holders %v, waiters %v; want holders %v, waiters %v", holders, waiters, wantHolders, wantWaiters)
	}
}

// Slots go to waiters strictly in the order they started waiting, across
// every caller the pool serves: a waiter that gives up in the middle of the
// queue leaves it without holding or freeing a slot, the waiters behind it
// keep their places, and a caller arriving after that queues at the back.
// Each grant is read from the pool the moment Release returns, so the order
// is proven by the pool's own state, not by goroutine timing.
func TestSlotsGoToWaitersInTheOrderTheyStartedWaiting(t *testing.T) {
	p := NewPool()
	lease, _, err := p.Acquire(context.Background(), 1, "holder", nil)
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]<-chan acquired{}
	cancelMiddle := func() {}
	for _, label := range []string{"w1", "w2", "w3", "w4", "w5"} {
		ctx := context.Background()
		if label == "w3" {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			cancelMiddle = cancel
		}
		got[label] = enqueue(t, p, ctx, 1, label)
	}
	requireQueueState(t, p, []string{"holder"}, []string{"w1", "w2", "w3", "w4", "w5"})

	cancelMiddle()
	select {
	case r := <-got["w3"]:
		if r.lease != nil || r.err != context.Canceled {
			t.Fatalf("cancelled waiter: lease %v, err %v; want no lease and canceled", r.lease, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter never returned")
	}
	// It left the queue and took nothing with it: the slot is still held by
	// the same holder, and nobody behind it moved up into a slot.
	requireQueueState(t, p, []string{"holder"}, []string{"w1", "w2", "w4", "w5"})

	got["w6"] = enqueue(t, p, context.Background(), 1, "w6")
	requireQueueState(t, p, []string{"holder"}, []string{"w1", "w2", "w4", "w5", "w6"})

	order := []string{"w1", "w2", "w4", "w5", "w6"}
	for i, label := range order {
		lease.Release()
		requireQueueState(t, p, []string{label}, order[i+1:])
		select {
		case r := <-got[label]:
			if r.err != nil || r.lease == nil {
				t.Fatalf("waiter %s: lease %v, err %v", label, r.lease, r.err)
			}
			lease = r.lease
		case <-time.After(5 * time.Second):
			t.Fatalf("waiter %s was granted the slot but never returned", label)
		}
	}
	lease.Release()
	requireQueueState(t, p, nil, nil)
}

// A waiter ahead of this one cancelling moves it up a place, and that is a
// change worth logging even though no slot changed hands.
func TestWaiterAheadCancellingIsReportedAsAChange(t *testing.T) {
	p := NewPool()
	held, _, _ := p.Acquire(context.Background(), 1, "holder", nil)
	defer held.Release()
	ctx, cancelAhead := context.WithCancel(context.Background())
	defer cancelAhead()
	ahead := enqueue(t, p, ctx, 1, "ahead")

	positions := make(chan WaitState, 64)
	behindCtx, cancelBehind := context.WithCancel(context.Background())
	defer cancelBehind()
	go func() {
		_, _, _ = p.Acquire(behindCtx, 1, "behind", func(state WaitState, changed bool) {
			if changed {
				positions <- state
			}
		})
	}()
	next := func() WaitState {
		t.Helper()
		select {
		case s := <-positions:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("onWait did not report a change")
			return WaitState{}
		}
	}
	if s := next(); s.Position != 2 || s.Queued != 2 {
		t.Fatalf("queued at position %d of %d, want 2 of 2", s.Position, s.Queued)
	}
	cancelAhead()
	<-ahead
	if s := next(); s.Position != 1 || s.Queued != 1 {
		t.Fatalf("after the waiter ahead left: position %d of %d, want 1 of 1", s.Position, s.Queued)
	}
}

// The rendered line must not let a holder's age pass for a queue position or
// a newcomer: it names this run's own place and wait, and labels each
// holder's age as time spent holding its slot.
func TestWaitStateDescribeLabelsEveryDuration(t *testing.T) {
	now := time.Now()
	got := WaitState{
		Position: 2,
		Queued:   3,
		Waited:   4*time.Minute + 300*time.Millisecond,
		Holders: []Holder{
			{Label: "run a (one)", Since: now.Add(-12 * time.Minute)},
			{Label: "run b (two)", Since: now},
		},
	}.Describe()
	want := "position 2 of 3 in the queue, waiting 4m0s; slot holders: run a (one), slot held 12m0s; run b (two), slot held 0s"
	if got != want {
		t.Fatalf("Describe() =\n  %q\nwant\n  %q", got, want)
	}
}
