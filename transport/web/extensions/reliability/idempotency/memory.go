package idempotency

import (
	"context"
	"sync"
	"time"
)

type memoryRecord struct {
	fingerprint string
	owner       string
	pending     bool
	response    Response
	expiresAt   time.Time
}

// MemoryStore is a thread-safe in-process Store. It is suitable for a single
// process; use RedisStore when duplicate suppression must span replicas.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]memoryRecord
	now     func() time.Time
}

// NewMemoryStore returns an empty Store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: make(map[string]memoryRecord), now: time.Now}
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

func cloneResponse(response Response) Response {
	response.Body = append([]byte(nil), response.Body...)
	return response
}
