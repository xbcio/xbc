// Package federation defines the protocol-neutral contract between external
// identity federation protocols (OIDC, OAuth2, SAML, LDAP) and the application
// that owns identity policy.
//
// A protocol module verifies an external credential -- a signed ID token, a
// signed SAML assertion, or an LDAP bind -- and only after that verification
// constructs an ExternalIdentity. It never decides which local user that
// identity belongs to. The application implements IdentityResolver and answers
// that question, because account binding, just-in-time provisioning, group
// mapping, and the kill switch that disables a provider are application
// policy, not protocol mechanics.
//
// # Contract
//
// The binding key is (Provider, Issuer, Subject). Protocol modules must never
// associate accounts by email or user name; email_verified travels only as a
// claim, and whether it may drive automatic binding is an application decision.
//
// Resolution is closed: values are created by Resolved or Refused and by
// nothing else, so a zero or malformed Resolution is detectable. ResolveIdentity
// turns every failure mode -- a nil resolver, a typed-nil resolver, a resolver
// error, and a resolved resolution with an empty subject -- into an operational
// error, never into a successful login.
//
// Session attributes are assembled by SessionAttributes: the application's
// attributes plus the reserved keys idp_protocol, idp, idp_issuer, and
// auth_time. An application attribute that uses a reserved key is an
// operational failure. The merged set must be JSON-safe and must encode to no
// more than MaxSessionAttributesBytes.
//
// Registry is the shared concurrency-safe provider registry. Each protocol
// module instantiates it with its own configuration type and static validator;
// Put replaces a provider atomically and a rejected configuration leaves the
// previous one in effect.
//
// # Lifecycle
//
// This package starts no goroutines, owns no connections, and performs no
// asynchronous work, so it implements neither plugin.Drainer nor a Stop
// method. Registry.Remove affects only subsequent logins: sessions created
// before a provider is removed stay valid until the application revokes them.
package federation
