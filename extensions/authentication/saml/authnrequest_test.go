package saml

import (
	"bytes"
	"compress/flate"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/beevik/etree"
)

func TestNewAuthnRequestProducesRedirectBinding(t *testing.T) {
	provider := newTestProvider(t, nil)
	request, err := provider.NewAuthnRequest("state-token")
	if err != nil {
		t.Fatalf("NewAuthnRequest: %v", err)
	}
	if !strings.HasPrefix(request.ID, "_") || !xmlIDPattern.MatchString(request.ID) {
		t.Fatalf("request ID %q is not an XML NCName", request.ID)
	}
	if len(request.ID) != 1+32 {
		t.Fatalf("request ID %q does not carry 128 bits", request.ID)
	}
	if !strings.HasPrefix(request.URL, provider.ssoURL+"?SAMLRequest=") {
		t.Fatalf("URL does not start with the SSO endpoint and SAMLRequest: %s", request.URL)
	}
	if strings.Contains(request.URL, "SigAlg") || strings.Contains(request.URL, "Signature=") {
		t.Fatalf("unsigned provider emitted signature parameters: %s", request.URL)
	}

	parameters, err := url.ParseQuery(request.URL[strings.Index(request.URL, "?")+1:])
	if err != nil {
		t.Fatalf("parse redirect query: %v", err)
	}
	if parameters.Get("RelayState") != "state-token" {
		t.Fatalf("RelayState = %q, want state-token", parameters.Get("RelayState"))
	}

	document := decodeRedirectRequest(t, parameters.Get("SAMLRequest"))
	root := document.Root()
	if root.Tag != "AuthnRequest" || root.NamespaceURI() != protocolNamespace {
		t.Fatalf("request root = {%s}%s", root.NamespaceURI(), root.Tag)
	}
	if id := root.SelectAttrValue("ID", ""); id != request.ID {
		t.Fatalf("request ID = %q, want %q", id, request.ID)
	}
	if destination := root.SelectAttrValue("Destination", ""); destination != provider.ssoURL {
		t.Fatalf("Destination = %q, want %q", destination, provider.ssoURL)
	}
	if binding := root.SelectAttrValue("ProtocolBinding", ""); binding != bindingHTTPPost {
		t.Fatalf("ProtocolBinding = %q, want %q", binding, bindingHTTPPost)
	}
	if acs := root.SelectAttrValue("AssertionConsumerServiceURL", ""); acs != provider.acsURL {
		t.Fatalf("AssertionConsumerServiceURL = %q, want %q", acs, provider.acsURL)
	}
	if instant := root.SelectAttrValue("IssueInstant", ""); instant != testNow.Format("2006-01-02T15:04:05Z07:00") {
		t.Fatalf("IssueInstant = %q, want %q", instant, testNow.Format("2006-01-02T15:04:05Z07:00"))
	}
	issuer := findTestElement(t, root, assertionNamespace, "Issuer")
	if issuer.Text() != provider.entityID {
		t.Fatalf("Issuer = %q, want %q", issuer.Text(), provider.entityID)
	}
	policy := findTestElement(t, root, protocolNamespace, "NameIDPolicy")
	if policy.SelectAttrValue("Format", "") != NameIDFormatPersistent {
		t.Fatalf("NameIDPolicy Format = %q", policy.SelectAttrValue("Format", ""))
	}
	if policy.SelectAttrValue("AllowCreate", "") != "true" {
		t.Fatalf("NameIDPolicy AllowCreate = %q", policy.SelectAttrValue("AllowCreate", ""))
	}
}

func TestNewAuthnRequestIDsAreUnique(t *testing.T) {
	provider := newTestProvider(t, nil)
	seen := make(map[string]struct{}, 32)
	for index := 0; index < 32; index++ {
		request, err := provider.NewAuthnRequest("")
		if err != nil {
			t.Fatalf("NewAuthnRequest: %v", err)
		}
		if _, duplicate := seen[request.ID]; duplicate {
			t.Fatalf("duplicate request ID %q", request.ID)
		}
		seen[request.ID] = struct{}{}
	}
}

func TestNewAuthnRequestSignsRedirectBinding(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.SPCertificate = spCertificate
		config.SPPrivateKey = spKey
	})
	request, err := provider.NewAuthnRequest("opaque")
	if err != nil {
		t.Fatalf("NewAuthnRequest: %v", err)
	}
	raw := request.URL
	signatureIndex := strings.Index(raw, "&Signature=")
	if signatureIndex < 0 {
		t.Fatalf("URL carries no signature: %s", raw)
	}
	signed := raw[strings.Index(raw, "?")+1 : signatureIndex]
	if !strings.Contains(signed, "SigAlg=") {
		t.Fatalf("signed octets carry no SigAlg: %s", signed)
	}
	parameters, err := url.ParseQuery(raw[strings.Index(raw, "?")+1:])
	if err != nil {
		t.Fatalf("parse redirect query: %v", err)
	}
	// Pin the exact signed octets, not merely their self-consistency: the
	// HTTP-Redirect binding signs SAMLRequest, then RelayState, then SigAlg,
	// with query escaping applied to each value.
	wantSigned := "SAMLRequest=" + url.QueryEscape(parameters.Get("SAMLRequest")) +
		"&RelayState=opaque&SigAlg=" + url.QueryEscape(AlgorithmRSASHA256)
	if signed != wantSigned {
		t.Fatalf("signed octets = %q, want %q", signed, wantSigned)
	}
	if parameters.Get("RelayState") != "opaque" {
		t.Fatalf("RelayState = %q, want opaque", parameters.Get("RelayState"))
	}
	if parameters.Get("SigAlg") != AlgorithmRSASHA256 {
		t.Fatalf("SigAlg = %q, want %q", parameters.Get("SigAlg"), AlgorithmRSASHA256)
	}
	signature, err := base64.StdEncoding.DecodeString(parameters.Get("Signature"))
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	digest := sha256.Sum256([]byte(signed))
	if err := rsa.VerifyPKCS1v15(&spKey.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("redirect signature does not verify: %v", err)
	}
}

func TestNewAuthnRequestRejectsUnsafeRelayState(t *testing.T) {
	provider := newTestProvider(t, nil)
	cases := map[string]string{
		"control character": "state\x00token",
		"newline":           "state\ntoken",
		"too long":          strings.Repeat("a", maxRelayState+1),
		"invalid utf8":      string([]byte{0xff, 0xfe}),
	}
	for name, relayState := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := provider.NewAuthnRequest(relayState); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewAuthnRequest = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestNewAuthnRequestRejectsNilProvider(t *testing.T) {
	var provider *Provider
	if _, err := provider.NewAuthnRequest(""); !errors.Is(err, ErrNilProvider) {
		t.Fatalf("NewAuthnRequest(nil provider) = %v, want ErrNilProvider", err)
	}
}

func TestNewAuthnRequestUsesConfiguredNameIDFormat(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.NameIDFormats = []string{NameIDFormatUnspecified, NameIDFormatPersistent}
	})
	request, err := provider.NewAuthnRequest("")
	if err != nil {
		t.Fatalf("NewAuthnRequest: %v", err)
	}
	parameters, err := url.ParseQuery(request.URL[strings.Index(request.URL, "?")+1:])
	if err != nil {
		t.Fatalf("parse redirect query: %v", err)
	}
	document := decodeRedirectRequest(t, parameters.Get("SAMLRequest"))
	policy := findTestElement(t, document.Root(), protocolNamespace, "NameIDPolicy")
	if policy.SelectAttrValue("Format", "") != NameIDFormatUnspecified {
		t.Fatalf("NameIDPolicy Format = %q, want %q", policy.SelectAttrValue("Format", ""), NameIDFormatUnspecified)
	}
}

// decodeRedirectRequest reverses the HTTP-Redirect encoding: standard base64
// then raw DEFLATE.
func decodeRedirectRequest(t *testing.T, encoded string) *etree.Document {
	t.Helper()
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode SAMLRequest: %v", err)
	}
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer func() { _ = reader.Close() }()
	document, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("inflate SAMLRequest: %v", err)
	}
	parsed := etree.NewDocument()
	if err := parsed.ReadFromBytes(document); err != nil {
		t.Fatalf("parse request XML: %v", err)
	}
	return parsed
}
