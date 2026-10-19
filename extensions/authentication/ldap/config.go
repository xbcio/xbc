package ldap

import (
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	ldapclient "github.com/go-ldap/ldap/v3"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

const (
	// protocolName is the value recorded in ExternalIdentity.Protocol.
	protocolName = "ldap"

	// defaultSubjectAttribute is the directory attribute used as the stable
	// external subject. entryUUID (RFC 4530) is the standard stable identifier;
	// uid and mail are mutable and reassignable and must be configured only
	// with a deliberate reason.
	defaultSubjectAttribute = "entryUUID"

	// defaultDialTimeout bounds one connection attempt.
	defaultDialTimeout = 5 * time.Second
	// defaultOperationTimeout bounds one bind or search request.
	defaultOperationTimeout = 10 * time.Second

	// maxUsernameLength bounds the user-supplied name before it is escaped and
	// placed into a search filter.
	maxUsernameLength = 1024
	// maxIssuerLength bounds the directory identifier recorded in the
	// identity.
	maxIssuerLength = 512
	// maxDNLength bounds a configured distinguished name.
	maxDNLength = 1024
	// maxFilterLength bounds the configured filter template.
	maxFilterLength = 512
	// maxAttributeNameLength bounds an attribute description.
	maxAttributeNameLength = 128
	// maxClaimNameLength bounds a local claim name.
	maxClaimNameLength = 128
)

// Config is the static configuration of one LDAP directory.
//
// New validates every field without performing network I/O; registry
// registration only stores a provider New already accepted. None of the fields
// may be changed after construction: a provider is immutable, so a
// configuration update is a new Provider stored through Registry.Put.
//
// String, Format, and LogValue render ServiceBindPassword as [REDACTED].
// Format is what makes the guarantee hold for the fmt verbs that dispatch
// methods, including %#v. The one escape is %p, which fmt handles before any
// method dispatch: never apply it to a Config value.
type Config struct {
	// Name is the stable application-chosen provider identifier. It is
	// recorded in ExternalIdentity.Provider and must match the registry key
	// the provider is stored under.
	Name string
	// Issuer is the directory identifier recorded in ExternalIdentity.Issuer
	// and in the idp_issuer session attribute. It is an application-chosen
	// stable name, for example "ldaps://directory.example.com" or
	// "corp-directory"; LDAP defines no issuer field of its own.
	Issuer string
	// URL is the directory endpoint. The scheme must be ldaps (implicit TLS)
	// or ldap with StartTLS set (explicit TLS). A path, query, fragment, or
	// userinfo component is rejected. The port defaults to 636 for ldaps and
	// 389 for ldap.
	URL string
	// StartTLS enables the StartTLS extended operation on an ldap:// URL. It
	// is required for ldap:// and rejected for ldaps://. Plaintext LDAP is
	// never supported.
	StartTLS bool
	// RootCAs adds trusted roots for the directory certificate. It only adds
	// roots; it can never disable verification. Nil selects the system roots.
	RootCAs *x509.CertPool
	// ServiceBindDN is the distinguished name of the least-privilege service
	// account used to search for the user entry. It is required.
	ServiceBindDN string
	// ServiceBindPassword is the service account password. It is sent only in
	// the bind request and never appears in an error or a log view.
	ServiceBindPassword outbound.Secret
	// BaseDN is the search base for the user entry. The search always uses
	// the whole-subtree scope. It is required.
	BaseDN string
	// UserFilter is the search filter template. It must contain exactly one
	// %s, which receives the RFC 4515-escaped user name, and no other %
	// verb; a filter such as "(uid=%s)" is the intended shape. It is
	// required.
	UserFilter string
	// SubjectAttribute is the directory attribute whose value becomes
	// ExternalIdentity.Subject. It must be stable and never reused for a
	// different person; uid and mail do not qualify. Empty selects entryUUID.
	SubjectAttribute string
	// ClaimAttributes maps local claim names to directory attribute names,
	// for example {"email": "mail", "display_name": "cn"}. Every configured
	// attribute must be present in the user entry and valid UTF-8; a missing
	// attribute fails the login instead of being guessed. Nil maps no claims.
	ClaimAttributes map[string]string
	// AllowPrivate admits loopback, RFC 1918, carrier-grade NAT, and
	// unique-local addresses for local development against a test directory.
	// It never admits link-local addresses: cloud metadata endpoints live
	// there. It must stay disabled in production.
	AllowPrivate bool
	// DialTimeout bounds one connection attempt. Zero selects 5s. Negative
	// values are rejected.
	DialTimeout time.Duration
	// OperationTimeout bounds one bind or search request and the TLS
	// handshake. Zero selects 10s. Negative values are rejected.
	OperationTimeout time.Duration
	// Now is the clock stamped into ExternalIdentity.AuthTime. Nil selects
	// time.Now.
	Now func() time.Time
}

// Validate reports whether the configuration satisfies every static rule New
// enforces. It performs no network I/O and never renders the service password.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// String renders the configuration for diagnostics with the service bind
// password redacted.
func (c Config) String() string {
	return fmt.Sprintf("ldap.Config{Name:%q Issuer:%q URL:%q StartTLS:%t ServiceBindDN:%q ServiceBindPassword:%v BaseDN:%q UserFilter:%q SubjectAttribute:%q ClaimAttributes:%v AllowPrivate:%t DialTimeout:%s OperationTimeout:%s}",
		c.Name, c.Issuer, c.URL, c.StartTLS, c.ServiceBindDN, c.ServiceBindPassword,
		c.BaseDN, c.UserFilter, c.SubjectAttribute, c.ClaimAttributes, c.AllowPrivate,
		c.DialTimeout, c.OperationTimeout)
}

// Format implements fmt.Formatter so every verb renders the redacted string,
// including %#v, which would otherwise print the raw struct value.
func (c Config) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// LogValue implements slog.LogValuer with the service bind password redacted.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// settings is the immutable, normalized form of a Config. It is the only
// representation the Provider carries, so a later mutation of the caller's
// Config cannot change a live provider.
type settings struct {
	name          string
	issuer        string
	scheme        string
	host          string
	port          string
	startTLS      bool
	rootCAs       *x509.CertPool
	serviceBindDN string
	// serviceBindPassword keeps the Secret type so that formatting the field
	// value itself always applies Secret's redaction. That alone does not cover
	// a dereferenced Provider value: fmt does not dispatch methods on
	// unexported struct fields. Provider.Format carries a value receiver for
	// that case.
	serviceBindPassword outbound.Secret
	baseDN              string
	userFilter          string
	subjectAttribute    string
	claimAttributes     map[string]string
	allowPrivate        bool
	dialTimeout         time.Duration
	operationTimeout    time.Duration
	now                 func() time.Time
}

// normalizeConfig validates config and freezes it into settings. Every error
// wraps ErrInvalidConfig. No message includes the service password, a
// configured distinguished name, or a filter template value.
func normalizeConfig(config Config) (settings, error) {
	fail := func(format string, arguments ...any) (settings, error) {
		return settings{}, fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, arguments...))
	}

	if !validProviderName(config.Name) {
		return fail("Name %q is not a valid provider name", config.Name)
	}
	issuer := strings.TrimSpace(config.Issuer)
	if !validDirectoryText(issuer, maxIssuerLength) {
		return fail("Issuer is empty, too long, or contains control characters")
	}
	endpoint, err := parseDirectoryURL(config.URL, config.StartTLS)
	if err != nil {
		return fail("%v", err)
	}
	serviceBindDN := strings.TrimSpace(config.ServiceBindDN)
	if !validDN(serviceBindDN) {
		return fail("ServiceBindDN is not a non-empty valid distinguished name")
	}
	if config.ServiceBindPassword == "" {
		return fail("ServiceBindPassword is required")
	}
	baseDN := strings.TrimSpace(config.BaseDN)
	if !validDN(baseDN) {
		return fail("BaseDN is not a non-empty valid distinguished name")
	}
	filter := strings.TrimSpace(config.UserFilter)
	if err := validateUserFilter(filter); err != nil {
		return fail("UserFilter %v", err)
	}

	subjectAttribute := strings.TrimSpace(config.SubjectAttribute)
	if subjectAttribute == "" {
		subjectAttribute = defaultSubjectAttribute
	}
	if !validAttributeDescription(subjectAttribute) {
		return fail("SubjectAttribute %q is not a valid attribute description", subjectAttribute)
	}

	claimAttributes, err := normalizeClaimAttributes(config.ClaimAttributes)
	if err != nil {
		return fail("%v", err)
	}

	dialTimeout := config.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = defaultDialTimeout
	}
	if dialTimeout < 0 {
		return fail("DialTimeout must not be negative")
	}
	operationTimeout := config.OperationTimeout
	if operationTimeout == 0 {
		operationTimeout = defaultOperationTimeout
	}
	if operationTimeout < 0 {
		return fail("OperationTimeout must not be negative")
	}

	now := config.Now
	if now == nil {
		now = time.Now
	}

	return settings{
		name:                config.Name,
		issuer:              issuer,
		scheme:              endpoint.scheme,
		host:                endpoint.host,
		port:                endpoint.port,
		startTLS:            config.StartTLS,
		rootCAs:             config.RootCAs,
		serviceBindDN:       serviceBindDN,
		serviceBindPassword: config.ServiceBindPassword,
		baseDN:              baseDN,
		userFilter:          filter,
		subjectAttribute:    subjectAttribute,
		claimAttributes:     claimAttributes,
		allowPrivate:        config.AllowPrivate,
		dialTimeout:         dialTimeout,
		operationTimeout:    operationTimeout,
		now:                 now,
	}, nil
}

// endpoint is the parsed, normalized form of Config.URL.
type endpoint struct {
	scheme string
	host   string
	port   string
}

// parseDirectoryURL validates the directory URL and applies the TLS rules:
// ldaps means implicit TLS, ldap requires StartTLS, and there is no plaintext
// mode. It never has to mention the configured URL value, which keeps
// connection strings out of error text.
func parseDirectoryURL(raw string, startTLS bool) (endpoint, error) {
	if raw == "" {
		return endpoint{}, fmt.Errorf("URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.Opaque != "" {
		return endpoint{}, fmt.Errorf("URL is not an absolute directory URL")
	}
	if parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return endpoint{}, fmt.Errorf("URL must not carry userinfo, a query, or a fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return endpoint{}, fmt.Errorf("URL must not carry a path")
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "ldap", "ldaps":
	default:
		return endpoint{}, fmt.Errorf("URL scheme must be ldap or ldaps")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return endpoint{}, fmt.Errorf("URL has an empty port")
	}
	host := parsed.Hostname()
	if host == "" {
		return endpoint{}, fmt.Errorf("URL has no host")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return endpoint{}, fmt.Errorf("URL host must not carry an IPv6 zone")
		}
	} else if !validHostName(host) {
		return endpoint{}, fmt.Errorf("URL host is not a valid host name or IP address")
	}

	port := parsed.Port()
	if port == "" {
		if scheme == "ldaps" {
			port = "636"
		} else {
			port = "389"
		}
	} else if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return endpoint{}, fmt.Errorf("URL port is not in 1..65535")
	}

	switch {
	case scheme == "ldaps" && startTLS:
		return endpoint{}, fmt.Errorf("StartTLS applies only to ldap:// URLs; ldaps:// is already encrypted")
	case scheme == "ldap" && !startTLS:
		return endpoint{}, fmt.Errorf("plaintext ldap:// is not supported; use ldaps:// or enable StartTLS")
	}
	return endpoint{scheme: scheme, host: host, port: port}, nil
}

// validProviderName enforces the provider-name rule shared by every protocol
// module. The federation registry applies the same rule before its validator
// runs; this copy keeps the static error local to this module.
func validProviderName(name string) bool {
	if len(name) == 0 || len(name) > federationMaxProviderNameLength {
		return false
	}
	for index, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && index > 0:
		default:
			return false
		}
	}
	return true
}

// federationMaxProviderNameLength mirrors federation.MaxProviderNameLength
// without importing a second package for one constant.
const federationMaxProviderNameLength = 64

// validDirectoryText reports whether value is non-empty, no longer than limit,
// and free of control characters. Surrounding whitespace must already be
// trimmed.
func validDirectoryText(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validDN reports whether value is a non-empty distinguished name that the
// LDAP client can parse.
func validDN(value string) bool {
	if !validDirectoryText(value, maxDNLength) {
		return false
	}
	parsed, err := ldapclient.ParseDN(value)
	return err == nil && len(parsed.RDNs) > 0
}

// validHostName applies a conservative host-name check: the value comes from a
// URL and reaches both the resolver and the TLS server name.
func validHostName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// validateUserFilter checks the filter template without running it: exactly
// one %s placeholder, no other % verb, balanced parentheses, and the shape of
// an LDAP filter.
func validateUserFilter(filter string) error {
	if filter == "" {
		return fmt.Errorf("is required")
	}
	if len(filter) > maxFilterLength {
		return fmt.Errorf("is longer than %d bytes", maxFilterLength)
	}
	if strings.Count(filter, "%s") != 1 {
		return fmt.Errorf("must contain exactly one %%s placeholder")
	}
	if strings.Contains(strings.ReplaceAll(filter, "%s", ""), "%") {
		return fmt.Errorf("must not contain a %% verb other than the single %%s placeholder")
	}
	if !strings.HasPrefix(filter, "(") || !strings.HasSuffix(filter, ")") {
		return fmt.Errorf("must be wrapped in parentheses")
	}
	depth := 0
	for _, r := range filter {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return fmt.Errorf("has unbalanced parentheses")
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("has unbalanced parentheses")
	}
	return nil
}

// normalizeClaimAttributes copies and validates the claim mapping. Claim
// names and attribute names are both lower-cased for lookups later; the
// caller's map and its values are never retained by reference.
func normalizeClaimAttributes(claims map[string]string) (map[string]string, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	normalized := make(map[string]string, len(claims))
	for claim, attribute := range claims {
		if !validClaimName(claim) {
			return nil, fmt.Errorf("claim name %q is not valid", claim)
		}
		attribute = strings.TrimSpace(attribute)
		if !validAttributeDescription(attribute) {
			return nil, fmt.Errorf("claim %q names an invalid attribute description", claim)
		}
		normalized[claim] = attribute
	}
	return normalized, nil
}

// validClaimName accepts the characters a session attribute key may carry.
func validClaimName(name string) bool {
	if name == "" || len(name) > maxClaimNameLength {
		return false
	}
	for index, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
		default:
			return false
		}
		if index == 0 && (r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// validAttributeDescription accepts a name such as entryUUID or cn and a
// numeric OID such as 1.2.840.113556.1.4.1.
func validAttributeDescription(name string) bool {
	if name == "" || len(name) > maxAttributeNameLength {
		return false
	}
	if name[0] >= '0' && name[0] <= '9' {
		for _, r := range name {
			if (r < '0' || r > '9') && r != '.' {
				return false
			}
		}
		return !strings.HasPrefix(name, ".") && !strings.HasSuffix(name, ".") && !strings.Contains(name, "..")
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}
