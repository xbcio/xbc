// Package lease defines the protocol-neutral contract for renewable ownership
// of a named resource.
//
// It is a contract module: it owns no Definition, no Config, and no Bundle. It
// publishes only the vocabulary that transports, plugins, and application
// composition roots implement against, so that a component which needs to
// arbitrate ownership never has to depend on the backend that happens to
// provide it.
//
// # What the contract promises
//
// Acquisition is atomic and carries a ttl, so a process that crashes cannot
// retain ownership forever. Contention is ordinary control flow: TryAcquire
// reports it as (nil, false, nil), never as an error, because two owners
// racing for the same key is the expected outcome of the design rather than a
// failure of either one. Renew and Release compare the stored owner token
// atomically, so a lease that has already expired or been superseded cannot
// extend or delete its successor's claim.
//
// # Who holds a key
//
// TryAcquire takes a claimant: the name of the process asking. An
// implementation publishes it inside the owner token, so reading the stored
// value answers which process holds the key rather than only that someone does.
// It is never what ownership is decided by -- the token also carries a value
// unique to the one acquisition, because a claimant repeats across restarts and
// a token that were only the claimant would make a dead process's lease compare
// equal to its successor's. A caller with no identity to publish passes the
// empty string and gets a token that is unique and nothing more.
//
// # What the contract deliberately does not promise
//
// There is no enumeration: a Locker cannot list the keys it holds, because
// listing is a cluster-membership question and belongs to whoever aggregates
// per-process metrics. There is no fencing token, no generation counter, and
// no ordering between successive owners.
//
// That absence is the point. This contract arbitrates resource placement, not
// correctness: a caller whose work must not run twice keeps its own mutual
// exclusion, and treats a lost lease as a reason to stop rather than as proof
// that no other copy of the work is running. A backend that is cheap enough to
// be a single point of failure is therefore acceptable here -- ownership that
// briefly overlaps after a renewal failure costs extra capacity, while
// abandoning ownership costs a cluster-wide reshuffle.
//
// # Usage
//
// A backend implements both interfaces; a consumer declares only the interface
// it needs.
//
//	type redisLocker struct{ client *redis.Client }
//
//	func (l *redisLocker) TryAcquire(ctx context.Context, key, claimant string, ttl time.Duration) (lease.Lease, bool, error) {
//		token := ownerToken(claimant) // "<claimant>/<random>", or just the random half
//		acquired, err := l.client.SetNX(ctx, key, token, ttl).Result()
//		if err != nil || !acquired {
//			return nil, false, err
//		}
//		return &redisLease{client: l.client, key: key, owner: token}, true, nil
//	}
//
//	var _ lease.Locker = (*redisLocker)(nil)
package lease
