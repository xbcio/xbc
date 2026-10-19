// Package saml implements the service-provider half of SAML 2.0 web browser
// SSO: SP-initiated AuthnRequests over the HTTP-Redirect binding, assertion
// validation of HTTP-POST responses, SP metadata generation, and explicit IdP
// metadata loading. It is a protocol client only. It registers no routes,
// creates no sessions, and never decides which local user an external identity
// belongs to -- the verified result travels to the application's
// IdentityResolver.
//
// The web adapter (a separate module) owns the /start, /acs, and /metadata
// routes and the one-time login transaction that binds a response to the
// request ID generated here.
//
// # Request flow
//
// NewAuthnRequest generates a 128-bit request ID, builds an AuthnRequest,
// deflates and base64-encodes it, and returns the IdP redirect URL. When
// SPPrivateKey is configured the query string is signed with RSA-SHA256 over
// exactly the octets the redirect binding defines. RelayState is opaque,
// bounded application state.
//
// # Response validation
//
// Verify fails closed and never trusts an unverified tree:
//
//  1. The document size, depth, and XML shape are bounded. DOCTYPE and ENTITY
//     declarations are rejected before parsing even though Go's XML decoder
//     does not resolve external entities; directives, duplicate attributes,
//     duplicate IDs, and malformed IDs are rejected after parsing.
//  2. Exactly one Response, one Assertion, and one Signature are required.
//     Encrypted assertions and attributes are rejected. An assertion nested
//     anywhere except as the single direct child is an ambiguity and is
//     rejected.
//  3. The Signature must be a direct child of the element its single Reference
//     URI names: the response or the assertion. inspectSignature enforces one
//     SignedInfo, one SignatureValue, one Reference, SHA-2 signature and digest
//     algorithms from the configured allowlist, and exactly the
//     enveloped-signature plus canonicalization transform chain. SHA-1 is
//     rejected even if configured.
//  4. The signature is verified over that referenced element with the trusted
//     IdP certificates. Multiple certificates may be configured so a rotation
//     period accepts the old and the new certificate at the same time.
//  5. Issuer, Status, Destination, Audience, Recipient, InResponseTo,
//     NotBefore, NotOnOrAfter, the NameID format allowlist, and the bearer
//     SubjectConfirmationData are all checked against the configuration and
//     the expected request ID. Conditions use an injected clock and a leeway
//     capped at 60 seconds.
//  6. The assertion ID is claimed atomically in the replay cache until the end
//     of its validity window. A duplicate is ErrReplay. A cache failure is
//     ErrUnavailable: the login never succeeds when replay protection is
//     unavailable.
//
// # Failure semantics
//
// Every response failure wraps ErrInvalidResponse, ErrReplay, or ErrUnavailable.
// errors.Is distinguishes only those categories; the wrapped reason -- which
// stage failed, such as an expired assertion or a bad signature -- exists for
// diagnostics, and a caller such as an assertion consumer service must render a
// generic failure instead of exposing it. Error text never carries assertion
// content, a NameID, an attribute value, a certificate, or a key. Configuration
// failures are ErrInvalidConfig.
//
// # Issuer and subject
//
// ExternalIdentity.Issuer is the IdP entityID. Subject is the NameID, accepted
// only in a configured format: persistent is the default, unspecified is an
// explicit opt-in, and transient and email-address formats can never be
// allowed because a subject must be stable and must never be an email address.
// Claims come from the configured attribute mapping; every configured
// attribute must be present, values must be text and valid UTF-8, and the
// result is a fresh map.
//
// # Metadata
//
// Provider.Metadata renders SP metadata with the entity ID, the HTTP-POST
// assertion consumer service, the NameID formats, and the public signing
// certificate. It never renders private key material. LoadMetadata is the only
// network call in this package: it explicitly fetches IdP metadata through the
// shared federation/outbound policy, which enforces HTTPS and rejects
// non-public addresses. A refresh is LoadMetadata followed by New and
// Registry.Put; a live provider is never mutated.
//
// # Lifecycle
//
// A Provider starts no goroutines and holds no connection pool. Verify and
// NewAuthnRequest are pure request handling, and LoadMetadata uses an ordinary
// HTTP client that is closed with the response. The package therefore
// implements neither plugin.Drainer nor Stop, and retiring a provider needs no
// shutdown call. MemoryReplayCache is single-replica by design; a
// multi-replica deployment must supply a shared ReplayCache.
package saml
