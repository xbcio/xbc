// Package outbound is the shared outbound HTTP client for federation protocol
// modules (OIDC, OAuth2, SAML, LDAP). Discovery documents, JWKS sets,
// SAML metadata, userinfo responses, and token endpoints all leave the process
// through one policy instead of each module carrying its own copy.
//
// # Policy
//
// HTTPS is required. Plain HTTP is admitted only through the explicit
// AllowHTTP development switch, which must never be enabled in production.
// Certificate verification and a TLS 1.2 floor are not configurable.
//
// Targets must resolve to public addresses. Loopback, private, link-local,
// carrier-grade NAT, unique-local, unspecified, multicast, and the other
// special-use ranges are rejected; an answer that mixes public and non-public
// addresses is rejected as a whole, because picking only the public half would
// let resolver ordering decide which policy result is observed. Cloud metadata
// endpoints live in the link-local range and are rejected for that reason.
//
// Enforcement happens twice. Before dialing, every resolved address is
// validated and the connection is made to the validated numeric address, never
// to the original host name. At connect time, the dialer's Control hook
// validates the address actually being connected, so a resolver that changes
// its answer between validation and dial cannot redirect the connection.
//
// Redirects are followed only for GET and HEAD requests -- a redirected POST
// (a token or userinfo exchange) is rejected rather than replayed at a new
// location. Every hop is re-validated against the policy, an HTTPS-to-HTTP
// downgrade is rejected, the configured hop count is enforced, and
// Authorization, Cookie, and the other credential-bearing headers are stripped
// when a redirect leaves the original origin.
//
// # Limits
//
// Policy carries dial, TLS handshake, response-header, and total timeouts, and
// ReadBody applies a response-body limit. JWKS responses may pass a different
// limit explicitly.
//
// # Secrets
//
// Secret is the type for client_secret and comparable material. It implements
// fmt.Formatter, so the fmt verbs that dispatch methods -- including %#v and
// invalid verbs such as %d -- render [REDACTED]. Converting it to string is the
// deliberate, single point of declassification. The one exception is %p: fmt
// handles that verb before any method dispatch, so it must never be applied to
// a Secret value.
//
// That protection covers the value itself when it is formatted or logged as a
// Secret; it does not cover every consumer of a structure that embeds one.
// encoding/json sees only the underlying string, so a structure that embeds a
// Secret renders the raw value when serialized as JSON. A carrying type that
// can appear in serialized diagnostics must redact its own serialized form
// rather than relying on this type alone.
//
// # Lifecycle
//
// This package starts no goroutines and owns no background work, so it needs
// neither Drain nor Stop. NewClient returns an ordinary *http.Client whose
// transport keeps idle connections; a caller that manages a long-lived client
// may call CloseIdleConnections when it shuts its own component down.
package outbound
