package lease

import (
	"context"
	"time"
)

// Locker creates renewable leases. TryAcquire returns (nil, false, nil) when
// another owner currently holds key; contention is expected control flow, not
// an error. Implementations must make acquisition atomic and attach ttl so a
// crashed process cannot retain a lock forever. Implementations must observe
// ctx, while allowing for the normal distributed-systems outcome where an
// acquisition committed remotely just as ctx was cancelled; callers therefore
// still release a lease returned with acquired=true after cancellation.
//
// claimant names the process asking, so that an operator reading the backing
// store directly can tell which process holds a key rather than only that
// someone does. It is published, never consulted: an implementation must not
// compare it to decide ownership, and must not store it as the whole owner
// token. A claimant is a chosen name -- an instance id -- so it repeats across
// restarts, and a token that were only the claimant would let a stale Lease
// from a previous incarnation compare equal and renew or release its
// successor's lock, which is the one thing Lease forbids. An implementation
// that publishes a claimant therefore composes it with a value unique to the
// one acquisition. The empty string is valid and means the caller has no
// identity worth publishing; the token is then unique and nothing more.
//
// Implementations must also bound their own calls. Placement runs on the
// startup path before the assembly plan is built, where the framework applies no
// deadline of its own, so a store that accepts a connection and then stops
// answering would stall a process indefinitely; a client configured with its own
// dial, read and write timeouts -- as the shipped Redis implementation is --
// converts that into the startup failure it should be.
type Locker interface {
	TryAcquire(ctx context.Context, key, claimant string, ttl time.Duration) (lease Lease, acquired bool, err error)
}

// Lease represents ownership established by Locker. Renew and Release return
// false without error when ownership has already expired or moved to a new
// owner. Both operations must compare Owner atomically with the stored token;
// an old Lease must never extend or remove a successor's lock.
//
// Owner is that token rather than the claimant the lease was acquired for. The
// token is unique to one acquisition, which is what makes the comparison safe;
// an implementation that prefixes it with the claimant makes the stored value
// readable without making it reusable.
type Lease interface {
	Key() string
	Owner() string
	Renew(ctx context.Context, ttl time.Duration) (owned bool, err error)
	Release(ctx context.Context) (released bool, err error)
}
