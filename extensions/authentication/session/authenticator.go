package session

import (
	"context"

	"github.com/xbcio/xbc/extensions/authentication"
)

// Scheme is this plugin's authentication scheme name. It is what a security
// policy names in its authenticate list, and what a transport adapter's
// credential extractor must report so the two halves pair up.
const Scheme authentication.Scheme = "session"

// reasonInvalidCredential is the single collapsed rejection reason for every
// Authenticate failure once a credential has reached it: a value of the wrong
// Go type, a string that fails validID's shape check, a store error, a
// session the store did not find, and an expired session are all
// indistinguishable to the caller. Splitting any of them out would let a
// caller probe whether a specific session ID exists or ever existed.
const reasonInvalidCredential authentication.SafeReason = "invalid credential"

var _ authentication.Authenticator = (*Plugin)(nil)

// Scheme identifies this plugin to the authentication manager.
func (*Plugin) Scheme() authentication.Scheme { return Scheme }

// Authenticate performs every semantic check a transport adapter's extractor
// deliberately does not: session-ID shape (validID), store lookup, idle-TTL
// renewal, and expiry. The credential value is the raw session ID string the
// adapter extracted. A rejection never carries a challenge: cookie sessions
// have no authentication-scheme token to advertise, and the client's recovery
// path for a missing or dead session is the login flow, not a credential
// retry.
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
	return authentication.Accepted(authentication.Principal{
		Subject:    value.Subject,
		AuthMethod: string(Scheme),
		Attributes: attributes,
	}), nil
}
