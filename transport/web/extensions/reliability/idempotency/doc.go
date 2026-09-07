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
// resolves the named *redis.Client automatically:
//
//	func (*Orders) RegisterRoutes(r *web.Router) {
//		r.POST("/orders", func(c *gin.Context) {
//			c.JSON(http.StatusCreated, gin.H{"status": "created"})
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
// Importing this package has no registration side effects. Import
// github.com/xbcio/xbc/transport/web/extensions/reliability/idempotency/autoload for
// process-wide composition, or use Definition/Bundle with a private assembly.
package idempotency
