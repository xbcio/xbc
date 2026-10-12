package jwt

import (
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
	"github.com/xbcio/xbc/transport/web"
)

// Scheme pairs the extractor with the neutral authenticator in the
// authentication manager's index.
func (*Extractor) Scheme() authentication.Scheme { return jwtcore.Scheme }

// ExtractCredential decides only whether the configured header syntactically
// belongs to this scheme. Signature, expiry, and revocation are Authenticate's
// job -- splitting them this way is what lets jwt and another bearer-style
// scheme share one header without either shadowing the other.
func (e *Extractor) ExtractCredential(c *web.Ctx) (authentication.CredentialResult, error) {
	raw := c.GetHeader(e.header)
	if strings.TrimSpace(raw) == "" {
		return authentication.AbsentWithChallenge(authentication.Challenge(e.scheme)), nil
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 || !strings.EqualFold(fields[0], e.scheme) {
		// Some other scheme owns this header value. WWW-Authenticate describes
		// what this route accepts, not what the client sent, so the challenge
		// still names this scheme.
		return authentication.AbsentWithChallenge(authentication.Challenge(e.scheme)), nil
	}
	if len(fields) != 2 || fields[1] == "" {
		return authentication.MalformedWithChallenge(
			"malformed credential",
			authentication.Challenge(e.scheme),
		), nil
	}
	return authentication.Presented(fields[1]), nil
}
