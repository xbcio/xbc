package federation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

var (
	// ErrNilResolver reports a nil or typed-nil IdentityResolver.
	ErrNilResolver = errors.New("federation: nil identity resolver")
	// ErrNilContext reports a nil context passed to ResolveIdentity.
	ErrNilContext = errors.New("federation: nil context")
	// ErrInvalidResolution reports a Resolution that was not produced by
	// Resolved or Refused, such as the zero value returned by a resolver that
	// never ran its own logic.
	ErrInvalidResolution = errors.New("federation: invalid resolution")
	// ErrEmptySubject reports a resolved Resolution with no local subject.
	ErrEmptySubject = errors.New("federation: resolved subject is empty")
)

// ExternalIdentity is an external identity that a protocol module has fully
// verified. The application never receives an unverified assertion.
//
// Subject must be stable within (Provider, Issuer) and must never be reused
// for a different person. It is never an email address or a user name.
//
// Claims carries normalized attributes of the external identity. The
// constructor owns defensive copying: a protocol module must not retain or
// later mutate a map it published here. Claim values must be JSON-safe.
type ExternalIdentity struct {
	// Protocol is the protocol that verified the identity: "oidc", "oauth2",
	// "saml", or "ldap".
	Protocol string
	// Provider is the application-chosen, stable identifier of the IdP in the
	// protocol module's registry.
	Provider string
	// Issuer is the OIDC iss claim, the SAML IdP entityID, or the LDAP
	// directory identifier.
	Issuer string
	// Subject is the IdP's stable account identifier.
	Subject string
	// Claims carries the normalized verified attributes.
	Claims map[string]any
	// AuthTime is the instant the external authentication happened.
	AuthTime time.Time
}

// IdentityResolver is implemented by the application to map a verified
// ExternalIdentity onto a local subject and its session attributes.
//
// A returned error is an operational failure and is handled exactly like a
// refusal: the login fails and no session is created. Returning a Resolution
// built by Resolved with an empty subject is an operational failure too.
//
// Resolve is called only after the protocol module verified the external
// identity; implementations must not treat the request as authenticated on
// their own.
type IdentityResolver interface {
	Resolve(ctx context.Context, identity ExternalIdentity) (Resolution, error)
}

// RefusalReason classifies why an application refused a verified external
// identity. Reasons are for internal logs and diagnostics only: a web adapter
// maps every refusal onto the same outward failure landing page and never
// renders the reason, the identity, or any IdP text to the browser.
type RefusalReason string

const (
	// RefusalUnbound means no local account is bound to this external identity.
	RefusalUnbound RefusalReason = "unbound"
	// RefusalDisabled means the local account or its provider binding is
	// disabled.
	RefusalDisabled RefusalReason = "disabled"
	// RefusalBindingRequired means the application requires an explicit
	// binding step before this identity may log in.
	RefusalBindingRequired RefusalReason = "binding_required"
	// RefusalOther is the fallback for a refusal the application does not
	// classify further.
	RefusalOther RefusalReason = "other"
)

// resolutionStatus is the closed set of Resolution states. The zero state is
// deliberately invalid so that an uninitialized Resolution can never be
// mistaken for a refusal.
type resolutionStatus uint8

const (
	resolutionInvalid resolutionStatus = iota
	resolutionResolved
	resolutionRefused
)

// Resolution is a closed identity resolution outcome. It is created by
// Resolved or Refused; its fields are private so no other value can be
// constructed.
type Resolution struct {
	status     resolutionStatus
	subject    string
	attributes map[string]any
	refusal    RefusalReason
}

// Resolved creates a successful resolution for a local subject. The subject
// is trimmed and must not be empty; an empty subject is reported by Validate
// and by ResolveIdentity as an operational failure. Attributes are copied, so
// later mutations of the caller's map do not affect the resolution.
func Resolved(subject string, attributes map[string]any) Resolution {
	return Resolution{
		status:     resolutionResolved,
		subject:    strings.TrimSpace(subject),
		attributes: cloneAttributes(attributes),
	}
}

// Refused creates a refusal. An empty reason is normalized to RefusalOther.
func Refused(reason RefusalReason) Resolution {
	if reason == "" {
		reason = RefusalOther
	}
	return Resolution{status: resolutionRefused, refusal: reason}
}

// Validate reports whether the resolution is a well-formed outcome: a zero or
// otherwise unconstructed Resolution is invalid, a resolved resolution needs a
// non-empty subject and JSON-safe attributes, and a refusal is always valid.
func (r Resolution) Validate() error {
	switch r.status {
	case resolutionResolved:
		if r.subject == "" {
			return ErrEmptySubject
		}
		if _, err := marshalAttributes(r.attributes); err != nil {
			return err
		}
		return nil
	case resolutionRefused:
		return nil
	default:
		return ErrInvalidResolution
	}
}

// IsResolved reports whether the resolution names a local subject. A refusal
// and the zero value both report false.
func (r Resolution) IsResolved() bool { return r.status == resolutionResolved }

// Subject returns the local subject of a resolved resolution and the empty
// string otherwise.
func (r Resolution) Subject() string {
	if r.status != resolutionResolved {
		return ""
	}
	return r.subject
}

// Attributes returns a defensive copy of the resolved session attributes, or
// nil when the resolution is not resolved.
func (r Resolution) Attributes() map[string]any {
	if r.status != resolutionResolved {
		return nil
	}
	return cloneAttributes(r.attributes)
}

// Refusal returns the refusal reason and whether this resolution is a refusal.
func (r Resolution) Refusal() (RefusalReason, bool) {
	if r.status != resolutionRefused {
		return "", false
	}
	return r.refusal, true
}

// ResolveIdentity calls resolver under the federation contract and reports
// every failure as an operational error: a nil context, a nil or typed-nil
// resolver, a resolver error, and an invalid resolution (including an empty
// subject or attributes that are not JSON-safe) never produce a successful
// outcome. It returns a refused resolution unchanged; refusing is ordinary
// control flow, not an error.
func ResolveIdentity(ctx context.Context, resolver IdentityResolver, identity ExternalIdentity) (Resolution, error) {
	if ctx == nil {
		return Resolution{}, ErrNilContext
	}
	if nilIdentityResolver(resolver) {
		return Resolution{}, ErrNilResolver
	}
	resolution, err := resolver.Resolve(ctx, identity)
	if err != nil {
		return Resolution{}, fmt.Errorf("federation: identity resolution failed: %w", err)
	}
	if err := resolution.Validate(); err != nil {
		return Resolution{}, err
	}
	return resolution, nil
}

// nilIdentityResolver reports whether resolver is nil or a typed-nil value
// stored in the interface. Both call through to a panic or a meaningless zero
// outcome if left unchecked.
func nilIdentityResolver(resolver IdentityResolver) bool {
	if resolver == nil {
		return true
	}
	value := reflect.ValueOf(resolver)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
