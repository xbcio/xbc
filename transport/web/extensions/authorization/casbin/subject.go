package casbin

import (
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

// SubjectResolver obtains a policy subject from facts already verified by an
// upstream authentication middleware. Returning false denies the request.
// Implementations must not treat an unverified header or token as an identity.
type SubjectResolver interface {
	ResolveSubject(*web.Ctx) (subject string, ok bool)
}

// SubjectResolverFunc adapts a function to SubjectResolver.
type SubjectResolverFunc func(*web.Ctx) (subject string, ok bool)

// ResolveSubject implements SubjectResolver.
func (f SubjectResolverFunc) ResolveSubject(c *web.Ctx) (string, bool) {
	if f == nil {
		return "", false
	}
	return f(c)
}

type principalSubjectResolver struct{}

func (principalSubjectResolver) ResolveSubject(c *web.Ctx) (string, bool) {
	principal, ok := web.CurrentPrincipal(c)
	if !ok {
		return "", false
	}
	subject := strings.TrimSpace(principal.Subject)
	return subject, subject != ""
}
