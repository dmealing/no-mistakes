// Package testslots caps how many callers do heavy test work at once across
// every repo on the machine.
//
// A pool is Size lock files in one directory. A slot is held by holding an
// exclusive OS file lock on one of them, so the kernel releases it when the
// holder exits or dies for any reason; there is no counter to strand and no
// liveness heuristic. The holder also writes a small JSON record into the
// file so waiters and `axi status` can say who has the slots. The record is
// diagnostics only; the lock is the authority.
package testslots

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultPollInterval is how often a waiter retries every slot.
	DefaultPollInterval = 250 * time.Millisecond
	// DefaultNotifyInterval is the longest a waiter stays silent: onWait is
	// re-invoked at least this often so a step heartbeat keeps a queued run
	// from looking wedged.
	DefaultNotifyInterval = 30 * time.Second
)

// Holder describes one live slot holder.
type Holder struct {
	Slot  int       `json:"slot"`
	PID   int       `json:"pid"`
	Label string    `json:"label"`
	Since time.Time `json:"since"`
}

// Describe renders the holder for a human, e.g. "run abc (feature) pid 42, 3m0s".
func (h Holder) Describe() string {
	label := h.Label
	if label == "" {
		label = "unknown holder"
	}
	if h.PID > 0 {
		label += fmt.Sprintf(" pid %d", h.PID)
	}
	if !h.Since.IsZero() {
		label += ", " + time.Since(h.Since).Truncate(time.Second).String()
	}
	return label
}

// DescribeHolders renders holders as one "; "-joined line.
func DescribeHolders(holders []Holder) string {
	parts := make([]string, 0, len(holders))
	for _, h := range holders {
		parts = append(parts, h.Describe())
	}
	return strings.Join(parts, "; ")
}

// Pool is a fixed-size set of slots. Size <= 0 means unlimited: Acquire
// returns immediately without touching the filesystem.
type Pool struct {
	Dir  string
	Size int

	PollInterval time.Duration
}

// NewPool returns a pool of size slots under dir.
func NewPool(dir string, size int) *Pool {
	return &Pool{Dir: dir, Size: size}
}

// Lease is a held slot. Release is idempotent and safe on a nil receiver.
type Lease struct {
	file *os.File
}

// Release frees the slot.
func (l *Lease) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = unlockFile(l.file)
	_ = l.file.Close()
	l.file = nil
}

func (p *Pool) slotPath(i int) string {
	return filepath.Join(p.Dir, fmt.Sprintf("slot-%d.lock", i))
}

// Acquire blocks until a slot is free or ctx ends. It never fails for lack of
// a slot. onWait, when non-nil, is called with the current holders when the
// wait starts and whenever the holder set changes (changed true), and again
// after at least DefaultNotifyInterval even when nothing changed (changed
// false), so a caller can log on changes and heartbeat on the rest. It
// returns the time spent waiting, zero when a slot was free at once, so the
// caller can start its own budget clocks at acquisition.
func (p *Pool) Acquire(ctx context.Context, label string, onWait func([]Holder, bool)) (*Lease, time.Duration, error) {
	if p == nil || p.Size <= 0 {
		return &Lease{}, 0, nil
	}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return nil, 0, fmt.Errorf("create test slot dir: %w", err)
	}
	poll := p.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	start := time.Now()
	var lastNotify time.Time
	var lastSeen string
	for queued := false; ; queued = true {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		// One scan per tick: a busy slot's open fd also yields its holder
		// record, so waiting never re-walks the files a second time.
		lease, holders, err := p.tryAcquire(label, onWait != nil)
		if err != nil {
			return nil, 0, err
		}
		if lease != nil {
			if !queued {
				return lease, 0, nil
			}
			return lease, time.Since(start), nil
		}
		if onWait != nil {
			seen := holderKey(holders)
			changed := lastNotify.IsZero() || seen != lastSeen
			if changed || time.Since(lastNotify) >= DefaultNotifyInterval {
				onWait(holders, changed)
				lastNotify, lastSeen = time.Now(), seen
			}
		}
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func holderKey(holders []Holder) string {
	var b strings.Builder
	for _, h := range holders {
		fmt.Fprintf(&b, "%d:%d:%s;", h.Slot, h.PID, h.Label)
	}
	return b.String()
}

func (p *Pool) tryAcquire(label string, wantHolders bool) (*Lease, []Holder, error) {
	var holders []Holder
	for i := 0; i < p.Size; i++ {
		f, err := os.OpenFile(p.slotPath(i), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open test slot %d: %w", i, err)
		}
		locked, err := tryLockFile(f)
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("lock test slot %d: %w", i, err)
		}
		if !locked {
			if wantHolders {
				holders = append(holders, readHolder(f, i))
			}
			f.Close()
			continue
		}
		// Diagnostics only; a failed write never costs the slot.
		_ = f.Chmod(0o600)
		if data, err := json.Marshal(Holder{Slot: i, PID: os.Getpid(), Label: label, Since: time.Now().UTC()}); err == nil {
			_ = f.Truncate(0)
			_, _ = f.WriteAt(data, 0)
		}
		return &Lease{file: f}, nil, nil
	}
	return nil, holders, nil
}

// readHolder renders the record a live holder wrote into its lock file. The
// caller holds the file open; the slot index comes from the scanner, never
// from the record.
func readHolder(f *os.File, slot int) Holder {
	var h Holder
	buf := make([]byte, 4096)
	if n, _ := f.ReadAt(buf, 0); n > 0 {
		_ = json.Unmarshal(buf[:n], &h)
	}
	h.Slot = slot
	return h
}

// Holders reports the live holders of the first Size slots. A slot whose lock
// is free, or cannot be probed, is not reported, whatever record it still
// holds.
func (p *Pool) Holders() []Holder {
	if p == nil || p.Size <= 0 {
		return nil
	}
	var holders []Holder
	for i := 0; i < p.Size; i++ {
		f, err := os.OpenFile(p.slotPath(i), os.O_RDWR, 0)
		if err != nil {
			continue
		}
		locked, err := tryLockFile(f)
		if locked {
			_ = unlockFile(f)
		} else if err == nil {
			holders = append(holders, readHolder(f, i))
		}
		f.Close()
	}
	return holders
}
