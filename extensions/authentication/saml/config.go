package saml

import (
	"crypto/rsa"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// Signature algorithm identifiers accepted in a configuration. SHA-1 is not in
// this list and can never be selected, regardless of what an IdP signs with.
const (
	AlgorithmRSASHA256   = "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"
	AlgorithmRSASHA384   = "http://www.w3.org/2001/04/xmldsig-more#rsa-sha384"
	AlgorithmRSASHA512   = "http://www.w3.org/2001/04/xmldsig-more#rsa-sha512"
	AlgorithmECDSASHA256 = "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha256"
	AlgorithmECDSASHA384 = "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha384"
	AlgorithmECDSASHA512 = "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha512"
)

// Digest algorithm identifiers accepted in a signature reference. SHA-1 is
// deliberately absent.
const (
	digestSHA256 = "http://www.w3.org/2001/04/xmlenc#sha256"
	digestSHA384 = "http://www.w3.org/2001/04/xmldsig-more#sha384"
	digestSHA512 = "http://www.w3.org/2001/04/xmlenc#sha512"
)

// Canonicalization algorithms accepted for SignedInfo and for the reference
// transform chain.
const (
	c14nExclusive           = "http://www.w3.org/2001/10/xml-exc-c14n#"
	c14nExclusiveWithComm   = "http://www.w3.org/2001/10/xml-exc-c14n#WithComments"
	c14n11                  = "http://www.w3.org/2006/12/xml-c14n11"
	c14n11WithComments      = "http://www.w3.org/2006/12/xml-c14n11#WithComments"
	c14n10                  = "http://www.w3.org/TR/2001/REC-xml-c14n-20010315"
	c14n10WithComments      = "http://www.w3.org/TR/2001/REC-xml-c14n-20010315#WithComments"
	envelopedSignature      = "http://www.w3.org/2000/09/xmldsig#enveloped-signature"
	rsaSHA256RedirectSigAlg = AlgorithmRSASHA256
)

const (
	defaultLeeway = 30 * time.Second
	maxLeeway     = 60 * time.Second
)

var (
	allowedSignatureAlgorithms = []string{
		AlgorithmRSASHA256, AlgorithmRSASHA384, AlgorithmRSASHA512,
		AlgorithmECDSASHA256, AlgorithmECDSASHA384, AlgorithmECDSASHA512,
	}
	allowedDigestAlgorithms = map[string]struct{}{
		digestSHA256: {}, digestSHA384: {}, digestSHA512: {},
	}
	allowedCanonicalizations = map[string]struct{}{
		c14nExclusive: {}, c14nExclusiveWithComm: {},
		c14n11: {}, c14n11WithComments: {},
		c14n10: {}, c14n10WithComments: {},
	}
	allowedNameIDFormats = map[string]struct{}{
		NameIDFormatPersistent:  {},
		NameIDFormatUnspecified: {},
	}
)

// Config is the static configuration of one SAML identity provider and the
// service provider it belongs to.
//
// New validates every field without network I/O. None of the fields may be
// changed after construction: a provider is immutable, so a configuration
// update is a new Provider stored through Registry.Put. IdP metadata is loaded
// by an explicit LoadMetadata call, never by New or by registry writes.
//
// String, Format, and LogValue redact SPPrivateKey unconditionally and never
// render certificate bodies. Format is what makes the guarantee hold for the
// fmt verbs that dispatch methods, including %#v. The one escape is %p, which
// fmt handles before any method dispatch: never apply it to a Config value.
type Config struct {
	// Name is the stable application-chosen provider identifier. It is
	// recorded in ExternalIdentity.Provider and must match the registry key
	// the provider is stored under.
	Name string
	// Issuer is the IdP entityID. It is recorded in
	// ExternalIdentity.Issuer, and both the Response and the Assertion must
	// name it exactly.
	Issuer string
	// SSOURL is the IdP's HTTP-Redirect single-sign-on endpoint. It must be
	// https unless Policy.AllowHTTP is set, and it must not carry a query.
	SSOURL string
	// EntityID is this service provider's entityID, published in SP metadata
	// and matched against every Audience restriction.
	EntityID string
	// ACSURL is the assertion consumer service URL. It is published in SP
	// metadata, sent in AuthnRequests, and must equal both the response
	// Destination and the bearer SubjectConfirmationData Recipient.
	ACSURL string
	// IdPCertificates are the trusted assertion-signing certificates. More
	// than one may be configured so a rotation period accepts the old and the
	// new certificate at the same time.
	IdPCertificates []*x509.Certificate
	// SPCertificate is the public service-provider certificate published in
	// SP metadata. SPPrivateKey, when set, must match it.
	SPCertificate *x509.Certificate
	// SPPrivateKey optionally signs AuthnRequests with RSA-SHA256 in the
	// HTTP-Redirect binding. It is never published in metadata and never
	// appears in an error or a log view. Decryption and SLO are out of scope.
	SPPrivateKey *rsa.PrivateKey
	// NameIDFormats is the allowlist of accepted NameID formats. Nil selects
	// [persistent]. Only persistent and unspecified may be configured:
	// transient is unstable by definition and an email address must never
	// become a subject.
	NameIDFormats []string
	// AllowedSignatureAlgorithms is the assertion signature algorithm
	// allowlist. Nil selects every RSA and ECDSA SHA-2 algorithm. SHA-1 is
	// rejected even if named explicitly.
	AllowedSignatureAlgorithms []string
	// ClaimAttributes maps local claim names to SAML Attribute Name values,
	// for example {"email": "mail"}. Every configured attribute must be
	// present in the assertion; a missing attribute fails the login instead of
	// being guessed.
	ClaimAttributes map[string]string
	// Leeway is the clock skew applied to NotBefore, NotOnOrAfter, and
	// SubjectConfirmationData. Zero selects 30 seconds; values above 60
	// seconds are rejected, so the acceptance window can never be widened
	// without bound.
	Leeway time.Duration
	// ReplayCache records assertion IDs atomically until they expire. Nil
	// selects a single-replica in-memory cache; a multi-replica deployment
	// must supply a shared implementation such as Redis.
	ReplayCache ReplayCache
	// Policy governs the explicit LoadMetadata fetch. The zero Policy selects
	// the production defaults. AllowHTTP and AllowPrivate are development
	// switches and must stay disabled in production.
	Policy outbound.Policy
	// Now is the clock used for condition checks, AuthTime, and the replay
	// cache. Nil selects time.Now.
	Now func() time.Time
}

// Validate reports whether the configuration satisfies every static rule New
// enforces. It performs no network I/O and never renders the private key.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// String renders the configuration for diagnostics with the private key
// redacted. Non-secret configuration is deliberately kept: an operator needs
// the URLs and entity IDs to diagnose a deployment.
func (c Config) String() string {
	return fmt.Sprintf("saml.Config{Name:%q Issuer:%q SSOURL:%q EntityID:%q ACSURL:%q IdPCertificates:%d SPCertificate:%t PrivateKey:%s NameIDFormats:%v AllowedSignatureAlgorithms:%v ClaimAttributes:%v Leeway:%s AllowHTTP:%t AllowPrivate:%t}",
		c.Name, c.Issuer, c.SSOURL, c.EntityID, c.ACSURL, len(c.IdPCertificates),
		c.SPCertificate != nil, redactedKey(c.SPPrivateKey), c.NameIDFormats,
		c.AllowedSignatureAlgorithms, c.ClaimAttributes, c.Leeway,
		c.Policy.AllowHTTP, c.Policy.AllowPrivate)
}

// Format implements fmt.Formatter so every verb renders the redacted string,
// including %#v, which would otherwise print the raw struct value and its
// private key.
func (c Config) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

// LogValue implements slog.LogValuer with the private key redacted.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// redactedKey returns the only rendering a private key may produce.
func redactedKey(key *rsa.PrivateKey) string {
	if key == nil {
		return "<none>"
	}
	return "[REDACTED]"
}

// settings is the immutable, normalized form of a Config. It is the only
// representation the Provider carries, so a later mutation of the caller's
// Config cannot change a live provider.
type settings struct {
	name            string
	issuer          string
	ssoURL          string
	entityID        string
	acsURL          string
	idpCertificates []*x509.Certificate
	spCertificate   *x509.Certificate
	spPrivateKey    *rsa.PrivateKey
	nameIDFormats   []string
	signatureAlgs   []string
	signatureAlgSet map[string]struct{}
	claimAttributes map[string]string
	leeway          time.Duration
	replay          ReplayCache
	policy          outbound.Policy
	now             func() time.Time
}

// normalizeConfig validates config and freezes it into settings. Every error
// wraps ErrInvalidConfig and carries at most a field name and a static reason.
func normalizeConfig(config Config) (settings, error) {
	fail := func(format string, arguments ...any) (settings, error) {
		return settings{}, fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, arguments...))
	}

	if !validProviderName(config.Name) {
		return fail("Name is not a valid provider name")
	}
	issuer := strings.TrimSpace(config.Issuer)
	if !validIdentifier(issuer, maxIssuerLength) {
		return fail("Issuer must be a non-empty entityID without whitespace")
	}
	ssoURL, err := parseEndpoint(config.SSOURL, config.Policy.AllowHTTP, false)
	if err != nil {
		return fail("SSOURL %v", err)
	}
	acsURL, err := parseEndpoint(config.ACSURL, config.Policy.AllowHTTP, true)
	if err != nil {
		return fail("ACSURL %v", err)
	}
	entityID := strings.TrimSpace(config.EntityID)
	if !validIdentifier(entityID, maxEntityIDBytes) {
		return fail("EntityID must be a non-empty identifier without whitespace")
	}

	if len(config.IdPCertificates) == 0 {
		return fail("IdPCertificates must contain at least one signing certificate")
	}
	idpCertificates := make([]*x509.Certificate, len(config.IdPCertificates))
	for index, certificate := range config.IdPCertificates {
		if certificate == nil {
			return fail("IdPCertificates contains a nil certificate")
		}
		idpCertificates[index] = certificate
	}

	var spCertificate *x509.Certificate
	if config.SPCertificate != nil {
		spCertificate = config.SPCertificate
	}
	if config.SPPrivateKey != nil {
		if spCertificate == nil {
			return fail("SPPrivateKey requires SPCertificate")
		}
		publicKey, ok := spCertificate.PublicKey.(*rsa.PublicKey)
		if !ok || !publicKey.Equal(&config.SPPrivateKey.PublicKey) {
			return fail("SPPrivateKey does not match SPCertificate")
		}
	}

	nameIDFormats, err := normalizeNameIDFormats(config.NameIDFormats)
	if err != nil {
		return fail("%v", err)
	}
	signatureAlgs, signatureAlgSet, err := normalizeSignatureAlgorithms(config.AllowedSignatureAlgorithms)
	if err != nil {
		return fail("%v", err)
	}
	claimAttributes, err := normalizeClaimAttributes(config.ClaimAttributes)
	if err != nil {
		return fail("%v", err)
	}

	leeway := config.Leeway
	if leeway == 0 {
		leeway = defaultLeeway
	}
	if leeway < 0 {
		return fail("Leeway must not be negative")
	}
	if leeway > maxLeeway {
		return fail("Leeway must not exceed %s", maxLeeway)
	}

	// NewClient performs no network I/O; using it here applies the shared
	// policy's validity rules at construction time instead of at first fetch.
	if _, err := outbound.NewClient(config.Policy); err != nil {
		return fail("Policy is invalid")
	}

	now := config.Now
	if now == nil {
		now = time.Now
	}
	replay := config.ReplayCache
	if replay == nil {
		replay = newMemoryReplayCache(now)
	}

	return settings{
		name:            config.Name,
		issuer:          issuer,
		ssoURL:          ssoURL,
		entityID:        entityID,
		acsURL:          acsURL,
		idpCertificates: idpCertificates,
		spCertificate:   spCertificate,
		spPrivateKey:    config.SPPrivateKey,
		nameIDFormats:   nameIDFormats,
		signatureAlgs:   signatureAlgs,
		signatureAlgSet: signatureAlgSet,
		claimAttributes: claimAttributes,
		leeway:          leeway,
		replay:          replay,
		policy:          config.Policy,
		now:             now,
	}, nil
}

// parseEndpoint validates an absolute http(s) URL with a host, no userinfo,
// and no fragment. Plain HTTP requires the explicit development switch. The
// returned string is the normalized URL the provider stores and compares
// against.
func parseEndpoint(raw string, allowHTTP, allowQuery bool) (string, error) {
	if !validURLText(raw) {
		return "", fmt.Errorf("must be a non-empty URL without whitespace")
	}
	target, err := url.Parse(raw)
	if err != nil || !target.IsAbs() || target.Host == "" || target.Opaque != "" {
		return "", fmt.Errorf("must be an absolute URL with a host")
	}
	if target.User != nil {
		return "", fmt.Errorf("must not carry userinfo")
	}
	if target.Fragment != "" {
		return "", fmt.Errorf("must not carry a fragment")
	}
	if !allowQuery && target.RawQuery != "" {
		return "", fmt.Errorf("must not carry a query")
	}
	switch strings.ToLower(target.Scheme) {
	case "https":
	case "http":
		if !allowHTTP {
			return "", fmt.Errorf("plain HTTP requires the AllowHTTP development switch")
		}
	default:
		return "", fmt.Errorf("scheme must be http or https")
	}
	return target.String(), nil
}

// normalizeNameIDFormats validates the NameID format allowlist. Transient is
// rejected because a transient identifier cannot be a stable subject, and the
// email format is rejected because a subject must never be an email address.
func normalizeNameIDFormats(formats []string) ([]string, error) {
	if len(formats) == 0 {
		return []string{NameIDFormatPersistent}, nil
	}
	normalized := make([]string, 0, len(formats))
	for _, format := range formats {
		if _, ok := allowedNameIDFormats[format]; !ok {
			return nil, fmt.Errorf("NameID format %q is not allowed", format)
		}
		if !slices.Contains(normalized, format) {
			normalized = append(normalized, format)
		}
	}
	return normalized, nil
}

// normalizeSignatureAlgorithms validates the signature algorithm allowlist.
// SHA-1 is not in the accepted set even when named explicitly.
func normalizeSignatureAlgorithms(algorithms []string) ([]string, map[string]struct{}, error) {
	if len(algorithms) == 0 {
		return append([]string(nil), allowedSignatureAlgorithms...), setOf(allowedSignatureAlgorithms), nil
	}
	normalized := make([]string, 0, len(algorithms))
	set := make(map[string]struct{}, len(algorithms))
	for _, algorithm := range algorithms {
		if !slices.Contains(allowedSignatureAlgorithms, algorithm) {
			return nil, nil, fmt.Errorf("signature algorithm %q is not allowed", algorithm)
		}
		if _, exists := set[algorithm]; exists {
			continue
		}
		set[algorithm] = struct{}{}
		normalized = append(normalized, algorithm)
	}
	return normalized, set, nil
}

// setOf builds a membership set from a slice.
func setOf(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

// normalizeClaimAttributes copies and validates the claim mapping. Claim names
// and SAML attribute names are both checked; the caller's map is never
// retained by reference.
func normalizeClaimAttributes(claims map[string]string) (map[string]string, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	if len(claims) > maxAttributes {
		return nil, fmt.Errorf("claim mapping has more than %d entries", maxAttributes)
	}
	normalized := make(map[string]string, len(claims))
	for claim, attribute := range claims {
		if !validClaimName(claim) {
			return nil, fmt.Errorf("claim name %q is not valid", claim)
		}
		attribute = strings.TrimSpace(attribute)
		if !validAttributeName(attribute) {
			return nil, fmt.Errorf("claim %q names an invalid attribute", claim)
		}
		normalized[claim] = attribute
	}
	return normalized, nil
}

// validClaimName accepts the characters a session attribute key may carry.
func validClaimName(name string) bool {
	if name == "" || len(name) > maxClaimNameLen {
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

// validAttributeName accepts a SAML Attribute Name: a bounded token without
// whitespace or control characters, such as mail or an OID URN.
func validAttributeName(name string) bool {
	if name == "" || len(name) > maxAttributeName {
		return false
	}
	for _, r := range name {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return utf8.ValidString(name)
}

// validProviderName enforces the provider-name rule shared by every protocol
// module. The federation registry applies the same rule before its validator
// runs; this copy keeps the static error local to this module.
func validProviderName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
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
