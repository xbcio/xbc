// Package session provides protocol-neutral, opaque server-side sessions for
// XBC.
//
// A session ID is a cryptographically random identifier. Built-in stores hash
// that identifier before using it as a storage key, and all identity and
// attribute data remains server-side. This package owns everything about a
// session except how its ID travels: configuration, the memory and Redis
// stores, the Manager that creates, rotates, and revokes sessions, and the
// authentication.Authenticator for the "session" scheme. The Web adapter at
// github.com/xbcio/xbc/transport/web/extensions/authentication/session carries
// the ID in an HTTP cookie configured by Cookie and bounded by Lifetime.
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
