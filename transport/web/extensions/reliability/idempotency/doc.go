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
// Retrying the same key, authenticated subject, route, query, content type, and
// body replays the stored response; reusing a key for a different fingerprint
// returns a conflict. NewMemoryStore is suitable only for a single process.
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
