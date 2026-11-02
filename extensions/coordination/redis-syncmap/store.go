package redissyncmap

import (
	"container/list"
	"sync"
)

// store is the map's local cache. It always keeps recency order; size 0 means
// it never evicts. Eviction is deliberately local: an entry dropped here is
// still in the backing store and still reaches peers, so the next Load of an
// evicted key reports absence until a later write, notification, or Resync
// brings it back.
type store struct {
	mu      sync.Mutex
	maxSize int
	order   *list.List
	entries map[string]*list.Element
}

type storeEntry struct {
	key   string
	value any
}

func newStore(maxSize int) *store {
	return &store{
		maxSize: maxSize,
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

// Set inserts or replaces one entry and evicts the least recently used entries
// beyond the configured size.
func (s *store) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if element, ok := s.entries[key]; ok {
		element.Value.(*storeEntry).value = value
		s.order.MoveToFront(element)
		return
	}
	s.entries[key] = s.order.PushFront(&storeEntry{key: key, value: value})
	if s.maxSize <= 0 {
		return
	}
	for s.order.Len() > s.maxSize {
		oldest := s.order.Back()
		if oldest == nil {
			return
		}
		s.order.Remove(oldest)
		delete(s.entries, oldest.Value.(*storeEntry).key)
	}
}

// Get returns one value and marks it most recently used.
func (s *store) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	element, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	s.order.MoveToFront(element)
	return element.Value.(*storeEntry).value, true
}

// Delete removes one entry.
func (s *store) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	element, ok := s.entries[key]
	if !ok {
		return
	}
	s.order.Remove(element)
	delete(s.entries, key)
}

// Clear removes every entry.
func (s *store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.order.Init()
	clear(s.entries)
}

// Range snapshots the entries under the lock and then calls f without it, so a
// callback may call back into the map without deadlocking. The values
// themselves are shared, as they are between Load calls.
func (s *store) Range(f func(key string, value any) bool) {
	s.mu.Lock()
	snapshot := make([]storeEntry, 0, len(s.entries))
	for element := s.order.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*storeEntry)
		snapshot = append(snapshot, storeEntry{key: entry.key, value: entry.value})
	}
	s.mu.Unlock()

	for _, entry := range snapshot {
		if !f(entry.key, entry.value) {
			return
		}
	}
}

// Len reports the number of locally held entries.
func (s *store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
