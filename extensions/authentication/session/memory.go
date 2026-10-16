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

// MemoryStore is a process-local, thread-safe IndexedStore. Identifiers are
// indexed only by IDDigest, and a second index maps each subject to the digests
// of its sessions. Every mutation runs under one lock, so the subject index
// never holds a member whose record is gone and the sequencing the IndexedStore
// contract asks of a distributed store is trivially satisfied. A cleanup loop
// bounds retention of expired records.
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]memoryRecord
	index   map[string]map[string]struct{}
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
		index:   make(map[string]map[string]struct{}),
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
		if !record.live(now) {
			s.dropLocked(key)
		}
	}
}

// live reports whether the record is within both its absolute lifetime and its
// idle lease.
func (r memoryRecord) live(now time.Time) bool {
	return r.value.ExpiresAt.After(now) && r.idleExpiresAt.After(now)
}

// indexLocked adds digest to subject's index. The caller holds s.mu.
func (s *MemoryStore) indexLocked(subject, digest string) {
	members := s.index[subject]
	if members == nil {
		members = make(map[string]struct{})
		s.index[subject] = members
	}
	members[digest] = struct{}{}
}

// unindexLocked removes digest from subject's index and drops the subject's
// entry once it is empty. The caller holds s.mu.
func (s *MemoryStore) unindexLocked(subject, digest string) {
	members := s.index[subject]
	delete(members, digest)
	if len(members) == 0 {
		delete(s.index, subject)
	}
}

// dropLocked deletes the record stored under digest, then its index entry. The
// session goes first, matching the order the IndexedStore contract gives a
// distributed store. The caller holds s.mu.
func (s *MemoryStore) dropLocked(digest string) {
	record, ok := s.records[digest]
	if !ok {
		return
	}
	delete(s.records, digest)
	s.unindexLocked(record.value.Subject, digest)
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
		if existing.live(now) {
			return ErrAlreadyExists
		}
		s.dropLocked(key)
	}
	s.records[key] = memoryRecord{
		value:         stored,
		idleExpiresAt: earlier(now.Add(idleTTL), stored.ExpiresAt),
		lastTouched:   now,
	}
	s.indexLocked(stored.Subject, key)
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
	if !record.live(now) {
		s.dropLocked(key)
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
	if !record.live(now) {
		s.dropLocked(key)
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
	if !ok || !oldRecord.live(now) {
		s.dropLocked(oldKey)
		return ErrNotFound
	}
	if existing, ok := s.records[newKey]; ok {
		if existing.live(now) {
			return ErrAlreadyExists
		}
		s.dropLocked(newKey)
	}
	s.dropLocked(oldKey)
	s.records[newKey] = memoryRecord{
		value:         stored,
		idleExpiresAt: earlier(now.Add(idleTTL), stored.ExpiresAt),
		lastTouched:   now,
	}
	s.indexLocked(stored.Subject, newKey)
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
	s.dropLocked(IDDigest(id))
	return nil
}

var _ IndexedStore = (*MemoryStore)(nil)

// ListBySubject returns the live sessions of subject. A member whose record is
// gone, dead, or owned by another subject is pruned instead of reported.
func (s *MemoryStore) ListBySubject(ctx context.Context, subject string) ([]SessionInfo, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, ErrStopped
	}
	infos := make([]SessionInfo, 0, len(s.index[subject]))
	for digest := range s.index[subject] {
		record, ok := s.records[digest]
		switch {
		case !ok || record.value.Subject != subject:
			s.unindexLocked(subject, digest)
		case !record.live(now):
			s.dropLocked(digest)
		default:
			infos = append(infos, SessionInfo{
				Digest:     digest,
				Subject:    subject,
				Attributes: cloneMap(record.value.Attributes),
				IssuedAt:   record.value.IssuedAt,
				ExpiresAt:  record.value.ExpiresAt,
				LastSeenAt: record.lastTouched,
			})
		}
	}
	return infos, nil
}

// RevokeBySubject deletes every live session of subject and prunes the members
// it read. Dead and foreign members are removed without being counted.
func (s *MemoryStore) RevokeBySubject(ctx context.Context, subject string) (int, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return 0, err
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return 0, ErrStopped
	}
	revoked := 0
	for digest := range s.index[subject] {
		record, ok := s.records[digest]
		if !ok || record.value.Subject != subject {
			s.unindexLocked(subject, digest)
			continue
		}
		if record.live(now) {
			revoked++
		}
		s.dropLocked(digest)
	}
	return revoked, nil
}

// RevokeByDigest deletes the session named by digest only when it is indexed
// under subject, so a digest that belongs to another subject is never touched.
func (s *MemoryStore) RevokeByDigest(ctx context.Context, subject, digest string) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	subject, err := normalizeSubject(subject)
	if err != nil {
		return false, err
	}
	if !validDigest(digest) {
		return false, errors.New("session: digest must be 64 lowercase hex characters")
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false, ErrStopped
	}
	if _, member := s.index[subject][digest]; !member {
		return false, nil
	}
	record, ok := s.records[digest]
	if !ok || record.value.Subject != subject {
		s.unindexLocked(subject, digest)
		return false, nil
	}
	live := record.live(now)
	s.dropLocked(digest)
	return live, nil
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
		clear(s.index)
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
