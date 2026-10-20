package idempotency

import (
	"context"
	"errors"
	"sync"
	"time"
)

// defaultMemoryMaxEntries bounds an in-process store. Each record's body is
// bounded by the configured max_response_bytes, its whitelisted header map by
// 8 KiB, and its separately carried Content-Type by 1 KiB, so retained memory
// is bounded by this count times the sum of those bounds plus per-record
// overhead. There is no configuration field for the cap: the memory backend is
// the single-process default, and a deployment that needs a different trade-off
// uses the Redis backend.
const defaultMemoryMaxEntries = 10000

// errMemoryStoreFull means every retained record is a live pending
// reservation, so admitting a new key would require dropping one. The
// middleware maps a store error to 503 idempotency_unavailable.
var errMemoryStoreFull = errors.New("idempotency: memory store is full of pending reservations")

type memoryRecord struct {
	fingerprint string
	owner       string
	pending     bool
	response    Response
	expiresAt   time.Time
}

// MemoryStore is a thread-safe in-process Store. It is suitable for a single
// process; use RedisStore when duplicate suppression must span replicas.
//
// A MemoryStore retains at most 10000 records. When it is full it reclaims
// expired records first, then evicts completed records closest to expiry, and
// refuses a new key only while every retained record is still a live pending
// reservation. An evicted completed record no longer replays, so a retry of
// that key executes again: size the deployment for key rate x ttl, or use the
// Redis backend when duplicate suppression must be strict across time or
// replicas.
type MemoryStore struct {
	mu         sync.Mutex
	records    map[string]memoryRecord
	maxEntries int
	now        func() time.Time
}

// NewMemoryStore returns an empty Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]memoryRecord), maxEntries: defaultMemoryMaxEntries, now: time.Now}
}

func (s *MemoryStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// Acquire atomically creates a pending reservation or observes the current one.
func (s *MemoryStore) Acquire(ctx context.Context, key, fingerprint, owner string, pendingTTL time.Duration) (AcquireResult, error) {
	if err := ctx.Err(); err != nil {
		return AcquireResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = make(map[string]memoryRecord)
	}
	now := s.clock()
	record, exists := s.records[key]
	if exists && !record.expiresAt.After(now) {
		delete(s.records, key)
		exists = false
	}
	if !exists {
		if err := s.makeRoomLocked(now); err != nil {
			return AcquireResult{}, err
		}
		s.records[key] = memoryRecord{
			fingerprint: fingerprint,
			owner:       owner,
			pending:     true,
			expiresAt:   now.Add(pendingTTL),
		}
		return AcquireResult{State: Acquired}, nil
	}
	if record.fingerprint != fingerprint {
		return AcquireResult{State: Conflict}, nil
	}
	if record.pending {
		return AcquireResult{State: Pending}, nil
	}
	return AcquireResult{State: Completed, Response: cloneResponse(record.response)}, nil
}

// Complete performs an owner-safe pending-to-completed transition.
func (s *MemoryStore) Complete(ctx context.Context, key, fingerprint, owner string, response Response, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[key]
	if !exists || !record.expiresAt.After(s.clock()) || !record.pending || record.fingerprint != fingerprint || record.owner != owner {
		if exists && !record.expiresAt.After(s.clock()) {
			delete(s.records, key)
		}
		return ErrOwnershipLost
	}
	record.pending = false
	record.owner = ""
	record.response = cloneResponse(response)
	record.expiresAt = s.clock().Add(ttl)
	s.records[key] = record
	return nil
}

// Release removes only the caller's still-pending reservation.
func (s *MemoryStore) Release(ctx context.Context, key, fingerprint, owner string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[key]
	if !exists {
		return nil
	}
	if !record.expiresAt.After(s.clock()) {
		delete(s.records, key)
		return nil
	}
	if !record.pending || record.fingerprint != fingerprint || record.owner != owner {
		return ErrOwnershipLost
	}
	delete(s.records, key)
	return nil
}

func (s *MemoryStore) limitLocked() int {
	if s.maxEntries <= 0 {
		return defaultMemoryMaxEntries
	}
	return s.maxEntries
}

// makeRoomLocked frees one slot before a new reservation is inserted. Expired
// records go first, then the completed record closest to expiry: a completed
// record only caches a replay, while a pending record is the duplicate
// suppression an in-flight request depends on. When every retained record is
// still pending, the store refuses the new key rather than drop a live
// reservation.
func (s *MemoryStore) makeRoomLocked(now time.Time) error {
	limit := s.limitLocked()
	if len(s.records) < limit {
		return nil
	}
	for key, record := range s.records {
		if !record.expiresAt.After(now) {
			delete(s.records, key)
		}
	}
	for len(s.records) >= limit {
		victim := ""
		var expiry time.Time
		for key, record := range s.records {
			if record.pending {
				continue
			}
			if victim == "" || record.expiresAt.Before(expiry) {
				victim, expiry = key, record.expiresAt
			}
		}
		if victim == "" {
			return errMemoryStoreFull
		}
		delete(s.records, victim)
	}
	return nil
}

func cloneResponse(response Response) Response {
	response.Body = append([]byte(nil), response.Body...)
	if response.Header != nil {
		response.Header = response.Header.Clone()
	}
	return response
}
