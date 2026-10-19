package saml

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ReplayCache records assertion IDs until they expire. Verify calls Claim
// exactly once per response, after the signature and every condition have been
// validated, and fails closed on any error other than ErrReplay.
//
// Implementations must make Claim atomic: a set-if-absent that returns
// ErrReplay when the ID was already present and unexpired. A read-then-write
// implementation lets two concurrent callbacks both succeed.
//
// The cache must be shared by every replica that can receive a callback for
// the same provider. MemoryReplayCache is a single-replica implementation.
type ReplayCache interface {
	// Claim records id until expires. It returns ErrReplay when id is already
	// recorded and has not expired, and any other error when the cache itself
	// is unavailable.
	Claim(ctx context.Context, id string, expires time.Time) error
}

// MemoryReplayCache is a concurrency-safe, in-memory ReplayCache. It is
// correct only for a single replica: a second process keeps a separate map and
// would accept the same assertion. Multi-replica deployments must supply a
// shared implementation.
//
// Expired entries are discarded opportunistically on Claim. The cache never
// starts a goroutine.
type MemoryReplayCache struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]time.Time
}

// NewMemoryReplayCache returns an empty single-replica replay cache.
func NewMemoryReplayCache() *MemoryReplayCache {
	return newMemoryReplayCache(time.Now)
}

// newMemoryReplayCache is the injectable constructor used by tests and by a
// provider that shares its configured clock.
func newMemoryReplayCache(now func() time.Time) *MemoryReplayCache {
	if now == nil {
		now = time.Now
	}
	return &MemoryReplayCache{now: now, entries: make(map[string]time.Time)}
}

// Claim implements ReplayCache. An empty id or expiry is an error rather than
// a silent no-op: either would disable replay protection for that assertion.
func (c *MemoryReplayCache) Claim(ctx context.Context, id string, expires time.Time) error {
	if ctx == nil {
		return ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: context is done", ErrUnavailable)
	}
	if id == "" || expires.IsZero() {
		return fmt.Errorf("%w: replay claim needs an ID and an expiry", ErrUnavailable)
	}
	if c == nil {
		return fmt.Errorf("%w: nil replay cache", ErrUnavailable)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if recorded, exists := c.entries[id]; exists {
		if now.Before(recorded) {
			return ErrReplay
		}
		delete(c.entries, id)
	}
	for key, recorded := range c.entries {
		if !now.Before(recorded) {
			delete(c.entries, key)
		}
	}
	c.entries[id] = expires
	return nil
}
