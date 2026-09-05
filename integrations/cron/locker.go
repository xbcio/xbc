package cron

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
type Locker interface {
	TryAcquire(ctx context.Context, key string, ttl time.Duration) (lease Lease, acquired bool, err error)
}

// Lease represents ownership established by Locker. Renew and Release return
// false without error when ownership has already expired or moved to a new
// owner. Both operations must compare Owner atomically with the stored token;
// an old Lease must never extend or remove a successor's lock.
type Lease interface {
	Key() string
	Owner() string
	Renew(ctx context.Context, ttl time.Duration) (owned bool, err error)
	Release(ctx context.Context) (released bool, err error)
}
