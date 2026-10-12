package apikey

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/xbcio/xbc/extensions/authentication"
	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
)

// Scheme pairs the extractor with the neutral authenticator it feeds.
func (*Extractor) Scheme() authentication.Scheme { return apikeycore.Scheme }

// entryState classifies why singleEntry could not return one clean value:
// missing entirely, one usable value, repeated, or present but blank or padded
// with whitespace. ExtractCredential needs this distinction to report the
// API-key entry's two independent Malformed reasons.
type entryState uint8

const (
	entryAbsent entryState = iota
	entryOK
	entryDuplicate
	entryBlank
)

// singleEntry classifies exactly one metadata key's values. It never returns a
// usable value for any state but entryOK, so a caller cannot accidentally treat
// a repeated or blank-padded entry as a clean credential.
func singleEntry(md metadata.MD, key string) (value string, present bool, state entryState) {
	values := md.Get(key)
	if len(values) == 0 {
		return "", false, entryAbsent
	}
	if len(values) != 1 {
		return "", true, entryDuplicate
	}
	raw := values[0]
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed != raw {
		return "", true, entryBlank
	}
	return trimmed, true, entryOK
}

// ExtractCredential decides only whether a call carries syntactically
// extractable API-key evidence. It applies the same two-phase rule as the Web
// extractor, over metadata instead of headers: Phase A decides whether the
// call names apikey's scheme at all, and a call that does not is Absent, never
// Malformed, so an unauthenticated caller cannot probe whether apikey is in a
// method's scheme selection; Phase B, reached only once Phase A says yes,
// applies the extractability checks. Minimum key length, the app-ID
// requirement, and repository lookup are the authenticator's job.
func (e *Extractor) ExtractCredential(_ context.Context, md metadata.MD) (authentication.CredentialResult, error) {
	challenge := authentication.Challenge(e.bearerScheme)

	entryKey, keyPresent, keyState := singleEntry(md, e.key)
	authorization, authorizationPresent, authState := singleEntry(md, "authorization")
	appIDValue, _, appIDState := singleEntry(md, e.appIDKey)

	// Phase A. The key entry names apikey in any form, valid or not.
	// authorization names it only when AllowBearer makes it one of apikey's
	// sources; then an unreadable value still counts, because apikey consumes
	// the entry in this configuration and cannot tell what it would have said.
	var bearerFields []string
	bearerNamesScheme := false
	if e.allowBearer && authorizationPresent {
		if authState == entryOK {
			bearerFields = strings.Fields(authorization)
			bearerNamesScheme = len(bearerFields) > 0 && strings.EqualFold(bearerFields[0], e.bearerScheme)
		} else {
			bearerNamesScheme = true
		}
	}
	if !keyPresent && !bearerNamesScheme {
		return authentication.AbsentWithChallenge(challenge), nil
	}

	// Phase B: apikey's scheme has been named.
	switch keyState {
	case entryDuplicate:
		return authentication.MalformedWithChallenge("duplicate api key metadata", challenge), nil
	case entryBlank:
		return authentication.MalformedWithChallenge("empty api key metadata", challenge), nil
	}
	if e.allowBearer && (authState == entryDuplicate || authState == entryBlank) {
		return authentication.MalformedWithChallenge("malformed credential", challenge), nil
	}
	if appIDState == entryDuplicate || appIDState == entryBlank {
		return authentication.MalformedWithChallenge("malformed app id metadata", challenge), nil
	}
	// Which of two competing sources wins would otherwise be undefined.
	if e.allowBearer && keyPresent && authorizationPresent {
		return authentication.MalformedWithChallenge("conflicting api key metadata", challenge), nil
	}

	if keyPresent {
		return authentication.Presented(apikeycore.PresentedKey{Secret: entryKey, AppID: appIDValue}), nil
	}
	if len(bearerFields) != 2 || bearerFields[1] == "" {
		return authentication.MalformedWithChallenge("malformed credential", challenge), nil
	}
	return authentication.Presented(apikeycore.PresentedKey{Secret: bearerFields[1], AppID: appIDValue}), nil
}
