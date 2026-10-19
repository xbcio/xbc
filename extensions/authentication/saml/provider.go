package saml

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation"
)

// protocolName is the value recorded in ExternalIdentity.Protocol.
const protocolName = "saml"

// Provider is one immutable SAML service-provider configuration together with
// the validation policy derived from it. Verify, NewAuthnRequest, and Metadata
// are safe for concurrent use.
//
// A Provider performs no network I/O at construction: New only validates and
// freezes the configuration. IdP metadata is fetched by the explicit
// LoadMetadata call, and a refresh is that call followed by Registry.Put of a
// new provider.
//
// String, Format, and LogValue render the provider with the SP private key
// redacted, including for a dereferenced Provider value and for structured log
// records.
type Provider struct {
	settings
	initialized bool
}

// New validates config without network I/O and returns a provider ready for
// Verify and NewAuthnRequest.
func New(config Config) (*Provider, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return &Provider{settings: normalized, initialized: true}, nil
}

// Name returns the registered provider name, or the empty string for a nil
// provider.
func (p *Provider) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// String renders the provider for diagnostics with the private key redacted.
func (p *Provider) String() string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("saml.Provider{Name:%q Issuer:%q SSOURL:%q EntityID:%q ACSURL:%q IdPCertificates:%d SPCertificate:%t PrivateKey:%s NameIDFormats:%v AllowedSignatureAlgorithms:%v Leeway:%s}",
		p.name, p.issuer, p.ssoURL, p.entityID, p.acsURL, len(p.idpCertificates),
		p.spCertificate != nil, redactedKey(p.spPrivateKey), p.nameIDFormats,
		p.signatureAlgs, p.leeway)
}

// Format implements fmt.Formatter so every verb renders the redacted string,
// including %#v, which would otherwise print the unexported settings fields.
//
// The receiver is a value, not a pointer. fmt dispatches methods on a struct's
// fields only when they are exported, so a dereferenced Provider value would
// otherwise render the unexported private key field verbatim. A value receiver
// makes the Provider value itself satisfy fmt.Formatter. Formatting a nil
// *Provider still prints "<nil>": fmt converts a nil-pointer panic in a
// formatting method to that string.
func (p Provider) Format(state fmt.State, _ rune) {
	// p is addressable, so the pointer-receiver String is reachable through the
	// value. Calling it as p.String() rather than (&p).String() keeps a
	// Format-receiver mutation compilable, so it is killed by behavior.
	_, _ = io.WriteString(state, p.String())
}

// LogValue implements slog.LogValuer with the private key redacted.
func (p *Provider) LogValue() slog.Value { return slog.StringValue(p.String()) }

// usable reports whether the provider was constructed by New.
func (p *Provider) usable() error {
	if p == nil {
		return ErrNilProvider
	}
	if !p.initialized {
		return fmt.Errorf("%w: provider was not constructed by New", ErrInvalidConfig)
	}
	return nil
}

// ResponseInput is one HTTP-POST response as received by the assertion
// consumer service, together with the values the login transaction binds it
// to.
type ResponseInput struct {
	// ResponseXML is the decoded SAMLResponse document. The web adapter
	// base64-decodes the form value; this package accepts the XML bytes.
	ResponseXML []byte
	// ExpectedRequestID is the ID of the AuthnRequest this callback answers.
	// It must equal the response InResponseTo and the bearer
	// SubjectConfirmationData InResponseTo.
	ExpectedRequestID string
	// ACSURL must equal the configured ACS URL. It is the value the request
	// was actually delivered to, and the response Destination and the bearer
	// Recipient must both name it.
	ACSURL string
}

// Verify fully validates one SAML response and returns the verified external
// identity.
//
// The flow fails closed: XML parser protections run first, then the response
// structure is checked, then the single signature is verified against the
// element its signed reference names, then every condition and the replay
// cache are checked. Nothing is consumed until the signature is valid, and no
// login succeeds if the replay cache is unavailable.
//
// Every response failure wraps ErrInvalidResponse, ErrReplay, or ErrUnavailable,
// so errors.Is cannot distinguish a malformed response, a bad signature, an
// expired assertion, or an unknown subject. The wrapped reason exists for
// diagnostics; a caller must not expose it to end users. Configuration and
// programming errors surface separately as ErrNilContext, ErrNilProvider, or
// ErrInvalidConfig. No error text ever carries assertion content, a NameID, an
// attribute value, a certificate, or a key.
func Verify(ctx context.Context, provider *Provider, input ResponseInput) (federation.ExternalIdentity, error) {
	if ctx == nil {
		return federation.ExternalIdentity{}, ErrNilContext
	}
	if err := provider.usable(); err != nil {
		return federation.ExternalIdentity{}, err
	}
	now := provider.now()
	assertion, expires, err := verifyResponse(provider, input, now)
	if err != nil {
		return federation.ExternalIdentity{}, err
	}
	if err := provider.replay.Claim(ctx, assertion.assertionID, expires); err != nil {
		if errors.Is(err, ErrReplay) {
			return federation.ExternalIdentity{}, ErrReplay
		}
		return federation.ExternalIdentity{}, fmt.Errorf("%w: replay cache is unavailable", ErrUnavailable)
	}
	return federation.ExternalIdentity{
		Protocol: protocolName,
		Provider: provider.name,
		Issuer:   assertion.issuer,
		Subject:  assertion.subject,
		Claims:   assertion.claims,
		AuthTime: now,
	}, nil
}

// verifiedAssertion is the part of a validated response that becomes an
// ExternalIdentity.
type verifiedAssertion struct {
	assertionID string
	issuer      string
	subject     string
	claims      map[string]any
}

// parseSAMLTime parses a SAML timestamp. RFC 3339 parsing accepts the
// fractional seconds SAML permits, and the result is normalized to UTC.
func parseSAMLTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: malformed timestamp", ErrInvalidResponse)
	}
	return parsed.UTC(), nil
}
