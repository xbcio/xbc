package web

import (
	"context"
	"fmt"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

// CredentialExtractor is the Web projection of authentication.CredentialSource.
//
// The protocol-neutral contract takes only a context and a scheme, so a plugin
// implementing it directly could never see a header or a cookie. This interface
// closes that gap without letting HTTP leak into the authentication package:
// the framework wraps the registered extractors in a per-request adapter, and a
// future gRPC transport will supply its own equivalent.
//
// An extractor decides only belonging and extractability: does this request
// carry a credential naming its scheme, and can a value be pulled out of it.
// Signature checks, expiry, and every other structural or semantic check
// belong to the Authenticator. Two plugins sharing one header (jwt and apikey
// both read Authorization) must therefore agree on the
// Absent/Malformed/Presented boundary:
//
//   - header missing, or present but not this scheme's prefix -> Absent
//   - this scheme's prefix but no value follows it            -> Malformed
//   - this scheme's prefix and a value follows it             -> Presented
type CredentialExtractor interface {
	Scheme() authentication.Scheme
	ExtractCredential(*Ctx) (authentication.CredentialResult, error)
}

// newExtractorIndex keys extractors by scheme. Two extractors claiming one
// scheme is unresolvable -- the framework would have to pick arbitrarily, which
// is exactly the nondeterminism this design exists to remove -- so it fails
// startup instead.
func newExtractorIndex(
	entries []plugin.Entry[CredentialExtractor],
) (map[authentication.Scheme]CredentialExtractor, error) {
	index := make(map[authentication.Scheme]CredentialExtractor, len(entries))
	owners := make(map[authentication.Scheme]plugin.Identity, len(entries))
	for _, entry := range entries {
		scheme := entry.Value.Scheme()
		if err := scheme.Validate(); err != nil {
			return nil, fmt.Errorf(
				"xbc: web credential extractor %s declares an invalid scheme: %w",
				entry.Identity, err,
			)
		}
		if owner, exists := owners[scheme]; exists {
			return nil, fmt.Errorf(
				"xbc: web credential extractors %s and %s both claim scheme %q",
				owner, entry.Identity, scheme,
			)
		}
		owners[scheme] = entry.Identity
		index[scheme] = entry.Value
	}
	return index, nil
}

// requestCredentialSource adapts the extractor index to one live request. It is
// created per request and never stored, so the Ctx it captures cannot outlive
// the handler. The Ctx is wrapped once at construction rather than per lookup:
// Credential runs once per scheme in the selection, and every extractor must
// see the same request surface.
type requestCredentialSource struct {
	ctx        *Ctx
	extractors map[authentication.Scheme]CredentialExtractor
}

var _ authentication.CredentialSource = requestCredentialSource{}

func (s requestCredentialSource) Credential(
	_ context.Context,
	scheme authentication.Scheme,
) (authentication.CredentialResult, error) {
	extractor, ok := s.extractors[scheme]
	if !ok {
		// No startup validation guarantees the manager's scheme set and the
		// extractor index agree; the two are consistent today only because
		// every authentication plugin exports its Authenticator and
		// CredentialExtractor from the same value. Reaching this branch means
		// some plugin exported only half of that pair, which is a
		// framework/plugin assembly defect rather than a rejected request.
		return authentication.CredentialResult{}, fmt.Errorf(
			"xbc: web has no credential extractor for scheme %q", scheme,
		)
	}
	return extractor.ExtractCredential(s.ctx)
}
