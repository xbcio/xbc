// Package oncetoken issues single-use tokens backed by Redis and consumes
// them atomically.
//
// A token is 32 lowercase hexadecimal characters derived from 16 bytes read
// from crypto/rand, so it carries 128 bits of entropy. Issue binds the token to
// a path (an empty path means no binding) and stores that path under
// Prefix+token with a lifetime: the store's default, or a per-call override.
//
// # Single-use consumption
//
// Consume reads the bound path, compares it with the caller's path, and deletes
// the key in one Lua evaluation. Redis executes a script atomically, so two
// consumers racing on the same token cannot both observe it: exactly one
// receives the bound path and every other receives ErrExpired. A naive
// GET-then-DEL sequence has a window between the two commands in which both
// requests read the value and both then delete it, so the "one-time" token is
// in fact used twice.
//
// # Why a path mismatch destroys the token
//
// When the stored path and the caller's path are both non-empty and differ,
// Consume deletes the key before returning ErrPathMismatch. The rejection and
// the destruction happen in the same script, so an attacker holding a stolen
// token cannot probe candidate paths one at a time against a still-valid token:
// the first wrong guess burns it.
//
// # Token format
//
// Consume accepts only ^[0-9a-f]{32}$. The check runs before the Redis key is
// built, because Prefix+token is the key: a caller-supplied token containing a
// colon, a wildcard, or another key's name could otherwise address keys outside
// the store's namespace. A malformed token returns ErrInvalid without touching
// Redis.
//
// # HTTP use
//
// The package depends on a Redis client and the standard library only, and it
// has no HTTP types, so a caller wraps it in its own middleware:
//
//	func OnceToken(store *oncetoken.Store) gin.HandlerFunc {
//		return func(c *gin.Context) {
//			ctx := c.Request.Context()
//			if _, err := store.Consume(ctx, c.Query("token"), c.Request.URL.Path); err != nil {
//				c.AbortWithStatus(http.StatusUnauthorized)
//				return
//			}
//			c.Next()
//		}
//	}
//
// The response deliberately does not distinguish the three errors: reporting
// that a token expired rather than mismatched would confirm that it existed.
//
// # Errors
//
// Consume reports ErrInvalid, ErrExpired, or ErrPathMismatch; a Redis failure
// is wrapped with the underlying cause. No error contains the token itself,
// because a token is a credential and error strings are routinely logged. This
// package writes no logs of its own.
package oncetoken
