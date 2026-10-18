package oauth2

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/xbcio/xbc/extensions/authentication/federation"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// AuthorizationRequest is the one-time transaction material for a single
// authorization-code login. Begin returns it; the web layer stores state and
// the PKCE verifier -- bound to the initiating browser and to a short TTL --
// and passes it back to Exchange when the callback arrives.
//
// URL is the authorization endpoint with every parameter applied. State and
// CodeVerifier are secrets until the transaction is consumed: State travels
// through the browser, CodeVerifier never leaves the server except as its
// derived challenge.
type AuthorizationRequest struct {
	// URL is the absolute authorization endpoint URL to redirect the browser
	// to.
	URL string
	// State is the opaque anti-forgery value bound to this transaction.
	State string
	// CodeVerifier is the PKCE verifier whose S256 challenge was sent in URL.
	CodeVerifier string
}

// Provider is one immutable OAuth 2.0 provider configuration together with
// the HTTP clients derived from it. Every method is safe for concurrent use.
//
// A Provider performs no network I/O at construction and no discovery at all:
// every endpoint is configured explicitly. Begin builds the authorization URL
// locally; Exchange redeems the code and makes exactly one userinfo call.
//
// String, Format, and LogValue render the provider with the client secret
// redacted, including for %#v and structured log records.
type Provider struct {
	settings
	client         *http.Client
	userInfoClient *http.Client
	initialized    bool
}

// New validates config without network I/O and returns a provider ready for
// Begin. The provider is immutable; replace it through the registry to apply a
// configuration change.
func New(config Config) (*Provider, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	client, err := outbound.NewClient(normalized.policy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	userInfoPolicy := normalized.policy
	userInfoPolicy.MaxRedirects = outbound.NoRedirects
	userInfoClient, err := outbound.NewClient(userInfoPolicy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return &Provider{
		settings:       normalized,
		client:         client,
		userInfoClient: userInfoClient,
		initialized:    true,
	}, nil
}

// Name returns the registered provider name, or the empty string for a nil
// provider.
func (p *Provider) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// String renders the provider for diagnostics with the client secret redacted.
// The provider embeds the immutable settings, so the guarantee Config carries
// must hold for the value an application actually holds.
func (p *Provider) String() string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("oauth2.Provider{Name:%q Issuer:%q AuthorizationEndpoint:%q TokenEndpoint:%q UserInfoEndpoint:%q ClientID:%q ClientSecret:[REDACTED] RedirectURI:%q Scopes:%v TokenEndpointAuthMethod:%q SubjectField:%q}",
		p.name, p.issuer, p.authorizationEndpoint, p.tokenEndpoint, p.userInfoEndpoint,
		p.clientID, p.redirectURI, p.scopes, p.authMethod, p.subjectField)
}

// Format implements fmt.Formatter so every verb renders the redacted string,
// including %#v, which would otherwise print the unexported settings fields --
// and with them the client secret.
func (p *Provider) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, p.String())
}

// LogValue implements slog.LogValuer with the client secret redacted, so a
// provider attached to a log record never carries the credential.
func (p *Provider) LogValue() slog.Value { return slog.StringValue(p.String()) }

// Begin creates a new authorization-code transaction: a fresh state and PKCE
// verifier, and the authorization URL that carries their derived values. The
// authorization endpoint is configured, so no discovery call is needed.
func (p *Provider) Begin() (AuthorizationRequest, error) {
	if err := p.usable(); err != nil {
		return AuthorizationRequest{}, err
	}

	state, err := randomToken(transactionEntropyBytes)
	if err != nil {
		return AuthorizationRequest{}, err
	}
	verifier, err := randomToken(transactionEntropyBytes)
	if err != nil {
		return AuthorizationRequest{}, err
	}

	endpoint, err := url.Parse(p.authorizationEndpoint)
	if err != nil {
		return AuthorizationRequest{}, fmt.Errorf("%w: authorization endpoint is unusable: %v", ErrInvalidConfig, err)
	}
	query := endpoint.Query()
	query.Set("response_type", "code")
	query.Set("client_id", p.clientID)
	query.Set("redirect_uri", p.redirectURI)
	if p.scope != "" {
		query.Set("scope", p.scope)
	}
	query.Set("state", state)
	query.Set("code_challenge", codeChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	endpoint.RawQuery = query.Encode()

	return AuthorizationRequest{
		URL:          endpoint.String(),
		State:        state,
		CodeVerifier: verifier,
	}, nil
}

// Exchange redeems an authorization code, fetches the account document once,
// and returns the verified external identity. The transaction must be the one
// Begin created: its PKCE verifier must match the challenge the IdP saw. A
// transaction without a well-formed verifier is rejected before any network
// call.
//
// Exchange performs no state comparison: matching state to the stored
// transaction, and consuming both exactly once, is the web layer's job.
func (p *Provider) Exchange(ctx context.Context, code string, request AuthorizationRequest) (federation.ExternalIdentity, error) {
	if err := p.usable(); err != nil {
		return federation.ExternalIdentity{}, err
	}
	if ctx == nil {
		return federation.ExternalIdentity{}, ErrNilContext
	}
	if !validCodeVerifier(request.CodeVerifier) {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: PKCE code verifier is missing or malformed", ErrInvalidTransaction)
	}
	if code == "" {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: authorization code is empty", ErrInvalidTransaction)
	}
	if len(code) > maxAuthorizationCodeLength {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: authorization code exceeds %d bytes", ErrInvalidTransaction, maxAuthorizationCodeLength)
	}

	accessToken, err := p.exchangeCode(ctx, code, request.CodeVerifier)
	if err != nil {
		return federation.ExternalIdentity{}, err
	}
	document, err := p.fetchUserInfo(ctx, accessToken)
	if err != nil {
		return federation.ExternalIdentity{}, err
	}
	return p.identity(document)
}

// identity maps the userinfo document onto the federation identity. The
// subject is read from the configured stable-ID path and must be a non-empty
// string; there is no fallback to an email address, a login name, or any other
// attribute. Every configured claim path must be present.
func (p *Provider) identity(document map[string]any) (federation.ExternalIdentity, error) {
	raw, ok := fieldAt(document, p.subjectPath)
	if !ok {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: response carries no subject field %q", ErrInvalidUserInfo, p.subjectField)
	}
	subject, ok := raw.(string)
	if !ok || strings.TrimSpace(subject) == "" {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: subject field %q is not a non-empty string", ErrInvalidUserInfo, p.subjectField)
	}

	var claims map[string]any
	if len(p.claimPaths) > 0 {
		claims = make(map[string]any, len(p.claimPaths))
		for name, path := range p.claimPaths {
			value, ok := fieldAt(document, path)
			if !ok {
				return federation.ExternalIdentity{}, fmt.Errorf("%w: response carries no claim field %q", ErrInvalidUserInfo, strings.Join(path, "."))
			}
			claims[name] = value
		}
	}

	return federation.ExternalIdentity{
		Protocol: protocolName,
		Provider: p.name,
		Issuer:   p.issuer,
		Subject:  subject,
		Claims:   claims,
		AuthTime: p.now().UTC(),
	}, nil
}

// usable reports whether the provider was constructed by New. The zero
// Provider has no clients and no settings; reaching the network with it would
// panic instead of returning an error.
func (p *Provider) usable() error {
	if p == nil {
		return ErrNilProvider
	}
	if !p.initialized || p.client == nil || p.userInfoClient == nil || p.now == nil {
		return fmt.Errorf("%w: provider was not constructed by New", ErrInvalidConfig)
	}
	return nil
}
