package placement

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/plugin"
)

// renewOutcome is what one renewal round established about a slot. Three states
// are needed rather than a bool because "the store no longer confirms us" and
// "we already gave the slot back" have opposite responses: the first is a
// renewal failure the process keeps hosting through, the second is the ordinary
// result of a stop that has already happened.
type renewOutcome uint8

const (
	renewConfirmed renewOutcome = iota
	renewLost
	renewQuiesced
)

// heldSlot is one won slot: the ownership the store granted, plus the local
// state that decides whether this process may still act on it.
type heldSlot struct {
	workload plugin.WorkloadKey
	index    int
	key      string
	lease    lease.Lease

	// mu serializes renewal against release.
	//
	// It is deliberately held across the store calls. Renew and Release are the
	// two operations that must not interleave: a renewal that lands after the
	// release writes the slot back, and the whole point of releasing early is
	// that the process disappears before its TTL. Holding one mutex across both
	// makes them ordered by construction -- whichever wins, the release is the
	// last thing that touches the key.
	//
	// Because it spans a remote call, nothing that only wants to *read* this
	// slot may take it: a readiness probe or a metrics scrape that waited here
	// would inherit the store's latency, which is exactly the dependency the
	// probe exists to report on rather than to suffer. Observable fields are
	// therefore guarded by state instead, and mu is taken only by renew and
	// release.
	mu       sync.Mutex
	quiesced bool
	released bool

	// state guards the observable fields below, and is never held across a store
	// call. Lock order is mu then state; state is never held while acquiring mu,
	// so the two cannot deadlock.
	state    sync.RWMutex
	won      time.Time
	renewed  time.Time
	degraded bool
}

func newHeldSlot(workload plugin.WorkloadKey, index int, key string, won lease.Lease) *heldSlot {
	now := time.Now()
	return &heldSlot{
		workload: workload,
		index:    index,
		key:      key,
		lease:    won,
		won:      now,
		renewed:  now,
	}
}

// renew asks the store to confirm this process still owns the slot.
//
// A slot that has been quiesced reports renewQuiesced without touching the
// store at all. That check is the entire defence against a renewal landing
// after a release: the two share mu, so a renewal that started before the
// release completes before it, and one that starts after it finds quiesced set
// and writes nothing.
func (s *heldSlot) renew(ctx context.Context, ttl time.Duration) (renewOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quiesced {
		return renewQuiesced, nil
	}
	owned, err := s.lease.Renew(ctx, ttl)
	if err != nil {
		s.setDegraded(true)
		return renewLost, fmt.Errorf("placement: renew slot %q: %w", s.key, err)
	}
	if !owned {
		s.setDegraded(true)
		return renewLost, nil
	}
	s.setDegraded(false)
	return renewConfirmed, nil
}

// setDegraded records the outcome of one renewal round for readers.
func (s *heldSlot) setDegraded(degraded bool) {
	s.state.Lock()
	defer s.state.Unlock()
	s.degraded = degraded
	if !degraded {
		s.renewed = time.Now()
	}
}

// release gives the slot back and makes every later renewal a no-op.
//
// It is idempotent: the first call does the work and later calls report success
// without touching the store, so PreStop and the Stop backstop can both run.
// A store that reports the key as no longer ours is not an error -- the slot
// already belongs to someone else or has expired, which is the state this call
// was trying to reach, so the store's own answer is deliberately not reported
// as a distinguishable outcome.
func (s *heldSlot) release(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return nil
	}
	s.quiesced = true
	if _, err := s.lease.Release(ctx); err != nil {
		return fmt.Errorf("placement: release slot %q: %w", s.key, err)
	}
	s.state.Lock()
	s.released = true
	s.state.Unlock()
	return nil
}

// slotState is one read of a slot's observable fields.
type slotState struct {
	workload plugin.WorkloadKey
	index    int
	key      string
	owner    string
	won      time.Time
	renewed  time.Time
	degraded bool
	released bool
}

// snapshot reads this slot's observable fields without touching the store and
// without waiting behind an in-flight renewal.
func (s *heldSlot) snapshot() slotState {
	s.state.RLock()
	defer s.state.RUnlock()
	return slotState{
		workload: s.workload,
		index:    s.index,
		key:      s.key,
		owner:    s.lease.Owner(),
		won:      s.won,
		renewed:  s.renewed,
		degraded: s.degraded,
		released: s.released,
	}
}
