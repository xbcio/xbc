package oidc

import (
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

const (
	// protocolName is the value recorded in ExternalIdentity.Protocol.
	protocolName = "oidc"

	defaultLeeway                 = 30 * time.Second
	maxLeeway                     = 60 * time.Second
	defaultJWKSCacheTTL           = 5 * time.Minute
	defaultJWKSMinRefreshInterval = 30 * time.Second

	// maxAuthorizationCodeLength bounds the one-time code accepted from a
	// callback. A code is an opaque IdP value; a value this long is not a
	// legitimate authorization code.
	maxAuthorizationCodeLength = 8192
)

// defaultAlgorithms is the signature allowlist a configuration inherits when
// it names none: one RSA and one ECDSA algorithm from the SHA-256 family.
var defaultAlgorithms = []string{"RS256", "ES256"}

// TokenEndpointAuthMethod is the client authentication method used at the
// token endpoint. The method must be configured explicitly; it is never
// inferred from the discovery document, because an IdP advertising a method
// is not a decision about which credential leaves this process.
type TokenEndpointAuthMethod string

const (
	// ClientSecretBasic sends the client credentials in the Authorization
	// header of the token request. It is the default.
	ClientSecretBasic TokenEndpointAuthMethod = "client_secret_basic"
	// ClientSecretPost sends the client credentials in the request body of
	// the token request.
	ClientSecretPost TokenEndpointAuthMethod = "client_secret_post"
)

// Config is the static configuration of one OpenID Connect provider.
//
// New validates every field without performing network I/O; registry
// registration only stores a provider New already accepted. None of the fields
// may be changed after construction: a provider is immutable, so a
// configuration update is a new Provider stored through Registry.Put.
//
// String, Format, and LogValue render ClientSecret as [REDACTED]. Format is
// what makes the guarantee hold for the fmt verbs that dispatch methods,
// including %#v. The one escape is %p, which fmt handles before any method
// dispatch: never apply it to a Config value.
type Config struct {
	// Name is the stable application-chosen provider identifier. It is
	// recorded in ExternalIdentity.Provider and must match the registry key
	// the provider is stored under.
	Name string
	// Issuer is the registered OIDC issuer identifier. The discovery
	// document is derived from it, and the iss claim of every ID token must
	// match it exactly.
	Issuer string
	// ClientID is the OAuth 2.0 client identifier registered with the IdP.
	ClientID string
	// ClientSecret is the client credential for the token endpoint. It is
	// sent only in the Authorization header or the request body, never in a
	// URL, and it never appears in an error or a log view.
	ClientSecret outbound.Secret
	// RedirectURI is the exact callback URL registered with the IdP. It is
	// sent verbatim; wildcards are not supported and are rejected. Plain HTTP
	// requires the AllowHTTP development switch.
	RedirectURI string
	// Scopes is the requested scope set. It must contain openid. Nil or empty
	// selects ["openid"].
	Scopes []string
	// TokenEndpointAuthMethod selects how client credentials reach the token
	// endpoint. Empty selects ClientSecretBasic.
	TokenEndpointAuthMethod TokenEndpointAuthMethod
	// AllowedAlgorithms is the ID token signature algorithm allowlist. Nil or
	// empty selects ["RS256", "ES256"]. Only asymmetric algorithms are
	// accepted: none and every HMAC algorithm are rejected at construction,
	// so a JWKS public key can never be confused with an HMAC secret.
	AllowedAlgorithms []string
	// Leeway is the clock skew applied to exp, nbf, and iat. Empty selects
	// 30 seconds; values above 60 seconds are rejected, so the window can
	// never be widened without bound.
	Leeway time.Duration
	// JWKSCacheTTL is how long a fetched JWKS stays usable. Empty selects
	// 5 minutes. It must not be shorter than JWKSMinRefreshInterval.
	JWKSCacheTTL time.Duration
	// JWKSMinRefreshInterval is the minimum interval between two JWKS
	// fetches. Empty selects 30 seconds. An unknown key ID is only fetched
	// again after this interval has passed, so an attacker cannot turn token
	// verification into a request amplifier.
	JWKSMinRefreshInterval time.Duration
	// Policy governs every outbound request: discovery, JWKS, and the token
	// endpoint. The zero Policy selects the production defaults. AllowHTTP
	// and AllowPrivate are development switches and must stay disabled in
	// production.
	Policy outbound.Policy
	// Now is the clock used for token validation and cache freshness. Nil
	// selects time.Now.
	Now func() time.Time
}

// Validate reports whether the configuration satisfies every static rule New
// enforces. It performs no network I/O and never renders the client secret.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// String renders the configuration for diagnostics with the client secret
// redacted.
func (c Config) String() string {
	return fmt.Sprintf("oidc.Config{Name:%q Issuer:%q ClientID:%q ClientSecret:%v RedirectURI:%q Scopes:%v TokenEndpointAuthMethod:%q AllowedAlgorithms:%v}",
		c.Name, c.Issuer, c.ClientID, c.ClientSecret, c.RedirectURI, c.Scopes, c.TokenEndpointAuthMethod, c.AllowedAlgorithms)
}

// Format implements fmt.Formatter so every verb renders the redacted string,
// including %#v, which would otherwise print the raw struct value.
func (c Config) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// LogValue implements slog.LogValuer with the client secret redacted.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// settings is the immutable, normalized form of a Config. It is the only
// representation the Provider carries, so a later mutation of the caller's
// Config cannot change a live provider.
type settings struct {
	name         string
	issuer       string
	clientID     string
	clientSecret string
	redirectURI  string
	scopes       []string
	scope        string
	authMethod   TokenEndpointAuthMethod
	algorithms   []string
	leeway       time.Duration
	jwksTTL      time.Duration
	jwksRefresh  time.Duration
	policy       outbound.Policy
	now          func() time.Time
}

// normalizeConfig validates config and freezes it into settings. Every error
// wraps ErrInvalidConfig and is safe to log: no field value that reaches a
// message is a credential.
func normalizeConfig(config Config) (settings, error) {
	fail := func(format string, arguments ...any) (settings, error) {
		return settings{}, fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, arguments...))
	}

	if !validProviderName(config.Name) {
		return fail("Name %q is not a valid provider name", config.Name)
	}
	issuer, err := parseIssuer(config.Issuer, config.Policy.AllowHTTP)
	if err != nil {
		return fail("Issuer %q is not usable: %v", config.Issuer, err)
	}
	if !validCredentialText(config.ClientID) {
		return fail("ClientID is empty or contains whitespace or control characters")
	}
	if config.ClientSecret == "" {
		return fail("ClientSecret is required for both supported token endpoint authentication methods")
	}
	if _, err := parseRedirectURI(config.RedirectURI, config.Policy.AllowHTTP); err != nil {
		return fail("RedirectURI %q is not usable: %v", config.RedirectURI, err)
	}
	scopes, err := normalizeScopes(config.Scopes)
	if err != nil {
		return fail("%v", err)
	}

	authMethod := config.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = ClientSecretBasic
	}
	switch authMethod {
	case ClientSecretBasic, ClientSecretPost:
	default:
		return fail("TokenEndpointAuthMethod %q is not supported", authMethod)
	}

	algorithms, err := normalizeAlgorithms(config.AllowedAlgorithms)
	if err != nil {
		return fail("%v", err)
	}

	leeway := config.Leeway
	switch {
	case leeway < 0:
		return fail("Leeway must not be negative")
	case leeway == 0:
		leeway = defaultLeeway
	case leeway > maxLeeway:
		return fail("Leeway must not exceed %s", maxLeeway)
	}

	ttl := config.JWKSCacheTTL
	switch {
	case ttl < 0:
		return fail("JWKSCacheTTL must not be negative")
	case ttl == 0:
		ttl = defaultJWKSCacheTTL
	}
	refresh := config.JWKSMinRefreshInterval
	switch {
	case refresh < 0:
		return fail("JWKSMinRefreshInterval must not be negative")
	case refresh == 0:
		refresh = defaultJWKSMinRefreshInterval
	}
	if ttl < refresh {
		return fail("JWKSCacheTTL %s must not be shorter than JWKSMinRefreshInterval %s", ttl, refresh)
	}

	if config.Policy.DialTimeout < 0 || config.Policy.TLSHandshakeTimeout < 0 ||
		config.Policy.ResponseHeaderTimeout < 0 || config.Policy.TotalTimeout < 0 ||
		config.Policy.MaxBodyBytes < 0 {
		return fail("Policy must not carry a negative timeout or body limit")
	}

	now := config.Now
	if now == nil {
		now = time.Now
	}

	return settings{
		name:         config.Name,
		issuer:       issuer,
		clientID:     config.ClientID,
		clientSecret: string(config.ClientSecret),
		redirectURI:  config.RedirectURI,
		scopes:       scopes,
		scope:        strings.Join(scopes, " "),
		authMethod:   authMethod,
		algorithms:   algorithms,
		leeway:       leeway,
		jwksTTL:      ttl,
		jwksRefresh:  refresh,
		policy:       config.Policy,
		now:          now,
	}, nil
}

// validProviderName mirrors the federation registry rule
// ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ so an invalid name fails at construction
// instead of at Registry.Put.
func validProviderName(name string) bool {
	if len(name) == 0 || len(name) > federation.MaxProviderNameLength {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case index > 0 && (character == '_' || character == '-'):
		default:
			return false
		}
	}
	return true
}

// validCredentialText reports whether value is non-empty and free of
// whitespace and ASCII control characters. Client identifiers and scope tokens
// travel inside form bodies and Basic credentials, where whitespace would be
// silently re-encoded.
func validCredentialText(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= ' ' || character == 0x7f {
			return false
		}
	}
	return true
}

// parseIssuer validates an issuer identifier and returns it unchanged, so the
// exact configured string is what the iss claim is compared against. An issuer
// is an https URL without userinfo, query, or fragment; plain HTTP is admitted
// only through the AllowHTTP development switch.
func parseIssuer(raw string, allowHTTP bool) (string, error) {
	target, err := parseEndpoint(raw, allowHTTP)
	if err != nil {
		return "", err
	}
	if target.RawQuery != "" {
		return "", fmt.Errorf("an issuer must not carry a query")
	}
	return raw, nil
}

// parseRedirectURI validates the callback URL. A query component is allowed --
// registered redirect URIs may carry application state -- but a wildcard, a
// fragment, or userinfo is not. Plain HTTP is admitted only through the same
// AllowHTTP development switch that governs the issuer, so a production
// configuration can never return an authorization code over cleartext.
func parseRedirectURI(raw string, allowHTTP bool) (string, error) {
	if strings.Contains(raw, "*") {
		return "", fmt.Errorf("wildcards are not supported")
	}
	if _, err := parseEndpoint(raw, allowHTTP); err != nil {
		return "", err
	}
	return raw, nil
}

// parseEndpoint validates an absolute http(s) URL with a host, no userinfo,
// and no fragment. Endpoints discovered from an IdP pass the same rule, so a
// document cannot point the client at a relative path or a non-HTTP scheme.
func parseEndpoint(raw string, allowHTTP bool) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("URL is empty")
	}
	target, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !target.IsAbs() || target.Host == "" || target.Opaque != "" {
		return nil, fmt.Errorf("URL must be absolute with a host")
	}
	if target.User != nil {
		return nil, fmt.Errorf("URL must not carry userinfo")
	}
	if target.Fragment != "" {
		return nil, fmt.Errorf("URL must not carry a fragment")
	}
	switch strings.ToLower(target.Scheme) {
	case "https":
	case "http":
		if !allowHTTP {
			return nil, fmt.Errorf("plain HTTP requires the AllowHTTP development switch")
		}
	default:
		return nil, fmt.Errorf("scheme %q is not http or https", target.Scheme)
	}
	return target, nil
}

// normalizeScopes validates the requested scope set, requires openid, and
// removes duplicates while preserving order.
func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return []string{"openid"}, nil
	}
	normalized := make([]string, 0, len(scopes))
	seen := make(map[string]bool, len(scopes))
	hasOpenID := false
	for _, scope := range scopes {
		if !validCredentialText(scope) || strings.ContainsAny(scope, " \t") {
			return nil, fmt.Errorf("scope %q must be a single non-empty token", scope)
		}
		if scope == "openid" {
			hasOpenID = true
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		normalized = append(normalized, scope)
	}
	if !hasOpenID {
		return nil, fmt.Errorf("Scopes must contain openid")
	}
	return normalized, nil
}

// normalizeAlgorithms validates the signature allowlist. Only asymmetric
// algorithms are accepted: alg=none and the HMAC family are rejected here, so
// no configuration can reintroduce the algorithm-confusion class of attacks.
func normalizeAlgorithms(algorithms []string) ([]string, error) {
	if len(algorithms) == 0 {
		return append([]string(nil), defaultAlgorithms...), nil
	}
	normalized := make([]string, 0, len(algorithms))
	seen := make(map[string]bool, len(algorithms))
	for _, algorithm := range algorithms {
		if _, ok := signatureHash(algorithm); !ok {
			return nil, fmt.Errorf("signature algorithm %q is not an allowed asymmetric algorithm", algorithm)
		}
		if seen[algorithm] {
			continue
		}
		seen[algorithm] = true
		normalized = append(normalized, algorithm)
	}
	return normalized, nil
}

// signatureHash returns the hash an allowed signature algorithm uses. The same
// hash defines at_hash, so both mappings live in one place.
func signatureHash(algorithm string) (func() hash.Hash, bool) {
	switch algorithm {
	case "RS256", "PS256", "ES256":
		return sha256.New, true
	case "RS384", "PS384", "ES384":
		return sha512.New384, true
	case "RS512", "PS512", "ES512":
		return sha512.New, true
	default:
		return nil, false
	}
}
