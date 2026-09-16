package web

import (
	"fmt"
	"sort"

	"github.com/xbcio/xbc/plugin"
)

// reservedMiddlewareKeys are the middleware identities Server assembles itself.
// They are ordering anchors, not plugin Definitions, and the distinction is
// deliberate rather than provisional.
//
// The common rule is that a framework-owned ordering pin makes each of these
// stages required, and a required stage cannot be an opt-in plugin: it would be
// selectable at the composition root and disableable through
// plugins.<key>.enabled, and neither omission has a coherent meaning.
//
// Authentication enforcement is a framework guarantee: web.security defaults to
// deny, and Start fails when a route needs authentication that no registered
// authenticator can satisfy. Omitting its Bundle would silently serve every
// route unauthenticated, which is exactly the failure the deny default exists to
// prevent. It also enforces the Server Definition's own web.security section,
// and config.NewUniverse gives each section exactly one owning plugin, so a
// second Definition cannot claim it without first moving the section out of
// web.Config.
//
// The error boundary is the outermost PhaseError middleware, the scope every
// contributed ErrorMapper runs in and the last stop where an unknown error
// still becomes a safe non-leaking Problem Detail. As a Definition it added no
// selectability -- it had no configuration and always travelled inside
// web.Bundle() -- while disabling it reported an unsatisfiable internal
// ordering pin rather than the rule it broke.
//
// What the anchors are for is ordering: a contributed middleware declares
// After: Require(AuthenticationMiddlewareKey) so authorization always observes a
// published Principal, and a focused error middleware sits inside
// ErrorBoundaryKey by phase. Because those anchors live in the same flat
// namespace as plugin keys, the reservation has to be explicit and enforced --
// otherwise a plugin could claim one and the only symptom would be a
// duplicate-identity failure at Start.
var reservedMiddlewareKeys = map[plugin.Key]string{
	AuthenticationMiddlewareKey: "the Server assembles the authentication middleware from its own web.security section",
	ErrorBoundaryKey:            "the Server assembles the outermost error boundary from the collected ErrorMapper plugins",
}

// ReservedMiddlewareIdentityError reports a contributed middleware that claims
// an identity the Web transport assembles itself.
type ReservedMiddlewareIdentityError struct {
	Identity plugin.Identity
	Reason   string
}

func (e *ReservedMiddlewareIdentityError) Error() string {
	return fmt.Sprintf(
		"xbc: middleware identity %s is reserved by the Web transport and cannot be contributed by a plugin: %s",
		e.Identity, e.Reason,
	)
}

// ReservedMiddlewareKeys returns the sorted plugin keys the Web transport
// reserves for middleware its Server assembles itself. No Definition may claim
// one, and no plugin may contribute a Middleware under one; they exist so
// contributed middleware can order itself against a framework-owned stage.
func ReservedMiddlewareKeys() []plugin.Key {
	keys := make([]plugin.Key, 0, len(reservedMiddlewareKeys))
	for key := range reservedMiddlewareKeys {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// rejectReservedMiddlewareIdentities fails assembly when a contributed entry
// claims a reserved identity. Without this the framework entry and the
// contributed one would collide as a generic duplicate identity, which reports
// the symptom rather than the rule that was broken.
func rejectReservedMiddlewareIdentities(contributed []plugin.Entry[Middleware]) error {
	for _, entry := range contributed {
		if reason, reserved := reservedMiddlewareKeys[entry.Identity.Plugin]; reserved {
			return &ReservedMiddlewareIdentityError{Identity: entry.Identity, Reason: reason}
		}
	}
	return nil
}
