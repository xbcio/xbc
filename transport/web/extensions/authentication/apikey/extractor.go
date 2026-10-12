package apikey

import (
	"strings"

	"github.com/xbcio/xbc/extensions/authentication"
	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
	"github.com/xbcio/xbc/transport/web"
)

// Scheme pairs the extractor with the neutral authenticator it feeds.
func (*Extractor) Scheme() authentication.Scheme { return apikeycore.Scheme }

// headerState classifies why singleHeader could not return one clean value:
// missing entirely, one usable value, duplicated across multiple header
// lines, or present but blank or padded with whitespace. ExtractCredential
// needs this distinction to report the API-key header's two independent
// Malformed reasons.
type headerState uint8

const (
	headerAbsent headerState = iota
	headerOK
	headerDuplicate
	headerBlank
)

// singleHeader classifies exactly one header's values. It never returns a
// usable value for any state but headerOK, so a caller cannot accidentally
// treat a duplicated or blank-padded header as a clean credential.
func singleHeader(c *web.Ctx, name string) (value string, present bool, state headerState) {
	if c == nil || c.Request() == nil {
		return "", false, headerAbsent
	}
	values := c.Request().Header.Values(name)
	if len(values) == 0 {
		return "", false, headerAbsent
	}
	if len(values) != 1 {
		return "", true, headerDuplicate
	}
	raw := values[0]
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed != raw {
		return "", true, headerBlank
	}
	return trimmed, true, headerOK
}

// ExtractCredential decides only whether a request carries syntactically
// extractable API-key evidence: which header holds it, whether that header is
// unusable (duplicated or blank), and whether the key header and Authorization
// header disagree about which one is authoritative. Minimum key length,
// app-ID requirement, and repository lookup are the authenticator's job.
//
// Extraction runs in two phases. Phase A decides whether this request names
// apikey's scheme at all; a request that does not must return Absent, never a
// Malformed reason, because Malformed is itself observable to an
// unauthenticated caller and must not leak whether apikey is even in a
// route's scheme selection (for example, a lone duplicated X-App-ID with no
// key header and no Authorization value names nothing of apikey's). Phase B,
// reached only once Phase A says yes, applies the extractability checks that
// produce this plugin's Malformed reasons.
func (e *Extractor) ExtractCredential(c *web.Ctx) (authentication.CredentialResult, error) {
	challenge := authentication.Challenge(e.bearerScheme)

	headerKey, keyPresent, keyState := singleHeader(c, e.header)
	authorization, authorizationPresent, authState := singleHeader(c, "Authorization")
	appIDValue, _, appIDState := singleHeader(c, e.appIDHeader)

	// Phase A: does this request name apikey's scheme? The key header does so
	// in any form, valid or not: a duplicated X-API-Key still unambiguously
	// names apikey. Authorization only names apikey's scheme when AllowBearer
	// is enabled; when it is, an unreadable Authorization value (duplicated or
	// blank) still counts, because apikey consumes that header in this
	// configuration and cannot tell what it would have said. A readable value
	// naming some other scheme does not count: that credential belongs to
	// whichever scheme it names, not this one.
	var bearerFields []string
	bearerNamesScheme := false
	if e.allowBearer && authorizationPresent {
		if authState == headerOK {
			bearerFields = strings.Fields(authorization)
			bearerNamesScheme = len(bearerFields) > 0 && strings.EqualFold(bearerFields[0], e.bearerScheme)
		} else {
			bearerNamesScheme = true
		}
	}
	if !keyPresent && !bearerNamesScheme {
		return authentication.AbsentWithChallenge(challenge), nil
	}

	// Phase B: apikey's scheme has been named. Classify why a value cannot be
	// cleanly extracted.
	switch keyState {
	case headerDuplicate:
		return authentication.MalformedWithChallenge("duplicate api key header", challenge), nil
	case headerBlank:
		return authentication.MalformedWithChallenge("empty api key header", challenge), nil
	}
	// Authorization's own shape is only apikey's business when AllowBearer
	// makes Authorization one of apikey's credential sources; otherwise this
	// plugin does not read that header in this configuration and its shape is
	// none of apikey's business.
	if e.allowBearer && (authState == headerDuplicate || authState == headerBlank) {
		return authentication.MalformedWithChallenge("malformed credential", challenge), nil
	}
	if appIDState == headerDuplicate || appIDState == headerBlank {
		return authentication.MalformedWithChallenge("malformed app id header", challenge), nil
	}
	// The key header and Authorization header must not both be present: which
	// one wins would otherwise be undefined behavior. Authorization is only a
	// competing source when AllowBearer makes it one.
	if e.allowBearer && keyPresent && authorizationPresent {
		return authentication.MalformedWithChallenge("conflicting api key headers", challenge), nil
	}

	if keyPresent {
		return authentication.Presented(apikeycore.PresentedKey{Secret: headerKey, AppID: appIDValue}), nil
	}
	if len(bearerFields) != 2 || bearerFields[1] == "" {
		return authentication.MalformedWithChallenge("malformed credential", challenge), nil
	}
	return authentication.Presented(apikeycore.PresentedKey{Secret: bearerFields[1], AppID: appIDValue}), nil
}
