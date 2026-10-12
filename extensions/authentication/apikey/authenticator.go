package apikey

import (
	"context"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
)

// Scheme is this plugin's authentication scheme name. It is what a security
// policy names in its authenticate list, and what a transport adapter's
// credential extractor must report so the two halves pair up.
const Scheme authentication.Scheme = "apikey"

var _ authentication.Authenticator = (*Plugin)(nil)

// Scheme identifies this plugin to the authentication manager.
func (*Plugin) Scheme() authentication.Scheme { return Scheme }

// PresentedKey is the credential value a transport adapter hands to
// Authenticate: the presented secret and the caller-supplied app ID, which is
// empty when the request carries none. A bare string cannot hold both, and
// routing the app ID through a side channel would let it go missing between
// extraction and verification, so both travel together in one value. The
// adapter only extracts; every semantic check is Authenticate's.
type PresentedKey struct {
	Secret string
	AppID  string
}

// Authenticate performs every semantic check a transport adapter's extractor
// deliberately does not: minimum key length, the app-ID requirement, and
// repository lookup. A repository error, an unknown key, and a wrong key are
// intentionally indistinguishable to the caller so storage failures cannot
// become a credential-enumeration oracle.
func (p *Plugin) Authenticate(
	ctx context.Context,
	credential authentication.Credential,
) (authentication.Result, error) {
	state := p.state
	challenge := authentication.Challenge(state.config.bearerScheme)

	value, ok := credential.Value().(PresentedKey)
	if !ok {
		return authentication.RejectedWithChallenge("invalid credential type", challenge), nil
	}
	if len([]byte(value.Secret)) < state.config.minKeyBytes {
		return authentication.RejectedWithChallenge("invalid credential", challenge), nil
	}
	if state.config.requireAppID && value.AppID == "" {
		return authentication.RejectedWithChallenge("invalid credential", challenge), nil
	}

	digest := HashKey(value.Secret)
	stored, found, err := state.repo.Lookup(ctx, value.AppID, digest)
	// Repository diagnostics, an unknown key, and a wrong key are
	// intentionally collapsed into one rejection so storage failures cannot
	// become a credential-enumeration oracle.
	if err != nil || !found || strings.TrimSpace(stored.Subject) == "" {
		return authentication.RejectedWithChallenge("invalid credential", challenge), nil
	}

	attributes := cloneAttributes(stored.Attributes)
	if attributes == nil {
		attributes = make(map[string]any, 2)
	}
	if stored.ID != "" {
		attributes["credential_id"] = stored.ID
	}
	if stored.AppID != "" {
		attributes["app_id"] = stored.AppID
	}
	return authentication.Accepted(authentication.Principal{
		Subject:    stored.Subject,
		AuthMethod: string(Scheme),
		Attributes: attributes,
	}), nil
}
