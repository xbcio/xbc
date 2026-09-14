package session

import (
	"context"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/transport/web"
)

// Scheme is this plugin's authentication scheme name. It is what a security
// policy names in its authenticate list.
const Scheme authentication.Scheme = "session"

// reasonInvalidCredential is the single collapsed rejection reason for every
// Authenticate failure once a credential has reached it: a value of the wrong
// Go type, a string that fails validID's shape check, a store error, a
// session the store did not find, and an expired session are all
// indistinguishable to the caller. Splitting any of them out would let a
// caller probe whether a specific session ID exists or ever existed.
const reasonInvalidCredential authentication.SafeReason = "invalid credential"

var (
	_ authentication.Authenticator = (*Plugin)(nil)
	_ web.CredentialExtractor      = (*Plugin)(nil)
)

// Scheme identifies this plugin to both the credential extractor index and the
// authentication manager.
func (*Plugin) Scheme() authentication.Scheme { return Scheme }

// ExtractCredential decides only whether the configured cookie syntactically
// belongs to this scheme and whether a value can be read out of it. Whether
// that value names a real, live, unrevoked session is Authenticate's job.
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
	cookies := c.Request().CookiesNamed(p.config.name)
	if len(cookies) == 0 {
		return authentication.Absent(), nil
	}
	if len(cookies) != 1 || cookies[0] == nil || cookies[0].Value == "" {
		return authentication.Malformed("malformed credential"), nil
	}
	return authentication.Presented(cookies[0].Value), nil
}

// Authenticate performs every semantic check ExtractCredential deliberately
// does not: session-ID shape (validID), store lookup, idle-TTL renewal, and
// expiry. Like ExtractCredential's results, a rejection here never carries a
// challenge, for the same reason: there is no session scheme token to
// advertise.
func (p *Plugin) Authenticate(
	ctx context.Context,
	credential authentication.Credential,
) (authentication.Result, error) {
	id, ok := credential.Value().(string)
	if !ok {
		return authentication.Rejected(reasonInvalidCredential), nil
	}
	if p.manager.closed.Load() || !validID(id, p.config.idBytes) {
		return authentication.Rejected(reasonInvalidCredential), nil
	}
	touchCtx, cancel := context.WithTimeout(ctx, p.config.operationTimeout)
	value, found, err := p.manager.store.Touch(touchCtx, id, p.config.idleTTL, p.config.touchInterval)
	cancel()
	if err != nil || !found {
		return authentication.Rejected(reasonInvalidCredential), nil
	}

	attributes := cloneMap(value.Attributes)
	if attributes == nil {
		attributes = make(map[string]any, 1)
	}
	attributes["session_id"] = value.ID
	return authentication.Accepted(web.Principal{
		Subject:    value.Subject,
		AuthMethod: string(Scheme),
		Attributes: attributes,
	}), nil
}
