package grpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc/metadata"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
)

// CredentialExtractor is the gRPC projection of authentication.CredentialSource.
//
// The protocol-neutral contract takes only a context and a scheme, so a plugin
// implementing it directly could never see request metadata. This interface
// closes that gap without letting gRPC leak into the authentication package:
// the Server wraps the registered extractors in a per-call adapter over the
// incoming metadata. Metadata keys are lower-case, as gRPC delivers them.
//
// An extractor decides only belonging and extractability: does this call carry
// a credential naming its scheme, and can a value be pulled out of it. Every
// structural or semantic check belongs to the Authenticator. Two extractors
// sharing one key (jwt and apikey both read authorization) must therefore agree
// on the Absent/Malformed/Presented boundary:
//
//   - key missing, or present but not this scheme's prefix -> Absent
//   - this scheme's prefix but no value follows it         -> Malformed
//   - this scheme's prefix and a value follows it          -> Presented
type CredentialExtractor interface {
	Scheme() authentication.Scheme
	ExtractCredential(context.Context, metadata.MD) (authentication.CredentialResult, error)
}

// newExtractorIndex keys extractors by scheme. Two extractors claiming one
// scheme is unresolvable without an arbitrary choice, so it fails assembly.
func newExtractorIndex(entries []plugin.Entry[CredentialExtractor]) (map[authentication.Scheme]CredentialExtractor, error) {
	index := make(map[authentication.Scheme]CredentialExtractor, len(entries))
	owners := make(map[authentication.Scheme]plugin.Identity, len(entries))
	for _, entry := range entries {
		scheme := entry.Value.Scheme()
		if err := scheme.Validate(); err != nil {
			return nil, fmt.Errorf("xbc: gRPC credential extractor %s declares an invalid scheme: %w", entry.Identity, err)
		}
		if owner, exists := owners[scheme]; exists {
			return nil, fmt.Errorf("xbc: gRPC credential extractors %s and %s both claim scheme %q", owner, entry.Identity, scheme)
		}
		owners[scheme] = entry.Identity
		index[scheme] = entry.Value
	}
	return index, nil
}

// metadataCredentialSource adapts the extractor index to one live call. It is
// created per call and never stored.
type metadataCredentialSource struct {
	md         metadata.MD
	extractors map[authentication.Scheme]CredentialExtractor
}

var _ authentication.CredentialSource = metadataCredentialSource{}

func (s metadataCredentialSource) Credential(ctx context.Context, scheme authentication.Scheme) (authentication.CredentialResult, error) {
	extractor, ok := s.extractors[scheme]
	if !ok {
		// Assembly rejects a registered authenticator without an extractor,
		// so reaching this is a framework defect rather than a rejected call.
		return authentication.CredentialResult{}, fmt.Errorf("xbc: gRPC has no credential extractor for scheme %q", scheme)
	}
	return extractor.ExtractCredential(ctx, s.md)
}
