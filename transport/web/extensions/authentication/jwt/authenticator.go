package jwt

import (
	"context"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/transport/web"
)

// Scheme is this plugin's authentication scheme name. It is what a security
// policy names in its authenticate list.
const Scheme authentication.Scheme = "jwt"

var (
	_ authentication.Authenticator = (*Plugin)(nil)
	_ web.CredentialExtractor      = (*Plugin)(nil)
)

// Scheme identifies this plugin to both the credential extractor index and the
// authentication manager.
func (*Plugin) Scheme() authentication.Scheme { return Scheme }

// ExtractCredential decides only whether the configured header syntactically
// belongs to this scheme. Signature, expiry, and revocation are Authenticate's
// job -- splitting them this way is what lets jwt and another bearer-style
// scheme share one header without either shadowing the other.
func (p *Plugin) ExtractCredential(c *web.Ctx) (authentication.CredentialResult, error) {
	runtime := p.compiled
	raw := c.GetHeader(runtime.header)
	if strings.TrimSpace(raw) == "" {
		return authentication.AbsentWithChallenge(authentication.Challenge(runtime.scheme)), nil
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 || !strings.EqualFold(fields[0], runtime.scheme) {
		// Some other scheme owns this header value. WWW-Authenticate describes
		// what this route accepts, not what the client sent, so the challenge
		// still names this scheme.
		return authentication.AbsentWithChallenge(authentication.Challenge(runtime.scheme)), nil
	}
	if len(fields) != 2 || fields[1] == "" {
		return authentication.MalformedWithChallenge(
			"malformed credential",
			authentication.Challenge(runtime.scheme),
		), nil
	}
	return authentication.Presented(fields[1]), nil
}

// Authenticate performs every semantic check: parsing, signature, expiry, and
// subject extraction. Deliberately does not return, log, or serialize the
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
		// A principal must carry a non-empty subject (web.SetPrincipal enforces
		// this), so a token that verifies but names no subject cannot produce
		// one and is rejected the same as any other invalid token.
		return authentication.RejectedWithChallenge(
			"invalid token",
			authentication.Challenge(runtime.scheme),
		), nil
	}
	return authentication.Accepted(web.Principal{
		Subject:    subject,
		AuthMethod: string(Scheme),
		Attributes: map[string]any(claims),
	}), nil
}
