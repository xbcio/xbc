package session

import (
	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web"
)

// Scheme pairs the adapter with the neutral session authenticator it feeds.
func (*Plugin) Scheme() authentication.Scheme { return sessioncore.Scheme }

// ExtractCredential decides only whether the configured cookie syntactically
// belongs to this scheme and whether a value can be read out of it. Whether
// that value names a real, live, unrevoked session is the authenticator's job.
//
// A cookie with the configured name that appears more than once is
// Malformed, not Absent and not a silent first-wins: the extractor genuinely
// cannot pick a value in that case, the same way a duplicated header is
// Malformed for apikey.
//
// Unlike jwt and apikey, every result this method returns carries no
// challenge. Cookie sessions have no HTTP authentication-scheme token to
// advertise: a WWW-Authenticate: session header would name a scheme no
// client software can act on, and RFC 7235 expects a challenge to name a
// scheme the server actually implements. The client's real recovery path for
// a missing or dead session cookie is the login flow, not a credential
// retry, so there is nothing to challenge toward. This is a deliberate,
// tested exception to the challenge-on-every-result pattern jwt and apikey
// follow -- see TestPluginExtractCredentialClassifiesCookie's Challenge
// assertions.
func (p *Plugin) ExtractCredential(c *web.Ctx) (authentication.CredentialResult, error) {
	if c == nil || c.Request() == nil {
		return authentication.Absent(), nil
	}
	cookies := c.Request().CookiesNamed(p.cookie.Name)
	if len(cookies) == 0 {
		return authentication.Absent(), nil
	}
	if len(cookies) != 1 || cookies[0] == nil || cookies[0].Value == "" {
		return authentication.Malformed("malformed credential"), nil
	}
	return authentication.Presented(cookies[0].Value), nil
}
