// Package testslots caps how many runs do heavy test work at once.
//
// A Pool is a counting semaphore held in the daemon's memory and shared by
// every run's executor, so the cap spans all repositories the daemon serves.
// Waiters are admitted in arrival order. Nothing is persisted: a slot ends
// with its Lease, and a daemon restart starts with every slot free.
package testslots

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultNotifyInterval is the longest a waiter stays silent: onWait is
// re-invoked at least this often so a step heartbeat keeps a queued run from
// looking wedged.
const DefaultNotifyInterval = 30 * time.Second

// Holder describes one live slot holder.
type Holder struct {
	Label string
	Since time.Time
}

// Describe renders the holder for a human, e.g. "run abc (feature), slot
// held 3m0s". The age is how long it has held its slot, and says so: a bare
// age read as a newcomer that had just taken a slot ahead of the queue, when
// it was a run that had waited longer and had only just been admitted.
func (h Holder) Describe() string {
	return h.Label + ", slot held " + time.Since(h.Since).Truncate(time.Second).String()
}

// DescribeHolders renders holders as one "; "-joined line.
func DescribeHolders(holders []Holder) string {
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		parts = append(parts, h.Describe())
	}
	return strings.Join(parts, "; ")
}

// WaitState is what a queued caller sees: its own place in the queue, how
// long it has waited, and who holds the slots it is waiting for.
type WaitState struct {
	// Position is the caller's 1-based place in the queue; position 1 is
	// admitted next.
	Position int
	// Queued is how many callers are waiting, the caller included.
	Queued  int
	Waited  time.Duration
	Holders []Holder
}

// Describe renders the state for a human, e.g. "position 2 of 3 in the queue,
// waiting 4m0s; slot holders: run abc (feature), slot held 12m0s".
func (s WaitState) Describe() string {
	return fmt.Sprintf("position %d of %d in the queue, waiting %s; slot holders: %s",
		s.Position, s.Queued, s.Waited.Truncate(time.Second), DescribeHolders(s.Holders))
}

// Pool is the shared set of held slots and the queue behind them. The zero
// value is ready to use. The cap is not a property of the pool: each Acquire
// passes the limit from its own run's config, so a config edit applies to the
// next run without a daemon restart.
type Pool struct {
	mu      sync.Mutex
	holders []*Lease
	waiters []*waiter

	notifyInterval time.Duration
}

// NewPool returns an empty pool.
func NewPool() *Pool {
	return &Pool{}
}

// Lease is a held slot. Release is idempotent and safe on a nil receiver.
type Lease struct {
	pool   *Pool
	holder Holder
}

type waiter struct {
	limit int
	label string
	// lease is set under Pool.mu when the waiter is admitted.
	lease *Lease
	// wake is signalled when the waiter is admitted or the holder set or its
	// place in the queue changed.
	wake chan struct{}
}

// Release frees the slot and admits the waiters it makes room for.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	p := l.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	i := slices.Index(p.holders, l)
	if i < 0 {
		return
	}
	p.holders = slices.Delete(p.holders, i, i+1)
	p.admitLocked()
	for _, w := range p.waiters {
		w.signal()
	}
}

// Acquire blocks until fewer than limit slots are held and every earlier
// waiter has been admitted, or ctx ends. It never fails for lack of a slot.
// A limit <= 0, or a nil pool, means unlimited: it returns at once with a nil
// Lease.
//
// onWait, when non-nil, is called with the caller's WaitState when the wait
// starts and whenever the holder set or its place in the queue changes
// (changed true), and again after DefaultNotifyInterval when nothing changed
// (changed false), so a caller can log on changes and heartbeat on the rest.
//
// It returns the time spent waiting, zero when a slot was free at once, so
// the caller can start its own budget clocks at acquisition. A wait that ends
// without a slot reports its length too, so the caller never books queue
// time as its own.
func (p *Pool) Acquire(ctx context.Context, limit int, label string, onWait func(WaitState, bool)) (*Lease, time.Duration, error) {
	if p == nil || limit <= 0 {
		return nil, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	p.mu.Lock()
	if len(p.waiters) == 0 && len(p.holders) < limit {
		lease := p.grantLocked(label)
		p.mu.Unlock()
		return lease, 0, nil
	}
	start := time.Now()
	w := &waiter{limit: limit, label: label, wake: make(chan struct{}, 1)}
	p.waiters = append(p.waiters, w)
	state := p.waitStateLocked(w, start)
	interval := p.notifyInterval
	p.mu.Unlock()
	if interval <= 0 {
		interval = DefaultNotifyInterval
	}

	changed := true
	for {
		if onWait != nil {
			onWait(state, changed)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			p.mu.Lock()
			lease := w.lease
			if lease == nil {
				i := slices.Index(p.waiters, w)
				p.waiters = slices.Delete(p.waiters, i, i+1)
				admitted := p.admitLocked()
				// Every waiter that was behind this one moved up a place;
				// if a slot was handed out, every waiter's holders changed.
				for j, o := range p.waiters {
					if admitted || j >= i {
						o.signal()
					}
				}
			}
			p.mu.Unlock()
			lease.Release()
			return nil, time.Since(start), ctx.Err()
		case <-w.wake:
			timer.Stop()
			changed = true
		case <-timer.C:
			changed = false
		}
		p.mu.Lock()
		lease := w.lease
		if lease == nil {
			state = p.waitStateLocked(w, start)
		}
		p.mu.Unlock()
		if lease != nil {
			return lease, time.Since(start), nil
		}
	}
}

func (w *waiter) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (p *Pool) grantLocked(label string) *Lease {
	lease := &Lease{pool: p, holder: Holder{Label: label, Since: time.Now()}}
	p.holders = append(p.holders, lease)
	return lease
}

// admitLocked hands free slots to the waiters at the head of the queue and
// reports whether it admitted any. It stops at the first waiter whose limit
// is reached, so a later waiter never overtakes an earlier one.
func (p *Pool) admitLocked() bool {
	admitted := false
	for len(p.waiters) > 0 && len(p.holders) < p.waiters[0].limit {
		w := p.waiters[0]
		p.waiters = p.waiters[1:]
		w.lease = p.grantLocked(w.label)
		w.signal()
		admitted = true
	}
	return admitted
}

// waitStateLocked reports where the queued waiter w stands.
func (p *Pool) waitStateLocked(w *waiter, start time.Time) WaitState {
	return WaitState{
		Position: slices.Index(p.waiters, w) + 1,
		Queued:   len(p.waiters),
		Waited:   time.Since(start),
		Holders:  p.holdersLocked(),
	}
}

func (p *Pool) holdersLocked() []Holder {
	holders := make([]Holder, 0, len(p.holders))
	for _, l := range p.holders {
		holders = append(holders, l.holder)
	}
	return holders
}
