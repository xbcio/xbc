// Package idempotency prevents duplicate execution of routes explicitly
// marked Route.Idempotent. Its middleware runs in PhaseBusiness, after
// authentication, because the verified principal subject is part of every
// request fingerprint.
//
// # Usage
//
// Mark only operations that are safe to replay, then require clients to send an
// Idempotency-Key. For a replicated service, select the redis backend so every
// replica observes the same reservation; the Definition-driven composition
// resolves the named Redis client automatically, whatever topology it was
// configured for:
//
//	func (*Orders) RegisterRoutes(r *web.Router) {
//		r.POST("/orders", func(ctx context.Context, c *web.Ctx) error {
//			c.JSON(http.StatusCreated, map[string]string{"status": "created"})
//			return nil
//		}).Name("orders.create").Idempotent()
//	}
//
// A directly constructed plugin must inject its own Store for the redis
// backend, since it has no dependency graph to resolve one from:
//
//	func newIdempotencyPlugin(client *redis.Client) (*idempotency.Plugin, error) {
//		store, err := idempotency.NewRedisStore(client, "orders:idempotency:")
//		if err != nil {
//			return nil, err
//		}
//		cfg := idempotency.DefaultConfig()
//		return idempotency.New(cfg, idempotency.WithStore(store))
//	}
//
// Retrying the same key, authenticated subject, concrete request path, query,
// content type, and body replays the stored response; reusing a key for a
// different request -- including one key on two different path-parameter
// values routed through the same template -- returns a conflict. The replay
// carries the whitelisted response headers: Content-Type, Location,
// Content-Language, Content-Disposition, ETag, Last-Modified and Cache-Control.
// Set-Cookie is never stored or replayed, and a record written before the
// whitelist existed replays without the non-Content-Type headers. A whitelisted
// header value is bounded at 1 KiB, the header map at 8 KiB in total, and the
// separately carried Content-Type at 1 KiB; a value beyond its bound is dropped
// when the record is written and is never replayed, so a handler cannot make
// its own key unreplayable. A record stored by a version that scoped to the
// route template is not found after an upgrade on a parameterized route: the
// retry executes again and the old record expires with its TTL. Routes without
// path parameters are unaffected because their template and concrete path are
// the same.
//
// A MemoryStore retains at most 10000 records. Under pressure it reclaims
// expired records first, then evicts completed records closest to expiry, and
// refuses a new key while every retained record is a live pending reservation.
// An evicted completed record no longer replays, so a retry of that key
// executes again: size a single-process deployment for key rate times ttl, or
// use the Redis backend when duplicate suppression must be strict across time
// or replicas.
//
// A stored response is keyed and fingerprinted by the authenticated
// Principal's Subject, so a request carrying the Idempotency-Key header
// without a published Principal is refused with 403 idempotency_requires_principal
// before the store is ever touched: without a caller identity to scope the key
// to, two unrelated callers reusing the same header value would otherwise
// share one storage slot. A request without the header never reaches that
// refusal: a route marked Route.Idempotent answers a missing or malformed
// header with 400 invalid_idempotency_key exactly as it did before this rule,
// and the middleware does not act on any other route at all.
//
// Importing this package has no registration side effects. Import
// github.com/xbcio/xbc/transport/web/extensions/reliability/idempotency/autoload for
// process-wide composition, or use Definition/Bundle with a private assembly.
package idempotency
