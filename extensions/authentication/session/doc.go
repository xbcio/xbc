// Package session provides protocol-neutral, opaque server-side sessions for
// XBC.
//
// A session ID is a cryptographically random identifier. Built-in stores hash
// that identifier before using it as a storage key, and all identity and
// attribute data remains server-side. This package owns everything about a
// session except how its ID travels: configuration, the memory and Redis
// stores, the Manager that creates, rotates, and revokes sessions and works on
// a subject's index, the AttributeManager that verifies them and maintains
// their attributes in place, and the authentication.Authenticator for the
// "session" scheme. The Web adapter at
// github.com/xbcio/xbc/transport/web/extensions/authentication/session
// carries the ID in an HTTP cookie configured by Cookie and bounded by
// Lifetime.
//
// # Usage
//
// Configure plugins.session and select the Web adapter's Bundle, which
// includes this one. Application code that only needs the session lifecycle
// can depend on Manager directly:
//
//	var sessions = plugin.RefTo[session.Manager](session.Key)
//
//	func revoke(ctx context.Context, manager session.Manager, id string) error {
//		if err := manager.Revoke(ctx, id); err != nil && !errors.Is(err, session.ErrNotFound) {
//			return err
//		}
//		return nil
//	}
//
// Verified sessions travel as authentication.Principal values whose Attributes
// hold the stored attributes and the opaque ID under "session_id". Use the
// Redis backend when sessions must be shared across application replicas.
//
// Application code that needs verification of a presented ID, or in-place
// attribute maintenance, resolves the same plugin's extended contract:
//
//	var attributes = plugin.RefTo[session.AttributeManager](session.Key)
//
// AttributeManager embeds Manager, so a consumer that resolves the extended
// contract can also create, rotate, and revoke. Manager itself keeps its
// published method set: an application or test double that implements only the
// lifecycle does not gain methods and keeps compiling.
//
// # Updating attributes versus rotating
//
// AttributeManager.UpdateAttributes replaces a live session's attributes in
// place. Use it only when the session's authority does not change -- a display
// name, a profile snapshot, a locale. It preserves the ID, subject, issuance
// time, absolute expiry, and idle lease, so it neither extends the session nor
// revives one that expired, was revoked, or never existed (ErrNotFound).
//
// AttributeManager.PatchAttributes applies a delta instead: it removes every
// key in unset, then assigns every key in set. Use it when independent pieces
// of the application maintain independent attributes (a locale writer and a
// display name writer, say), because two whole-map updates racing each other
// keep only the last one while two patches of different keys both survive.
// Naming one key in both set and unset is a parameter error; an empty patch is
// a lookup that still requires a live session. The lifetime rules are
// UpdateAttributes' rules exactly: no deadline moves and no dead session comes
// back. The memory store merges under its lock; the Redis store merges
// optimistically with a bounded retry and reports ErrConflict when concurrent
// writers exhaust it. The Redis
// merge runs in Go rather than in Lua on purpose: Redis's cjson renders numbers
// with 14 significant digits and encodes an empty array as an empty object, so
// an arbitrary attribute value would not survive a Lua-side decode and encode.
// The script instead compares the value it was given with the value in Redis and
// writes the merged result only when they still match, so a concurrent writer
// costs a retry rather than a lost update. A healthy deployment pays one read
// and one single-key script per patch; a host that patches the same session from
// many goroutines at once should treat ErrConflict as a retry signal.
//
// A privilege change is never an attribute update or patch. Rotate the session
// (a new ID with the same absolute lifetime) or revoke and re-create it, so a
// credential minted under the old authority cannot keep the new one.
// UpdateAttributes needs a Store that also implements the optional
// AttributeStore capability; PatchAttributes needs a PatchStore. The built-in
// memory and Redis stores implement both, a custom Store injected with WithStore
// need not, and AttributeManager then returns ErrNotSupported. The Redis update
// runs as one single-key script that preserves the key TTL and the subject index
// and refuses a key whose lifetime has passed.
//
// # Concurrent session limit
//
// plugins.session accepts max_sessions (default 0, no limit) and on_exceed
// (revoke_oldest by default, or reject_new). When max_sessions is positive,
// Manager.Create first consults the subject index: revoke_oldest revokes the
// subject's earliest-issued live session until there is room and then creates
// the new one (each eviction is reported as EventRevoked with the evicted
// digest and Count 1), while reject_new reports ErrTooManySessions. The limit
// applies to Create only; Rotate replaces an existing session and never changes
// the count. The index requirement is enforced fail-closed: a Store without
// IndexedStore fails construction when the plugin is built and reports
// ErrNotSupported from Create if a manager was assembled directly, so a
// configured bound can never be silently ignored.
//
// The bound is best effort under concurrency. Parallel Creates can each read a
// full subject and briefly exceed the limit by up to their own number; the next
// Create sees the larger count and converges it. Sequential creates are strictly
// bounded, and only a limit that must hold under adversarial concurrency needs
// the store's own locking.
//
// A session evicted by the limit is gone; the next request that presents it
// fails exactly like an expired or revoked session and the client sees the one
// collapsed "invalid credential" rejection. The client is not told it was
// evicted, and there is no tombstone or "expired elsewhere" page: either would
// make the session check an oracle for which IDs existed. This is an
// intentional difference from frameworks that render a reason.
//
// # Verification verdicts
//
// AttributeManager.Verify is the single session check behind Authenticate. A
// valid session is returned with its idle lease renewed, exactly as
// Authenticate always did, subject to touch_interval throttling. Every other
// outcome is classified as a Verdict: Malformed (the ID fails the shape check),
// NotFound (an expired, evicted, revoked, or never-created session, all
// indistinguishable deliberately), or StoreError (with the underlying error). A
// stopped manager and an unusable context are StoreError too: neither says
// anything about the presented ID.
//
// A Verdict is for the host's own auditing, metrics, and diagnostics. Never
// return one to a client, and never derive a client-visible message from it:
// distinguishing "malformed" from "unknown" turns the session check into an
// oracle for which IDs exist. The Authenticate adapter collapses every
// non-valid verdict into one rejection reason for exactly that reason.
//
// # Observers
//
// An Observer receives a session Event after each operation completes,
// synchronously on the caller's goroutine. Events carry the operation kind, the
// subject, IDDigest of the session ID, a Verdict, a count (the number of live
// sessions a subject revocation deleted, whether a digest revocation deleted
// one, the number of keys an attribute patch named, or one per swept session),
// and the store error when one occurred. They never carry the session ID,
// attribute values, or the credential: the ID is the bearer token and
// attributes routinely hold identity data, so neither may reach a log line or a
// metrics label. Register observers with WithObserver (repeatable, notified in
// order) or contribute them from another plugin by exporting an
// ObserverContributor from its Definition. Observe must return quickly and must
// not block; a panicking observer is recovered, recorded through the log facade
// without session data, and does not affect the session operation or the
// observers after it.
//
// # Expiry events
//
// EventExpired reports a session the MemoryStore cleanup sweep removed after
// its absolute lifetime or idle lease passed. Digest is IDDigest of the pruned
// session, Subject is the subject the stored record carried, and the event has
// no field for the ID or the attributes. Only the memory backend's sweeper
// emits it: the Redis backend does not, because reliable expiry notifications
// would need keyspace-notification configuration on the Redis server, which a
// shared deployment cannot assume and which this package must not require.
// Lazy pruning (Get, Touch, ListBySubject, RevokeBySubject) does not emit it
// either; a dead session that is only ever pruned lazily therefore ends
// silently. The plugin installs its observers on the MemoryStore it creates and
// on a MemoryStore injected with WithStore, so expiry events reach the same
// observers as the operation events.
//
// # Alignment with Spring Session and Spring Security
//
// The capability set maps onto the Spring vocabulary as follows.
//
//	Spring Session / Security         this package
//	SessionRepository                 Store (Create, Get, Touch, Rotate, Delete)
//	FindByIndexNameSessionRepository  IndexedStore (ListBySubject, RevokeBySubject, RevokeByDigest)
//	changeSessionId / migrateSession  Rotate (a new ID, same absolute lifetime)
//	getLastAccessedTime, maxInactive  LastSeenAt, idle_ttl with touch_interval throttling
//	setAttribute / removeAttribute    PatchAttributes (set, unset)
//	HttpSessionIdResolver             the transport adapter's extractor list
//	Created / Deleted / Expired       EventCreated, EventRevoked, EventExpired
//	maximumSessions                   max_sessions
//	maxSessionsPreventsLogin          on_exceed: reject_new
//	SessionRegistry.expireNow         RevokeByDigest
//	spring.session.timeout, cookie.*  ttl, idle_ttl, name, path, domain, secure, ...
//
// Deliberate differences remain. This package adds an absolute ttl on top of
// the idle lease, stricter than Spring's default inactivity timeout. An invalid,
// expired, revoked, or evicted credential all collapse into one "invalid
// credential" rejection, and no session-creation policy or "expired elsewhere"
// page exists here: the former belongs to the host, and the latter needs a
// tombstone that would make session existence probeable.
//
// Importing this package has no registration side effects. Applications that
// use XBC's process-wide catalog opt in with a blank import of
// github.com/xbcio/xbc/extensions/authentication/session/autoload;
// applications with a private catalog can use Definition directly.
//
// # Subject index
//
// Manager.ListBySubject, Manager.RevokeBySubject, and Manager.RevokeByDigest
// work on a subject's sessions without holding their IDs: the activity list
// behind a "my devices" page, signing a user out everywhere, and ending one
// listed session. ListBySubject returns SessionInfo values in no particular
// order; RevokeBySubject deletes every live session of the subject and returns
// how many it deleted; RevokeByDigest deletes one session named by
// SessionInfo.Digest only if it belongs to the subject, reports false for an
// unknown or foreign digest, and returns an error for a malformed one. Each
// method returns ErrStopped once the plugin stopped and a parameter error for
// an empty subject.
//
// These methods need a Store that also implements the optional IndexedStore
// capability. The built-in memory and Redis stores do; a custom Store injected
// with WithStore need not, and Manager then returns ErrNotSupported. Sessions
// created before a deployment adopted the index are not indexed and expire on
// their own schedule.
//
// SessionInfo.Digest, like IDDigest, is a handle that names a session, not a
// credential: it carries no session ID and cannot authenticate as the session.
// SessionInfo.LastSeenAt is the time the idle lease was last renewed. Touch
// throttles renewal to touch_interval, so it is approximate and can trail the
// latest request by up to that interval; a store that cannot report it leaves
// it zero.
//
// Change the subject's credentials first and call RevokeBySubject second. A
// session created while the revocation runs may survive it, so repeat the
// revocation when a guarantee against in-flight logins matters. The Redis
// backend keeps the index in one set per subject and addresses a single key per
// operation, which cluster clients require; the steps that span the session key
// and the index key are therefore separate commands, not a transaction. A
// failure between them leaves a harmless stale index member that the next list
// or revocation prunes.
package session
