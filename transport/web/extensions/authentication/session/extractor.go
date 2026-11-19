package session

import (
	"github.com/xbcio/xbc/extensions/authentication"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/transport/web"
)

// Scheme pairs the adapter with the neutral session authenticator it feeds.
func (*Plugin) Scheme() authentication.Scheme { return sessioncore.Scheme }

// ExtractCredential decides only whether the configured carrier chain
// syntactically carries a credential for this scheme and whether a value can
// be read out of it. Whether that value names a real, live, unrevoked session
// is the authenticator's job.
//
// The chain is evaluated whole, never stopping at the first Presented value:
// the first presented value wins, but any Malformed result fails the entire
// extraction, so a duplicated or unusable credential is never masked by a
// later usable one.
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
	if p == nil || p.carriers == nil {
		return authentication.Absent(), nil
	}
	return p.carriers.Extract(c), nil
}
