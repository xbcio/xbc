package saml

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/beevik/etree"
)

// AuthnRequest is one generated authentication request: the ID the login
// transaction must remember and the URL the browser is sent to.
type AuthnRequest struct {
	// ID is the AuthnRequest ID. The web adapter stores it in the one-time
	// login transaction and passes it back as ResponseInput.ExpectedRequestID.
	ID string
	// URL is the complete HTTP-Redirect binding URL at the IdP.
	URL string
}

// NewAuthnRequest builds an AuthnRequest, deflates and base64-encodes it for
// the HTTP-Redirect binding, and returns the redirect URL. When SPPrivateKey is
// configured the query string is signed with RSA-SHA256 and SigAlg/Signature
// parameters are appended; the AuthnRequest XML itself stays unsigned, as the
// redirect binding requires.
//
// relayState is opaque application state bound to the login transaction. It is
// optional, bounded, and rejected if it could break the query string.
func (p *Provider) NewAuthnRequest(relayState string) (AuthnRequest, error) {
	if err := p.usable(); err != nil {
		return AuthnRequest{}, err
	}
	if err := validateRelayState(relayState); err != nil {
		return AuthnRequest{}, err
	}
	id, err := newAuthnRequestID()
	if err != nil {
		return AuthnRequest{}, err
	}
	document := etree.NewDocument()
	document.CreateProcInst("xml", `version="1.0" encoding="UTF-8"`)
	request := document.CreateElement("samlp:AuthnRequest")
	request.CreateAttr("xmlns:samlp", protocolNamespace)
	request.CreateAttr("xmlns:saml", assertionNamespace)
	request.CreateAttr("ID", id)
	request.CreateAttr("Version", samlVersion)
	request.CreateAttr("IssueInstant", p.now().UTC().Format(time.RFC3339))
	request.CreateAttr("Destination", p.ssoURL)
	request.CreateAttr("ProtocolBinding", bindingHTTPPost)
	request.CreateAttr("AssertionConsumerServiceURL", p.acsURL)
	issuer := request.CreateElement("saml:Issuer")
	issuer.SetText(p.entityID)
	policy := request.CreateElement("samlp:NameIDPolicy")
	policy.CreateAttr("Format", p.nameIDFormats[0])
	policy.CreateAttr("AllowCreate", "true")

	xmlBytes, err := document.WriteToBytes()
	if err != nil {
		return AuthnRequest{}, fmt.Errorf("%w: could not serialize the request", ErrUnavailable)
	}
	encoded, err := deflateAndEncode(xmlBytes)
	if err != nil {
		return AuthnRequest{}, err
	}
	redirectURL, err := p.redirectURL(encoded, relayState)
	if err != nil {
		return AuthnRequest{}, err
	}
	return AuthnRequest{ID: id, URL: redirectURL}, nil
}

// deflateAndEncode applies the HTTP-Redirect binding encoding: raw DEFLATE
// followed by standard base64.
func deflateAndEncode(document []byte) (string, error) {
	var buffer bytes.Buffer
	writer, err := flate.NewWriter(&buffer, flate.DefaultCompression)
	if err != nil {
		return "", fmt.Errorf("%w: could not compress the request", ErrUnavailable)
	}
	if _, err := writer.Write(document); err != nil {
		return "", fmt.Errorf("%w: could not compress the request", ErrUnavailable)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("%w: could not compress the request", ErrUnavailable)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes()), nil
}

// redirectURL assembles the final URL. Parameters are emitted in the order the
// SAML HTTP-Redirect binding signs: SAMLRequest, RelayState, SigAlg, then the
// Signature appended after the signed octets.
func (p *Provider) redirectURL(encoded, relayState string) (string, error) {
	parts := []string{"SAMLRequest=" + url.QueryEscape(encoded)}
	if relayState != "" {
		parts = append(parts, "RelayState="+url.QueryEscape(relayState))
	}
	signed := strings.Join(parts, "&")
	if p.spPrivateKey != nil {
		signed += "&SigAlg=" + url.QueryEscape(rsaSHA256RedirectSigAlg)
		digest := sha256.Sum256([]byte(signed))
		signature, err := rsa.SignPKCS1v15(rand.Reader, p.spPrivateKey, crypto.SHA256, digest[:])
		if err != nil {
			return "", fmt.Errorf("%w: could not sign the request", ErrUnavailable)
		}
		signed += "&Signature=" + url.QueryEscape(base64.StdEncoding.EncodeToString(signature))
	}
	return p.ssoURL + "?" + signed, nil
}

// newAuthnRequestID returns a 128-bit random ID in the XML NCName shape a
// signature reference can name.
func newAuthnRequestID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("%w: random source is unavailable", ErrUnavailable)
	}
	return "_" + hex.EncodeToString(random), nil
}

// validateRelayState bounds the opaque state the adapter round-trips.
func validateRelayState(relayState string) error {
	if relayState == "" {
		return nil
	}
	if len(relayState) > maxRelayState || !utf8.ValidString(relayState) {
		return fmt.Errorf("%w: relay state is too long or not valid UTF-8", ErrInvalidConfig)
	}
	for _, r := range relayState {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: relay state contains a control character", ErrInvalidConfig)
		}
	}
	return nil
}
