package federation

import (
	"context"
	"sync"
	"time"
)

// memorySweepInterval bounds how often expired entries are reclaimed. The
// store starts no goroutine: reclamation runs inline on a write, which keeps
// the store lifecycle-free at the cost of a bounded amount of dead memory
// between sweeps.
const memorySweepInterval = time.Minute

// memorySweepThreshold forces a sweep on a write even before the interval
// elapses, so a burst of abandoned logins cannot grow the map without bound.
const memorySweepThreshold = 4096

type memoryEntry struct {
	transaction Transaction
	value       string
	expiresAt   time.Time
}

// MemoryStore keeps login transactions and reservations in this process.
//
// It is correct only for a single-replica deployment. A login started on one
// replica and completed on another cannot find its transaction in this store;
// multi-replica deployments must use RedisStore, which both replicas share.
//
// MemoryStore starts no goroutine and owns no resource, so it implements
// neither plugin.Drainer nor Stop. Expired entries are reclaimed inline on
// writes, and a process that stops serving abandons the map with the process.
// Every method is safe for concurrent use.
type MemoryStore struct {
	mu        sync.Mutex
	entries   map[string]memoryEntry
	now       func() time.Time
	lastSweep time.Time
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty single-replica store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[string]memoryEntry), now: time.Now}
}

func (s *MemoryStore) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

// Create stores tx unless a live transaction already holds the same provider
// and state.
func (s *MemoryStore) Create(ctx context.Context, tx Transaction) error {
	if s == nil {
		return ErrNilStore
	}
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	now := s.clock()
	if err := tx.validate(now); err != nil {
		return err
	}

	key := transactionKey("", tx.Provider, tx.State)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if entry, exists := s.entries[key]; exists && entry.expiresAt.After(now) {
		return ErrTransactionExists
	}
	s.entries[key] = memoryEntry{transaction: tx, expiresAt: tx.ExpiresAt}
	return nil
}

// Consume atomically deletes the transaction and returns it only to the
// browser that started it.
func (s *MemoryStore) Consume(ctx context.Context, provider, state, binding string) (Transaction, bool, error) {
	if s == nil {
		return Transaction{}, false, ErrNilStore
	}
	if err := checkContext(ctx); err != nil {
		return Transaction{}, false, err
	}
	if !validProviderName(provider) || !validState(state) || !validBindingDigest(binding) {
		return Transaction{}, false, nil
	}
	now := s.clock()

	key := transactionKey("", provider, state)
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[key]
	if !exists {
		return Transaction{}, false, nil
	}
	delete(s.entries, key)
	if !entry.expiresAt.After(now) {
		return Transaction{}, false, nil
	}
	if !entry.transaction.matches(provider, state, binding) {
		return Transaction{}, false, nil
	}
	return entry.transaction, true, nil
}

// SetIfAbsent reserves key under namespace for ttl and reports whether this
// call created it.
func (s *MemoryStore) SetIfAbsent(ctx context.Context, namespace, key string, ttl time.Duration) (bool, error) {
	if s == nil {
		return false, ErrNilStore
	}
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if !validReservationNamespace(namespace) || !validReservationKey(key) || !validReservationTTL(ttl) {
		return false, ErrInvalidReservation
	}
	now := s.clock()

	compound := reservationKey("", namespace, key)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if entry, exists := s.entries[compound]; exists && entry.expiresAt.After(now) {
		return false, nil
	}
	s.entries[compound] = memoryEntry{value: "1", expiresAt: now.Add(ttl)}
	return true, nil
}

// sweepLocked reclaims expired entries. It runs under the caller's lock.
func (s *MemoryStore) sweepLocked(now time.Time) {
	if len(s.entries) < memorySweepThreshold && now.Sub(s.lastSweep) < memorySweepInterval {
		return
	}
	for key, entry := range s.entries {
		if !entry.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
	s.lastSweep = now
}
