package session

import (
	"context"
	"errors"
	"sync"
	"time"
)

type memoryRecord struct {
	value         Session
	idleExpiresAt time.Time
	lastTouched   time.Time
}

// MemoryStore is a process-local, thread-safe Store. Identifiers are indexed
// only by IDDigest. A cleanup loop bounds retention of expired records.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]memoryRecord
	now     func() time.Time
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	stopped bool
}

// NewMemoryStore starts an in-process store cleanup loop.
func NewMemoryStore(cleanupInterval time.Duration) (*MemoryStore, error) {
	if cleanupInterval <= 0 {
		return nil, errors.New("session: memory cleanup interval must be positive")
	}
	store := &MemoryStore{
		records: make(map[string]memoryRecord),
		now:     time.Now,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go store.cleanupLoop(cleanupInterval)
	return store, nil
}

func (s *MemoryStore) clock() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func (s *MemoryStore) cleanupLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer func() {
		ticker.Stop()
		close(s.done)
	}()
	for {
		select {
		case <-ticker.C:
			s.cleanup(s.clock())
		case <-s.stop:
			return
		}
	}
}

func (s *MemoryStore) cleanup(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	for key, record := range s.records {
		if !record.value.ExpiresAt.After(now) || !record.idleExpiresAt.After(now) {
			delete(s.records, key)
		}
	}
}

func (s *MemoryStore) Create(ctx context.Context, value Session, idleTTL time.Duration) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	value, err := normalizeSession(value)
	if err != nil {
		return err
	}
	if idleTTL <= 0 {
		return errors.New("session: idle TTL must be positive")
	}
	now := s.clock()
	if !value.ExpiresAt.After(now) {
		return ErrNotFound
	}
	key := IDDigest(value.ID)
	stored := cloneSession(value)
	stored.ID = ""

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopped
	}
	if existing, ok := s.records[key]; ok {
		if existing.value.ExpiresAt.After(now) && existing.idleExpiresAt.After(now) {
			return ErrAlreadyExists
		}
		delete(s.records, key)
	}
	s.records[key] = memoryRecord{
		value:         stored,
		idleExpiresAt: earlier(now.Add(idleTTL), stored.ExpiresAt),
		lastTouched:   now,
	}
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, id string) (Session, bool, error) {
	if err := contextError(ctx); err != nil {
		return Session{}, false, err
	}
	if id == "" {
		return Session{}, false, nil
	}
	now := s.clock()
	key := IDDigest(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return Session{}, false, ErrStopped
	}
	record, ok := s.records[key]
	if !ok {
		return Session{}, false, nil
	}
	if !record.value.ExpiresAt.After(now) || !record.idleExpiresAt.After(now) {
		delete(s.records, key)
		return Session{}, false, nil
	}
	value := cloneSession(record.value)
	value.ID = id
	return value, true, nil
}

func (s *MemoryStore) Touch(ctx context.Context, id string, idleTTL, minInterval time.Duration) (Session, bool, error) {
	if err := contextError(ctx); err != nil {
		return Session{}, false, err
	}
	if id == "" || idleTTL <= 0 || minInterval < 0 {
		return Session{}, false, errors.New("session: invalid touch arguments")
	}
	now := s.clock()
	key := IDDigest(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return Session{}, false, ErrStopped
	}
	record, ok := s.records[key]
	if !ok {
		return Session{}, false, nil
	}
	if !record.value.ExpiresAt.After(now) || !record.idleExpiresAt.After(now) {
		delete(s.records, key)
		return Session{}, false, nil
	}
	if minInterval == 0 || now.Sub(record.lastTouched) >= minInterval {
		record.lastTouched = now
		record.idleExpiresAt = earlier(now.Add(idleTTL), record.value.ExpiresAt)
		s.records[key] = record
	}
	value := cloneSession(record.value)
	value.ID = id
	return value, true, nil
}

func (s *MemoryStore) Rotate(ctx context.Context, oldID string, replacement Session, idleTTL time.Duration) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	replacement, err := normalizeSession(replacement)
	if err != nil {
		return err
	}
	if oldID == "" || replacement.ID == oldID || idleTTL <= 0 {
		return errors.New("session: invalid rotation arguments")
	}
	now := s.clock()
	oldKey, newKey := IDDigest(oldID), IDDigest(replacement.ID)
	stored := cloneSession(replacement)
	stored.ID = ""

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopped
	}
	oldRecord, ok := s.records[oldKey]
	if !ok || !oldRecord.value.ExpiresAt.After(now) || !oldRecord.idleExpiresAt.After(now) {
		delete(s.records, oldKey)
		return ErrNotFound
	}
	if existing, ok := s.records[newKey]; ok {
		if existing.value.ExpiresAt.After(now) && existing.idleExpiresAt.After(now) {
			return ErrAlreadyExists
		}
		delete(s.records, newKey)
	}
	delete(s.records, oldKey)
	s.records[newKey] = memoryRecord{
		value:         stored,
		idleExpiresAt: earlier(now.Add(idleTTL), stored.ExpiresAt),
		lastTouched:   now,
	}
	return nil
}

func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrStopped
	}
	delete(s.records, IDDigest(id))
	return nil
}

// Close stops cleanup and erases all retained sessions. It is safe to call
// repeatedly and concurrently.
func (s *MemoryStore) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.stopped = true
		clear(s.records)
		s.mu.Unlock()
		close(s.stop)
	})
	<-s.done
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("session: context cannot be nil")
	}
	return ctx.Err()
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
