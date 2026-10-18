package oauth2

import (
	"fmt"
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
	protocolName = "oauth2"

	// maxAuthorizationCodeLength bounds the one-time code accepted from a
	// callback. A code is an opaque IdP value; a value this long is not a
	// legitimate authorization code.
	maxAuthorizationCodeLength = 8192
)

// TokenEndpointAuthMethod is the client authentication method used at the
// token endpoint. The method must be configured explicitly; it is never
// inferred from provider metadata, because a provider advertising a method is
// not a decision about which credential leaves this process.
type TokenEndpointAuthMethod string

const (
	// ClientSecretBasic sends the client credentials in the Authorization
	// header of the token request. It is the default.
	ClientSecretBasic TokenEndpointAuthMethod = "client_secret_basic"
	// ClientSecretPost sends the client credentials in the request body of the
	// token request.
	ClientSecretPost TokenEndpointAuthMethod = "client_secret_post"
)

// Config is the static configuration of one OAuth 2.0 provider.
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
	// Issuer is the IdP identifier recorded in ExternalIdentity.Issuer and in
	// the idp_issuer session attribute. OAuth 2.0 defines no issuer claim, so
	// this value is the application's stable name for the provider.
	Issuer string
	// AuthorizationEndpoint is the absolute URL the browser is redirected to.
	// A fixed query component is allowed and preserved.
	AuthorizationEndpoint string
	// TokenEndpoint is the absolute URL that redeems the authorization code.
	// A query component is rejected: the request body carries every parameter,
	// and credentials must never travel in a URL.
	TokenEndpoint string
	// UserInfoEndpoint is the absolute URL that returns the account document
	// used to build the identity. It is requested verbatim, with no redirects
	// and no added query parameters.
	UserInfoEndpoint string
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
	// Scopes is the requested scope set. Nil or empty sends no scope
	// parameter.
	Scopes []string
	// TokenEndpointAuthMethod selects how client credentials reach the token
	// endpoint. Empty selects ClientSecretBasic.
	TokenEndpointAuthMethod TokenEndpointAuthMethod
	// SubjectField is the dotted path to the IdP's stable account identifier
	// in the userinfo response, for example "sub" or "account.id". It is
	// required. The value at the path must be a non-empty JSON string. It must
	// name a stable account identifier, never an email address or a login
	// name: those change and are reassigned.
	SubjectField string
	// ClaimFields maps local claim names to dotted paths in the userinfo
	// response, for example {"email": "email", "name": "profile.name"}. Every
	// configured path must be present in the response; a missing field fails
	// the login instead of being guessed or defaulted. Nil maps no claims.
	ClaimFields map[string]string
	// Policy governs every outbound request: the token endpoint and the
	// userinfo endpoint. The zero Policy selects the production defaults.
	// AllowHTTP and AllowPrivate are development switches and must stay
	// disabled in production.
	Policy outbound.Policy
	// Now is the clock stamped into ExternalIdentity.AuthTime. Nil selects
	// time.Now.
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
	return fmt.Sprintf("oauth2.Config{Name:%q Issuer:%q AuthorizationEndpoint:%q TokenEndpoint:%q UserInfoEndpoint:%q ClientID:%q ClientSecret:%v RedirectURI:%q Scopes:%v TokenEndpointAuthMethod:%q SubjectField:%q ClaimFields:%v}",
		c.Name, c.Issuer, c.AuthorizationEndpoint, c.TokenEndpoint, c.UserInfoEndpoint,
		c.ClientID, c.ClientSecret, c.RedirectURI, c.Scopes, c.TokenEndpointAuthMethod,
		c.SubjectField, c.ClaimFields)
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
	name                  string
	issuer                string
	authorizationEndpoint string
	tokenEndpoint         string
	userInfoEndpoint      string
	clientID              string
	clientSecret          string
	redirectURI           string
	scopes                []string
	scope                 string
	authMethod            TokenEndpointAuthMethod
	subjectField          string
	subjectPath           []string
	claimPaths            map[string][]string
	policy                outbound.Policy
	now                   func() time.Time
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
	if _, err := parseEndpoint(config.AuthorizationEndpoint, config.Policy.AllowHTTP, true); err != nil {
		return fail("AuthorizationEndpoint %q is not usable: %v", config.AuthorizationEndpoint, err)
	}
	if _, err := parseEndpoint(config.TokenEndpoint, config.Policy.AllowHTTP, false); err != nil {
		return fail("TokenEndpoint %q is not usable: %v", config.TokenEndpoint, err)
	}
	if _, err := parseEndpoint(config.UserInfoEndpoint, config.Policy.AllowHTTP, false); err != nil {
		return fail("UserInfoEndpoint %q is not usable: %v", config.UserInfoEndpoint, err)
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

	subjectPath, err := parseFieldPath(config.SubjectField)
	if err != nil {
		return fail("SubjectField %q is not usable: %v", config.SubjectField, err)
	}
	claimPaths, err := normalizeClaimFields(config.ClaimFields)
	if err != nil {
		return fail("%v", err)
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
		name:                  config.Name,
		issuer:                issuer,
		authorizationEndpoint: config.AuthorizationEndpoint,
		tokenEndpoint:         config.TokenEndpoint,
		userInfoEndpoint:      config.UserInfoEndpoint,
		clientID:              config.ClientID,
		clientSecret:          string(config.ClientSecret),
		redirectURI:           config.RedirectURI,
		scopes:                scopes,
		scope:                 strings.Join(scopes, " "),
		authMethod:            authMethod,
		subjectField:          config.SubjectField,
		subjectPath:           subjectPath,
		claimPaths:            claimPaths,
		policy:                config.Policy,
		now:                   now,
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
// whitespace and ASCII control characters. Client identifiers, scope tokens,
// and claim names travel inside form bodies, Basic credentials, and session
// attributes, where whitespace would be silently re-encoded or lost.
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

// parseIssuer validates an issuer identifier and returns it unchanged. An
// issuer is an https URL without userinfo, query, or fragment; plain HTTP is
// admitted only through the AllowHTTP development switch.
func parseIssuer(raw string, allowHTTP bool) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("URL is empty")
	}
	if _, err := parseEndpoint(raw, allowHTTP, false); err != nil {
		return "", err
	}
	return raw, nil
}

// parseRedirectURI validates the callback URL. A query component is allowed --
// registered redirect URIs may carry application state -- but a wildcard, a
// fragment, or userinfo is not. Plain HTTP is admitted only through the same
// AllowHTTP development switch that governs the endpoints, so a production
// configuration can never return an authorization code over cleartext.
func parseRedirectURI(raw string, allowHTTP bool) (string, error) {
	if strings.Contains(raw, "*") {
		return "", fmt.Errorf("wildcards are not supported")
	}
	if _, err := parseEndpoint(raw, allowHTTP, true); err != nil {
		return "", err
	}
	return raw, nil
}

// parseEndpoint validates an absolute http(s) URL with a host, no userinfo,
// and no fragment. A query component is admitted only when allowQuery is set.
func parseEndpoint(raw string, allowHTTP bool, allowQuery bool) (*url.URL, error) {
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
	if !allowQuery && target.RawQuery != "" {
		return nil, fmt.Errorf("URL must not carry a query")
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

// normalizeScopes validates the requested scope set and removes duplicates
// while preserving order. An empty set sends no scope parameter.
func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(scopes))
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		if !validCredentialText(scope) {
			return nil, fmt.Errorf("scope %q must be a single non-empty token", scope)
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		normalized = append(normalized, scope)
	}
	return normalized, nil
}

// parseFieldPath splits a dotted path into its segments. A dotted path
// addresses nested JSON objects; a key that itself contains a dot cannot be
// addressed. Empty segments are rejected so a typo fails at construction
// instead of at the first login.
func parseFieldPath(raw string) ([]string, error) {
	if raw == "" {
		return nil, fmt.Errorf("field path is empty")
	}
	segments := strings.Split(raw, ".")
	for _, segment := range segments {
		if segment == "" {
			return nil, fmt.Errorf("field path contains an empty segment")
		}
	}
	return segments, nil
}

// normalizeClaimFields validates the claim mapping and splits every path. The
// returned map is a fresh copy owned by the provider.
func normalizeClaimFields(fields map[string]string) (map[string][]string, error) {
	if len(fields) == 0 {
		return nil, nil
	}
	normalized := make(map[string][]string, len(fields))
	for name, raw := range fields {
		if !validCredentialText(name) {
			return nil, fmt.Errorf("claim name %q must be a single non-empty token", name)
		}
		path, err := parseFieldPath(raw)
		if err != nil {
			return nil, fmt.Errorf("claim %q: %v", name, err)
		}
		normalized[name] = path
	}
	return normalized, nil
}
