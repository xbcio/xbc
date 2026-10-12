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
package session
