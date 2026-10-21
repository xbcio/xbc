// Package ldap verifies a user name and password against an LDAP directory and
// turns the matched entry into a federation.ExternalIdentity. It is a protocol
// client only. It registers no routes, creates no sessions, and never decides
// which local user an external identity belongs to -- the verified result
// travels to the application's IdentityResolver.
//
// LDAP is password-based, so there is no browser redirect and no web adapter.
// The application's own login handler calls Verify and then follows the same
// session hand-over as the redirect protocols: resolve the external identity,
// revoke any session the request already carried, and create a new session
// whose subject is the local user ID.
//
// # Verification flow
//
// Verify is deliberately search-then-bind, because a simple bind alone cannot
// tell an application which entry a user name belongs to:
//
//  1. An empty user name or password is rejected before any network call.
//     Empty passwords are never sent to a directory: many servers treat an
//     empty simple-bind password as an anonymous bind, which succeeds.
//  2. A connection is opened with TLS and bound as the configured
//     least-privilege service account.
//  3. The configured filter is searched under BaseDN with the user name
//     escaped per RFC 4515. Exactly one entry must match; zero and multiple
//     matches are both a plain credential failure.
//  4. A second, fresh connection binds as the matched entry's distinguished
//     name with the supplied password. The service connection is never rebound
//     as the user.
//  5. The external subject is read from SubjectAttribute (entryUUID by
//     default). It must be present, unambiguous, non-empty, and valid UTF-8;
//     there is no fallback to uid or mail. Configured claim attributes are
//     copied into Claims.
//
// # Failure semantics
//
// Every credential outcome -- unknown user, wrong password, multiple matches,
// empty user name, empty password -- returns exactly ErrInvalidCredentials,
// with no detail that would let a caller or an end user distinguish them. The
// caller must map it onto the same generic login failure it uses elsewhere.
// Directory and transport failures return ErrUnavailable, a rejected address
// returns ErrAddressRejected, and a matched entry that cannot yield an
// identity returns ErrInvalidDirectoryEntry.
//
// Error text never contains the service password, the user password, a
// distinguished name, an attribute value, the search filter, or the user name.
// Detailed directory diagnostics are deliberately not wrapped into errors for
// that reason.
//
// Diagnostic views are narrower than error text. Config and Provider String
// and Format always redact the service password -- for a Config value, a
// Provider value, or a *Provider alike. These views intentionally render the
// configured URL, ServiceBindDN, BaseDN, and UserFilter so an operator can
// diagnose a deployment. Those values are configuration, not credentials.
//
// # TLS and timeouts
//
// TLS is mandatory. The URL scheme is ldaps for implicit TLS, or ldap with
// StartTLS for explicit TLS; plaintext LDAP is rejected at configuration time
// and never dialed. Certificate-chain and host-name verification are always
// on: RootCAs can add trust anchors but there is no switch that disables
// verification, and the TLS floor is 1.2. Every connection attempt is bounded
// by DialTimeout and every bind, search, and TLS handshake by
// OperationTimeout; the caller's context bounds the whole call.
//
// # Egress address policy
//
// The directory host is resolved once per connection attempt, and every
// resolved address must pass the same policy the federation HTTP clients use:
// loopback, private, carrier-grade NAT, link-local, and special-purpose
// addresses are rejected unless AllowPrivate is set, and a mixed
// public/private answer is rejected as a whole. Link-local addresses stay
// rejected even with AllowPrivate, because cloud metadata endpoints live
// there. The connection is made to the validated numeric address, not to the
// host name, so a resolver that changes its answer cannot redirect the socket.
// AllowPrivate is a development switch and must stay disabled in production.
//
// # Usage
//
// Construct a provider from static configuration and register it under its
// name. Registration performs no network I/O:
//
//	provider, err := ldap.New(ldap.Config{
//		Name:                "corp",
//		Issuer:              "ldaps://directory.example.com",
//		URL:                 "ldaps://directory.example.com:636",
//		ServiceBindDN:       "cn=xbc-search,ou=services,dc=example,dc=com",
//		ServiceBindPassword: outbound.Secret(secret),
//		BaseDN:              "ou=people,dc=example,dc=com",
//		UserFilter:          "(uid=%s)",
//		ClaimAttributes:     map[string]string{"email": "mail", "display_name": "cn"},
//	})
//	if err != nil {
//		return err
//	}
//	registry := ldap.NewRegistry()
//	if err := registry.Put("corp", provider); err != nil {
//		return err
//	}
//	// In the application's login handler:
//	identity, err := ldap.Verify(ctx, provider, username, password)
//	if errors.Is(err, ldap.ErrInvalidCredentials) {
//		// Render the same generic failure for every credential outcome.
//	}
//
// Register the provider through ldap.NewRegistry, or through
// federation.NewRegistry with ldap.ValidateProvider when composing additional
// validation. A Put that fails static validation returns an error and leaves
// the previous provider in effect; Remove affects only subsequent logins and
// never touches sessions the application already created.
//
// # Lifecycle
//
// A Provider starts no goroutines and holds no connection pool: each Verify
// call opens and closes its own connections inline. The package therefore
// implements neither plugin.Drainer nor Stop, and retiring a provider needs no
// shutdown call. Rate limiting and account lockout are the application's
// responsibility; this package reports failures but never counts or throttles
// them.
package ldap
