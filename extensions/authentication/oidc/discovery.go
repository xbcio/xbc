package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// providerMetadata is the validated subset of an OIDC discovery document the
// authorization-code flow needs. It is immutable after publication.
type providerMetadata struct {
	issuer                string
	authorizationEndpoint string
	tokenEndpoint         string
	jwksURI               string
}

// discoveryDocument is the wire shape of the discovery response. Unknown
// members are ignored: a discovery document carries many fields this module
// does not consume.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// fetchMetadata fetches and validates one discovery document. The request
// travels through the outbound policy client, and the document's issuer must
// equal the registered issuer exactly; a document served under the issuer's
// well-known path that names a different issuer is a misdirected or spoofed
// response and is rejected.
func (p *Provider) fetchMetadata(ctx context.Context, endpoint string) (*providerMetadata, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDiscovery, err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDiscovery, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: discovery endpoint returned status %d", ErrDiscovery, response.StatusCode)
	}
	body, err := outbound.ReadBody(response, p.policy.MaxBodyBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDiscovery, err)
	}
	var document discoveryDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("%w: discovery document is not a JSON object", ErrDiscovery)
	}
	if document.Issuer != p.issuer {
		return nil, fmt.Errorf("%w: discovery document issuer %q does not match the registered issuer %q",
			ErrDiscovery, document.Issuer, p.issuer)
	}

	authorizationEndpoint, err := p.discoveredEndpoint("authorization_endpoint", document.AuthorizationEndpoint)
	if err != nil {
		return nil, err
	}
	tokenEndpoint, err := p.discoveredEndpoint("token_endpoint", document.TokenEndpoint)
	if err != nil {
		return nil, err
	}
	jwksURI, err := p.discoveredEndpoint("jwks_uri", document.JWKSURI)
	if err != nil {
		return nil, err
	}

	return &providerMetadata{
		issuer:                document.Issuer,
		authorizationEndpoint: authorizationEndpoint,
		tokenEndpoint:         tokenEndpoint,
		jwksURI:               jwksURI,
	}, nil
}

// discoveredEndpoint validates one endpoint named by the discovery document
// against the same URL shape the issuer had to satisfy.
func (p *Provider) discoveredEndpoint(name, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%w: discovery document carries no %s", ErrDiscovery, name)
	}
	if _, err := parseEndpoint(value, p.policy.AllowHTTP); err != nil {
		return "", fmt.Errorf("%w: %s %q is unusable: %v", ErrDiscovery, name, value, err)
	}
	return value, nil
}
