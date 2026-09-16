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
// Authentication enforcement is a framework guarantee: web.security defaults to
// deny, and Start fails when a route needs authentication that no registered
// authenticator can satisfy. A guarantee cannot be an opt-in plugin -- if the
// middleware were selected at the composition root, omitting its Bundle would
// silently serve every route unauthenticated, which is exactly the failure the
// deny default exists to prevent. It also enforces this Definition's own
// web.security section, and config.NewUniverse gives each section exactly one
// owning plugin, so a second Definition cannot claim it without first moving
// the section out of web.Config.
//
// What the anchor is for is ordering: a contributed middleware declares
// After: Require(AuthenticationMiddlewareKey) so authorization always observes a
// published Principal. Because that anchor lives in the same flat namespace as
// plugin keys, the reservation has to be explicit and enforced -- otherwise a
// plugin could claim the key and the only symptom would be a duplicate-identity
// failure at Start.
var reservedMiddlewareKeys = map[plugin.Key]string{
	AuthenticationMiddlewareKey: "the Server assembles the authentication middleware from its own web.security section",
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
