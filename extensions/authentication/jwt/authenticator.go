package jwt

import (
	"context"

	"github.com/xbcio/xbc/extensions/authentication"
)

// Scheme is this plugin's authentication scheme name. It is what a security
// policy names in its authenticate list, and what a transport adapter's
// credential extractor must report so the two halves pair up.
const Scheme authentication.Scheme = "jwt"

var _ authentication.Authenticator = (*Plugin)(nil)

// Scheme identifies this plugin to the authentication manager.
func (*Plugin) Scheme() authentication.Scheme { return Scheme }

// Authenticate performs every semantic check: parsing, signature, expiry, and
// subject extraction. The credential value is the raw token string a transport
// adapter extracted. Deliberately does not return, log, or serialize the
// underlying parser error -- signature, expiry, issuer, and parser details are
// all authentication oracles.
func (p *Plugin) Authenticate(
	_ context.Context,
	credential authentication.Credential,
) (authentication.Result, error) {
	runtime := p.compiled
	token, ok := credential.Value().(string)
	if !ok {
		return authentication.RejectedWithChallenge(
			"invalid credential type",
			authentication.Challenge(runtime.scheme),
		), nil
	}
	claims, err := runtime.verify(token)
	if err != nil {
		return authentication.RejectedWithChallenge(
			"invalid token",
			authentication.Challenge(runtime.scheme),
		), nil
	}
	subject, err := claims.GetSubject()
	if err != nil || subject == "" {
		// A principal must carry a non-empty subject (transports refuse to
		// publish one without it), so a token that verifies but names no
		// subject cannot produce one and is rejected the same as any other
		// invalid token.
		return authentication.RejectedWithChallenge(
			"invalid token",
			authentication.Challenge(runtime.scheme),
		), nil
	}
	return authentication.Accepted(authentication.Principal{
		Subject:    subject,
		AuthMethod: string(Scheme),
		Attributes: map[string]any(claims),
	}), nil
}
