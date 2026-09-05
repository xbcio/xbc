// Package security defines protocol-neutral authentication contracts and the
// ordered first-applicable authentication policy.
//
// Transport packages are responsible for extracting credentials. They expose
// the extraction outcome to Manager through CredentialSource; Manager then
// applies route scheme selection, ambiguity checks, and authentication without
// knowing anything about HTTP, Gin, headers, cookies, or other representations.
//
// Result values are closed: callers can create only Accepted and Rejected
// authenticator outcomes, and Manager creates the remaining rejection kinds.
// Operational failures are returned as Go errors, never encoded as ordinary
// credential rejection.
package security
