package oidc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/xbcio/xbc/extensions/authentication/federation"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// AuthorizationRequest is the one-time transaction material for a single
// authorization-code login. Begin returns it; the web layer stores it --
// state, nonce, and the PKCE verifier, bound to the initiating browser and to
// a short TTL -- and passes it back to Exchange when the callback arrives.
//
// URL is the authorization endpoint with every parameter applied. State,
// Nonce, and CodeVerifier are secrets until the transaction is consumed:
// State travels through the browser, Nonce and CodeVerifier never leave the
// server except as their derived values.
type AuthorizationRequest struct {
	// URL is the absolute authorization endpoint URL to redirect the browser
	// to.
	URL string
	// State is the opaque anti-forgery value bound to this transaction.
	State string
	// Nonce is the replay-binding value that the ID token must echo.
	Nonce string
	// CodeVerifier is the PKCE verifier whose S256 challenge was sent in URL.
	CodeVerifier string
}

// Provider is one immutable OpenID Connect provider configuration together
// with the concurrency-safe state derived from it: the discovered metadata and
// the cached JWKS. Every method is safe for concurrent use.
//
// A Provider performs no network I/O at construction. Discover fetches and
// validates the discovery document explicitly; Begin and Exchange never
// trigger discovery on their own. The JWKS is fetched lazily on the first
// token verification and cached with a TTL, so a Provider that is never used
// opens no connections.
//
// String, Format, and LogValue render the provider with the client secret
// redacted, including for %#v and structured log records.
type Provider struct {
	settings
	client      *http.Client
	initialized bool

	// discoverMu serializes discovery so a slow earlier document cannot
	// overwrite a newer one.
	discoverMu sync.Mutex

	// mu protects metadata and the JWKS cache published fields.
	mu sync.RWMutex
	// metadata is replaced as a whole and never mutated after publication.
	metadata *providerMetadata
	// keys is the last successfully fetched key set. It stays in place after
	// its TTL expires but is no longer used: an expired set is refreshed
	// before any key is selected from it. keysURI records which discovered
	// key set URL produced it, so a set fetched from superseded metadata is
	// never used after a later Discover.
	keys            jose.JSONWebKeySet
	keysURI         string
	keysFetched     bool
	keysFetchedAt   time.Time
	keysAttempted   bool
	keysAttemptedAt time.Time

	// keysMu serializes JWKS fetches: concurrent verifications that miss the
	// cache share a single outbound request.
	keysMu sync.Mutex
}

// New validates config without network I/O and returns a provider ready for
// Discover. The provider is immutable; replace it through the registry to
// apply a configuration change.
func New(config Config) (*Provider, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	client, err := outbound.NewClient(config.Policy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return &Provider{settings: normalized, client: client, initialized: true}, nil
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
	return fmt.Sprintf("oidc.Provider{Name:%q Issuer:%q ClientID:%q ClientSecret:[REDACTED] RedirectURI:%q Scopes:%v TokenEndpointAuthMethod:%q AllowedAlgorithms:%v}",
		p.name, p.issuer, p.clientID, p.redirectURI, p.scopes, p.authMethod, p.algorithms)
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

// Discover fetches the discovery document derived from the registered issuer
// and stores the validated metadata. The document's issuer must equal the
// registered issuer exactly. A successful discovery discards the cached JWKS,
// because the document may name a different key set.
//
// Discover is the only operation that fetches discovery metadata; Begin and
// Exchange report ErrNotDiscovered until it has succeeded.
func (p *Provider) Discover(ctx context.Context) error {
	if err := p.usable(); err != nil {
		return err
	}
	if ctx == nil {
		return ErrNilContext
	}
	p.discoverMu.Lock()
	defer p.discoverMu.Unlock()

	metadata, err := p.fetchMetadata(ctx, discoveryEndpoint(p.issuer))
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.metadata = metadata
	p.keys = jose.JSONWebKeySet{}
	p.keysURI = ""
	p.keysFetched = false
	p.keysFetchedAt = time.Time{}
	p.keysAttempted = false
	p.keysAttemptedAt = time.Time{}
	p.mu.Unlock()
	return nil
}

// Begin creates a new authorization-code transaction: fresh state, nonce, and
// PKCE verifier, and the authorization URL that carries their derived values.
// It requires a successful Discover.
func (p *Provider) Begin() (AuthorizationRequest, error) {
	if err := p.usable(); err != nil {
		return AuthorizationRequest{}, err
	}
	metadata, ok := p.discovered()
	if !ok {
		return AuthorizationRequest{}, fmt.Errorf("%w: call Discover before Begin", ErrNotDiscovered)
	}

	state, err := randomToken(transactionEntropyBytes)
	if err != nil {
		return AuthorizationRequest{}, err
	}
	nonce, err := randomToken(transactionEntropyBytes)
	if err != nil {
		return AuthorizationRequest{}, err
	}
	verifier, err := randomToken(transactionEntropyBytes)
	if err != nil {
		return AuthorizationRequest{}, err
	}

	endpoint, err := url.Parse(metadata.authorizationEndpoint)
	if err != nil {
		return AuthorizationRequest{}, fmt.Errorf("%w: authorization endpoint is unusable: %v", ErrDiscovery, err)
	}
	query := endpoint.Query()
	query.Set("response_type", "code")
	query.Set("client_id", p.clientID)
	query.Set("redirect_uri", p.redirectURI)
	query.Set("scope", p.scope)
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", codeChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	endpoint.RawQuery = query.Encode()

	return AuthorizationRequest{
		URL:          endpoint.String(),
		State:        state,
		Nonce:        nonce,
		CodeVerifier: verifier,
	}, nil
}

// Exchange redeems an authorization code and returns the verified external
// identity. The transaction must be the one Begin created: its nonce must be
// echoed by the ID token, and its PKCE verifier must match the challenge the
// IdP saw. A transaction without both is rejected before any network call.
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
	if request.Nonce == "" {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: nonce is empty; a transaction must be created by Begin", ErrInvalidTransaction)
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
	metadata, ok := p.discovered()
	if !ok {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: call Discover before Exchange", ErrNotDiscovered)
	}

	response, err := p.exchangeCode(ctx, metadata.tokenEndpoint, code, request.CodeVerifier)
	if err != nil {
		return federation.ExternalIdentity{}, err
	}
	return p.verifyIDToken(ctx, response, request.Nonce)
}

// usable reports whether the provider was constructed by New. The zero
// Provider has no client and no settings; reaching the network with it would
// panic instead of returning an error.
func (p *Provider) usable() error {
	if p == nil {
		return ErrNilProvider
	}
	if !p.initialized || p.client == nil || p.now == nil {
		return fmt.Errorf("%w: provider was not constructed by New", ErrInvalidConfig)
	}
	return nil
}

// discovered returns the current metadata snapshot.
func (p *Provider) discovered() (*providerMetadata, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.metadata, p.metadata != nil
}

// discoveryEndpoint derives the OIDC discovery URL from the registered
// issuer. The document location is never accepted from a request or from
// configuration other than the issuer itself.
func discoveryEndpoint(issuer string) string {
	return strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
}
