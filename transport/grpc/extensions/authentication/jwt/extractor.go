package jwt

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"

	"github.com/xbcio/xbc/extensions/authentication"
	jwtcore "github.com/xbcio/xbc/extensions/authentication/jwt"
)

// Scheme pairs the extractor with the neutral authenticator in the
// authentication manager's index.
func (*Extractor) Scheme() authentication.Scheme { return jwtcore.Scheme }

// ExtractCredential decides only whether the configured metadata entry
// syntactically belongs to this scheme. Signature, expiry, and revocation are
// the authenticator's job -- splitting them this way is what lets jwt and
// another bearer-style scheme share one entry without either shadowing the
// other.
//
// A repeated entry is Malformed only when one of its values names this scheme:
// which token the caller meant would otherwise be an arbitrary choice. Values
// naming other schemes belong to whichever scheme they name.
func (e *Extractor) ExtractCredential(_ context.Context, md metadata.MD) (authentication.CredentialResult, error) {
	challenge := authentication.Challenge(e.scheme)
	var claimed [][]string
	for _, value := range md.Get(e.key) {
		fields := strings.Fields(value)
		if len(fields) > 0 && strings.EqualFold(fields[0], e.scheme) {
			claimed = append(claimed, fields)
		}
	}
	switch {
	case len(claimed) == 0:
		// The challenge describes what this method accepts, not what the
		// client sent, so it names this scheme regardless.
		return authentication.AbsentWithChallenge(challenge), nil
	case len(claimed) > 1, len(claimed[0]) != 2:
		return authentication.MalformedWithChallenge("malformed credential", challenge), nil
	}
	return authentication.Presented(claimed[0][1]), nil
}
