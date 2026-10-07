// Package authentication defines protocol-neutral authentication contracts and the
// ordered first-applicable authentication policy.
//
// Transport packages are responsible for extracting credentials. They expose
// the extraction outcome to Manager through CredentialSource; Manager then
// applies route scheme selection and ordered first-applicable authentication
// without knowing anything about HTTP, Gin, headers, cookies, or other
// representations.
//
// Result values are closed: callers can create only Accepted and Rejected
// authenticator outcomes, and Manager creates the remaining rejection kinds.
// Operational failures are returned as Go errors, never encoded as ordinary
// credential rejection.
//
// Result has two distinct successful outcomes. Accepted(principal) is the
// ordinary case: a request authenticated as a specific principal. A nil or
// typed-nil principal there is always an operational failure, never a
// successful outcome -- Accepted(nil) does not mean "no principal". Only
// AcceptedWithoutPrincipal expresses that: a verified request whose caller is
// trusted but is not a natural person (a gateway signature, a
// service-to-service call), where fabricating a placeholder subject would
// pollute tenant resolution, authorization decisions, and audit records with
// a value nobody chose. An authorization layer that requires a subject --
// tenant resolution among them -- must treat AcceptedWithoutPrincipal as
// insufficient and refuse, not as an exemption from needing one: being
// authenticated and being exempt from a subject requirement are different
// facts, and conflating them would let a trusted-but-subjectless caller
// bypass checks meant for a specific identity.
package authentication
