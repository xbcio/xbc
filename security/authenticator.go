package security

import "context"

// SafeReason is a reason that the producer explicitly considers safe to expose
// to an unauthenticated caller. It must never contain credentials, verifier
// details, secrets, or sensitive principal data.
type SafeReason string

// Challenge is an opaque, transport-renderable authentication challenge. The
// transport that created or understands a challenge owns its final encoding.
type Challenge string

const (
	// ReasonUnauthenticated is used when no accepted scheme supplied credential
	// evidence.
	ReasonUnauthenticated SafeReason = "authentication required"
	// ReasonInvalidCredential is the safe fallback for an authenticator
	// rejection that omitted a reason.
	ReasonInvalidCredential SafeReason = "invalid credential"
	// ReasonMalformedCredential is the safe fallback for malformed credential
	// evidence that omitted a reason.
	ReasonMalformedCredential SafeReason = "malformed credential"
	// ReasonAmbiguousCredentials is used when more than one accepted scheme
	// supplied credential evidence.
	ReasonAmbiguousCredentials SafeReason = "multiple credentials presented"
)

// Credential holds one opaque, protocol-neutral credential. It deliberately
// does not implement fmt.Stringer so generic diagnostics cannot accidentally
// render credential material.
type Credential struct {
	value any
}

// Value returns the opaque value supplied by the credential extractor.
func (c Credential) Value() any { return c.value }

// CredentialAs performs a typed read of an opaque credential.
func CredentialAs[T any](credential Credential) (T, bool) {
	value, ok := credential.value.(T)
	return value, ok
}

// CredentialStatus is the closed set of credential extraction states.
type CredentialStatus uint8

const (
	// CredentialStatusAbsent means no credential of the scheme was presented.
	CredentialStatusAbsent CredentialStatus = iota + 1
	// CredentialStatusPresented means one opaque credential was extracted.
	CredentialStatusPresented
	// CredentialStatusMalformed means credential syntax was present but invalid.
	CredentialStatusMalformed
)

// String returns a stable diagnostic name for the status.
func (s CredentialStatus) String() string {
	switch s {
	case CredentialStatusAbsent:
		return "absent"
	case CredentialStatusPresented:
		return "presented"
	case CredentialStatusMalformed:
		return "malformed"
	default:
		return "invalid"
	}
}

// CredentialResult is a closed extraction outcome. Its fields are private so
// only the constructors below can create a non-zero state.
type CredentialResult struct {
	status     CredentialStatus
	credential any
	reason     SafeReason
	challenge  Challenge
}

// Absent reports that no credential of a scheme was presented.
func Absent() CredentialResult {
	return CredentialResult{status: CredentialStatusAbsent}
}

// AbsentWithChallenge reports no credential and supplies the challenge to use
// if every route-accepted scheme is absent.
func AbsentWithChallenge(challenge Challenge) CredentialResult {
	return CredentialResult{status: CredentialStatusAbsent, challenge: challenge}
}

// Presented reports one opaque credential. A nil or typed-nil value is invalid
// and Manager reports it as an operational extraction failure.
func Presented(credential any) CredentialResult {
	return CredentialResult{
		status:     CredentialStatusPresented,
		credential: credential,
	}
}

// Malformed reports credential syntax that was present but invalid.
func Malformed(reason SafeReason) CredentialResult {
	return malformed(reason, "")
}

// MalformedWithChallenge reports malformed credential syntax and supplies an
// optional transport challenge.
func MalformedWithChallenge(reason SafeReason, challenge Challenge) CredentialResult {
	return malformed(reason, challenge)
}

func malformed(reason SafeReason, challenge Challenge) CredentialResult {
	if reason == "" {
		reason = ReasonMalformedCredential
	}
	return CredentialResult{
		status:    CredentialStatusMalformed,
		reason:    reason,
		challenge: challenge,
	}
}

// Status returns the extraction status. The zero CredentialResult has an
// invalid status and is rejected by Manager.
func (r CredentialResult) Status() CredentialStatus { return r.status }

// Credential returns the presented credential, if any.
func (r CredentialResult) Credential() (Credential, bool) {
	if r.status != CredentialStatusPresented {
		return Credential{}, false
	}
	return Credential{value: r.credential}, true
}

// Reason returns the safe malformed-credential reason, if any.
func (r CredentialResult) Reason() (SafeReason, bool) {
	if r.status != CredentialStatusMalformed {
		return "", false
	}
	return r.reason, true
}

// Challenge returns the optional challenge associated with this extraction
// outcome.
func (r CredentialResult) Challenge() (Challenge, bool) {
	if r.challenge == "" {
		return "", false
	}
	return r.challenge, true
}

// CredentialSource supplies one extraction result for each scheme requested by
// Manager. A transport adapter normally implements this interface by invoking
// its scheme-specific extractor. Manager calls it exactly once for every
// selected scheme, in authentication-domain order, before verification.
type CredentialSource interface {
	Credential(context.Context, Scheme) (CredentialResult, error)
}

// CredentialSourceFunc adapts a function to CredentialSource.
type CredentialSourceFunc func(context.Context, Scheme) (CredentialResult, error)

// Credential implements CredentialSource.
func (f CredentialSourceFunc) Credential(ctx context.Context, scheme Scheme) (CredentialResult, error) {
	return f(ctx, scheme)
}

// Authenticator verifies credentials for one stable scheme. Rejected
// credentials must be returned with Rejected or RejectedWithChallenge. A Go
// error is reserved for operational failure and is terminal.
type Authenticator interface {
	Scheme() Scheme
	Authenticate(context.Context, Credential) (Result, error)
}

// ResultStatus is the closed set of manager result states.
type ResultStatus uint8

const (
	// ResultStatusAuthenticated means verification succeeded.
	ResultStatusAuthenticated ResultStatus = iota + 1
	// ResultStatusRejected means authentication did not produce a principal.
	ResultStatusRejected
)

// String returns a stable diagnostic name for the status.
func (s ResultStatus) String() string {
	switch s {
	case ResultStatusAuthenticated:
		return "authenticated"
	case ResultStatusRejected:
		return "rejected"
	default:
		return "invalid"
	}
}

// RejectionKind classifies a safe, non-operational rejection.
type RejectionKind uint8

const (
	// RejectionInvalidCredential means the sole presented credential failed
	// verification.
	RejectionInvalidCredential RejectionKind = iota + 1
	// RejectionMalformedCredential means the sole evidence was malformed.
	RejectionMalformedCredential
	// RejectionAmbiguousCredentials means multiple accepted schemes supplied
	// credential evidence.
	RejectionAmbiguousCredentials
	// RejectionUnauthenticated means every accepted scheme was absent.
	RejectionUnauthenticated
)

// String returns a stable diagnostic name for the rejection kind.
func (k RejectionKind) String() string {
	switch k {
	case RejectionInvalidCredential:
		return "invalid-credential"
	case RejectionMalformedCredential:
		return "malformed-credential"
	case RejectionAmbiguousCredentials:
		return "ambiguous-credentials"
	case RejectionUnauthenticated:
		return "unauthenticated"
	default:
		return "invalid"
	}
}

// Result is a closed authentication outcome. Authenticator implementations can
// create only Accepted and Rejected outcomes. Manager attaches the effective
// scheme and creates malformed, ambiguous, and unauthenticated rejections.
// Operational failures are returned separately as errors.
type Result struct {
	status     ResultStatus
	rejection  RejectionKind
	scheme     Scheme
	principal  any
	reason     SafeReason
	challenges []Challenge
}

// Accepted creates a successful Authenticator outcome. A nil or typed-nil
// principal is invalid and Manager reports it as an operational authenticator
// failure.
func Accepted(principal any) Result {
	return Result{status: ResultStatusAuthenticated, principal: principal}
}

// Rejected creates an ordinary invalid-credential Authenticator outcome.
func Rejected(reason SafeReason) Result {
	return rejectedAuthenticatorResult(reason, "")
}

// RejectedWithChallenge creates an ordinary invalid-credential Authenticator
// outcome with an optional challenge.
func RejectedWithChallenge(reason SafeReason, challenge Challenge) Result {
	return rejectedAuthenticatorResult(reason, challenge)
}

func rejectedAuthenticatorResult(reason SafeReason, challenge Challenge) Result {
	if reason == "" {
		reason = ReasonInvalidCredential
	}
	var challenges []Challenge
	if challenge != "" {
		challenges = []Challenge{challenge}
	}
	return Result{
		status:     ResultStatusRejected,
		rejection:  RejectionInvalidCredential,
		reason:     reason,
		challenges: challenges,
	}
}

// Status returns the result status. The zero Result has an invalid status and
// is rejected by Manager when returned by an Authenticator.
func (r Result) Status() ResultStatus { return r.status }

// Authenticated reports whether the result contains an authenticated principal.
func (r Result) Authenticated() bool { return r.status == ResultStatusAuthenticated }

// Rejected reports whether the result is an ordinary, non-operational rejection.
func (r Result) Rejected() bool { return r.status == ResultStatusRejected }

// Principal returns the authenticated principal, if any.
func (r Result) Principal() (any, bool) {
	if r.status != ResultStatusAuthenticated {
		return nil, false
	}
	return r.principal, true
}

// Scheme returns the scheme responsible for an authenticated result or a
// scheme-specific rejection. Ambiguous and unauthenticated results have no
// single scheme.
func (r Result) Scheme() (Scheme, bool) {
	if r.scheme == "" {
		return "", false
	}
	return r.scheme, true
}

// Rejection returns the rejection classification, if this result was rejected.
func (r Result) Rejection() (RejectionKind, bool) {
	if r.status != ResultStatusRejected {
		return 0, false
	}
	return r.rejection, true
}

// Reason returns the safe response reason for a rejection.
func (r Result) Reason() (SafeReason, bool) {
	if r.status != ResultStatusRejected {
		return "", false
	}
	return r.reason, true
}

// Challenges returns a defensive copy of the ordered, de-duplicated challenges.
func (r Result) Challenges() []Challenge {
	return append([]Challenge(nil), r.challenges...)
}

// RequiresPrincipal marks a downstream component that must run only after an
// authenticated principal has been established. A transport may use this
// marker to add a framework-owned ordering edge in the correct direction.
type RequiresPrincipal interface {
	RequiresPrincipal()
}
