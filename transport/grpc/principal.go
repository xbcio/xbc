package grpc

import (
	"context"
	"maps"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
)

type principalContextKey struct{}

type exemptContextKey struct{}

// withPrincipal publishes a verified identity on a call's context. It refuses
// an empty subject rather than installing an identity downstream authorization
// could misread, and stores a private copy of Attributes.
func withPrincipal(ctx context.Context, principal authentication.Principal) (context.Context, bool) {
	principal.Subject = strings.TrimSpace(principal.Subject)
	if principal.Subject == "" {
		return ctx, false
	}
	principal.AuthMethod = strings.TrimSpace(principal.AuthMethod)
	principal.Attributes = maps.Clone(principal.Attributes)
	return context.WithValue(ctx, principalContextKey{}, principal), true
}

// PrincipalFromContext returns the verified identity the Server's
// authentication stage published for this call. The returned Attributes map is
// a defensive copy. Handlers and interceptors inside the authentication stage
// read it from the context gRPC hands them; a stream handler reads it from
// ServerStream.Context().
func PrincipalFromContext(ctx context.Context) (authentication.Principal, bool) {
	if ctx == nil {
		return authentication.Principal{}, false
	}
	principal, ok := ctx.Value(principalContextKey{}).(authentication.Principal)
	if !ok || principal.Subject == "" {
		return authentication.Principal{}, false
	}
	principal.Attributes = maps.Clone(principal.Attributes)
	return principal, true
}

// AuthenticationExempt reports whether the authentication stage resolved this
// call's method to permit. Authorization interceptors must ask this rather than
// re-derive the answer from a registration-time declaration: an application
// rule in grpc.security may have overridden it. A call that never passed the
// authentication stage reports false -- fail closed, not open.
func AuthenticationExempt(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	exempt, _ := ctx.Value(exemptContextKey{}).(bool)
	return exempt
}

func withAuthenticationExempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, exemptContextKey{}, true)
}
