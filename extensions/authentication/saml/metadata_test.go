package saml

import (
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// testPolicy admits the local metadata server.
func testPolicy() outbound.Policy {
	return outbound.Policy{AllowHTTP: true, AllowPrivate: true}
}

// metadataXML renders an IdP metadata document with the given entity ID, SSO
// URL, and signing certificates.
func metadataXML(entityID, ssoURL string, certificates ...[]byte) string {
	var builder strings.Builder
	builder.WriteString(`<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="` + entityID + `">`)
	builder.WriteString(`<md:IDPSSODescriptor protocolSupportEnumeration="` + protocolNamespace + `">`)
	for _, certificate := range certificates {
		builder.WriteString(`<md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>`)
		builder.WriteString(base64.StdEncoding.EncodeToString(certificate))
		builder.WriteString(`</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>`)
	}
	builder.WriteString(`<md:NameIDFormat>` + NameIDFormatPersistent + `</md:NameIDFormat>`)
	builder.WriteString(`<md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="` + ssoURL + `"/>`)
	builder.WriteString(`</md:IDPSSODescriptor></md:EntityDescriptor>`)
	return builder.String()
}

// metadataServer serves one metadata document.
func metadataServer(t *testing.T, document string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = writer.Write([]byte(document))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLoadMetadataParsesIdPDescriptor(t *testing.T) {
	server := metadataServer(t, metadataXML("https://idp.example.com/entity", "https://idp.example.com/sso", idpCertificate.Raw))
	metadata, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy())
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if metadata.EntityID != "https://idp.example.com/entity" {
		t.Fatalf("EntityID = %q", metadata.EntityID)
	}
	if metadata.SSOURL != "https://idp.example.com/sso" {
		t.Fatalf("SSOURL = %q", metadata.SSOURL)
	}
	if len(metadata.Certificates) != 1 || !metadata.Certificates[0].Equal(idpCertificate) {
		t.Fatalf("Certificates = %d, want the IdP certificate", len(metadata.Certificates))
	}
	if len(metadata.NameIDFormats) != 1 || metadata.NameIDFormats[0] != NameIDFormatPersistent {
		t.Fatalf("NameIDFormats = %v", metadata.NameIDFormats)
	}
}

func TestLoadMetadataParsesEntitiesDescriptor(t *testing.T) {
	document := `<md:EntitiesDescriptor xmlns:md="` + metadataNamespace + `">` +
		metadataXML("https://idp.example.com/entity", "https://idp.example.com/sso", idpCertificate.Raw) +
		`</md:EntitiesDescriptor>`
	// The inner EntityDescriptor repeats the md namespace declaration, which is
	// valid XML and must not confuse the parser.
	server := metadataServer(t, document)
	metadata, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy())
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if metadata.EntityID != "https://idp.example.com/entity" {
		t.Fatalf("EntityID = %q", metadata.EntityID)
	}
}

func TestLoadMetadataSelectsSigningCertificates(t *testing.T) {
	document := `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp.example.com/entity">` +
		`<md:IDPSSODescriptor protocolSupportEnumeration="` + protocolNamespace + `">` +
		`<md:KeyDescriptor use="encryption"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` +
		base64.StdEncoding.EncodeToString(untrustedCertificate.Raw) +
		`</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>` +
		`<md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` +
		base64.StdEncoding.EncodeToString(idpCertificate.Raw) +
		`</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>` +
		`<md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` +
		base64.StdEncoding.EncodeToString(idpCertificate.Raw) +
		`</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>` +
		`<md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="https://idp.example.com/sso"/>` +
		`</md:IDPSSODescriptor></md:EntityDescriptor>`
	server := metadataServer(t, document)
	metadata, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy())
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if len(metadata.Certificates) != 1 || !metadata.Certificates[0].Equal(idpCertificate) {
		t.Fatalf("Certificates = %d, want one deduplicated IdP certificate", len(metadata.Certificates))
	}
}

func TestLoadMetadataRejectsInvalidDocuments(t *testing.T) {
	cases := map[string]string{
		"malformed":               `<not-xml`,
		"doctype":                 `<!DOCTYPE foo><foo/>`,
		"wrong root":              `<foo/>`,
		"no certificates":         metadataXML("https://idp.example.com/entity", "https://idp.example.com/sso"),
		"sso location with query": metadataXML("https://idp.example.com/entity", "https://idp.example.com/sso?tenant=one", idpCertificate.Raw),
		"no redirect endpoint":    `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp.example.com/entity"><md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + base64.StdEncoding.EncodeToString(idpCertificate.Raw) + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor></md:IDPSSODescriptor></md:EntityDescriptor>`,
		"two redirect endpoints":  `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp.example.com/entity"><md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + base64.StdEncoding.EncodeToString(idpCertificate.Raw) + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="https://idp.example.com/sso"/><md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="https://idp.example.com/sso2"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
		"bad certificate":         metadataXML("https://idp.example.com/entity", "https://idp.example.com/sso", []byte("not-a-certificate")),
		"two entity descriptors":  `<md:EntitiesDescriptor xmlns:md="` + metadataNamespace + `">` + metadataXML("https://a.example.com", "https://idp.example.com/sso", idpCertificate.Raw) + metadataXML("https://b.example.com", "https://idp.example.com/sso", idpCertificate.Raw) + `</md:EntitiesDescriptor>`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			server := metadataServer(t, document)
			if _, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy()); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("LoadMetadata = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadMetadataRejectsAmbiguousDescriptors(t *testing.T) {
	certificate := base64.StdEncoding.EncodeToString(idpCertificate.Raw)
	cases := map[string]string{
		"missing idp descriptor":             `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" entityID="https://idp.example.com/entity"/>`,
		"malformed entity id":                `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp example.com"><md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + certificate + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="https://idp.example.com/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
		"relative sso location":              `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp.example.com/entity"><md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + certificate + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
		"two certificates in one descriptor": `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp.example.com/entity"><md:IDPSSODescriptor><md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + certificate + `</ds:X509Certificate><ds:X509Certificate>` + certificate + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="https://idp.example.com/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
		"nameid format with nested xml":      `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp.example.com/entity"><md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` + certificate + `</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:NameIDFormat><md:Nested/></md:NameIDFormat><md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="https://idp.example.com/sso"/></md:IDPSSODescriptor></md:EntityDescriptor>`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			server := metadataServer(t, document)
			if _, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy()); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("LoadMetadata = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadMetadataAcceptsKeyDescriptorWithoutUse(t *testing.T) {
	document := `<md:EntityDescriptor xmlns:md="` + metadataNamespace + `" xmlns:ds="` + dsigNamespace + `" entityID="https://idp.example.com/entity">` +
		`<md:IDPSSODescriptor><md:KeyDescriptor><ds:KeyInfo><ds:X509Data><ds:X509Certificate>` +
		base64.StdEncoding.EncodeToString(idpCertificate.Raw) +
		`</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>` +
		`<md:SingleSignOnService Binding="` + bindingHTTPRedirect + `" Location="https://idp.example.com/sso"/>` +
		`</md:IDPSSODescriptor></md:EntityDescriptor>`
	server := metadataServer(t, document)
	metadata, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy())
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if len(metadata.Certificates) != 1 || !metadata.Certificates[0].Equal(idpCertificate) {
		t.Fatal("a key descriptor without use was not treated as a signing certificate")
	}
}

func TestLoadMetadataReportsTransportFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	if _, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("LoadMetadata = %v, want ErrUnavailable", err)
	}
}

func TestLoadMetadataRejectsOversizedBody(t *testing.T) {
	server := metadataServer(t, strings.Repeat("a", maxMetadataBytes+1))
	if _, err := LoadMetadata(t.Context(), server.URL+"/metadata", testPolicy()); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("LoadMetadata = %v, want ErrInvalidResponse", err)
	}
}

func TestLoadMetadataRejectsInvalidConfiguration(t *testing.T) {
	if _, err := LoadMetadata(nil, "https://idp.example.com/metadata", testPolicy()); !errors.Is(err, ErrNilContext) {
		t.Fatalf("LoadMetadata(nil context) = %v, want ErrNilContext", err)
	}
	if _, err := LoadMetadata(t.Context(), "http://idp.example.com/metadata", outbound.Policy{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadMetadata(plain HTTP) = %v, want ErrInvalidConfig", err)
	}
	if _, err := LoadMetadata(t.Context(), "https://idp.example.com/metadata", outbound.Policy{DialTimeout: -time.Second}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("LoadMetadata(invalid policy) = %v, want ErrInvalidConfig", err)
	}
}

func TestProviderMetadataRendersServiceProviderDocument(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.SPCertificate = spCertificate
		config.SPPrivateKey = spKey
	})
	document, err := provider.Metadata()
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	restricted := newTestDocument(t, document)
	root := restricted.Root()
	if root.Tag != "EntityDescriptor" || root.NamespaceURI() != metadataNamespace {
		t.Fatalf("metadata root = {%s}%s", root.NamespaceURI(), root.Tag)
	}
	if root.SelectAttrValue("entityID", "") != provider.entityID {
		t.Fatalf("entityID = %q, want %q", root.SelectAttrValue("entityID", ""), provider.entityID)
	}
	descriptor := findTestElement(t, root, metadataNamespace, "SPSSODescriptor")
	if descriptor.SelectAttrValue("AuthnRequestsSigned", "") != "true" {
		t.Fatal("AuthnRequestsSigned is not true for a signing provider")
	}
	if descriptor.SelectAttrValue("WantAssertionsSigned", "") != "true" {
		t.Fatal("WantAssertionsSigned is not true")
	}
	if descriptor.SelectAttrValue("protocolSupportEnumeration", "") != protocolNamespace {
		t.Fatal("protocolSupportEnumeration is wrong")
	}
	keyDescriptor := findTestElement(t, root, metadataNamespace, "KeyDescriptor")
	if keyDescriptor.SelectAttrValue("use", "") != "signing" {
		t.Fatal("KeyDescriptor use is not signing")
	}
	certificate := findTestElement(t, root, dsigNamespace, "X509Certificate")
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(certificate.Text()))
	if err != nil {
		t.Fatalf("decode certificate: %v", err)
	}
	if string(decoded) != string(spCertificate.Raw) {
		t.Fatal("metadata does not publish the SP certificate")
	}
	formats := directChildren(descriptor, metadataNamespace, "NameIDFormat")
	if len(formats) != 1 || formats[0].Text() != NameIDFormatPersistent {
		t.Fatalf("NameIDFormat elements = %d", len(formats))
	}
	service := findTestElement(t, root, metadataNamespace, "AssertionConsumerService")
	if service.SelectAttrValue("Binding", "") != bindingHTTPPost {
		t.Fatal("AssertionConsumerService binding is not HTTP-POST")
	}
	if service.SelectAttrValue("Location", "") != provider.acsURL {
		t.Fatalf("AssertionConsumerService Location = %q", service.SelectAttrValue("Location", ""))
	}
	if service.SelectAttrValue("index", "") != "0" || service.SelectAttrValue("isDefault", "") != "true" {
		t.Fatal("AssertionConsumerService is not the default index 0")
	}

	privateKey := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(spKey))
	if strings.Contains(string(document), privateKey) || strings.Contains(string(document), "PRIVATE KEY") {
		t.Fatal("metadata contains private key material")
	}
}

func TestProviderMetadataWithoutSigningKey(t *testing.T) {
	provider := newTestProvider(t, nil)
	document, err := provider.Metadata()
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	restricted := newTestDocument(t, document)
	descriptor := findTestElement(t, restricted.Root(), metadataNamespace, "SPSSODescriptor")
	if descriptor.SelectAttrValue("AuthnRequestsSigned", "") != "false" {
		t.Fatal("AuthnRequestsSigned is not false without a key")
	}
	if len(directChildren(descriptor, metadataNamespace, "KeyDescriptor")) != 0 {
		t.Fatal("metadata publishes a key descriptor without a certificate")
	}
}

func TestProviderMetadataRejectsNilProvider(t *testing.T) {
	var provider *Provider
	if _, err := provider.Metadata(); !errors.Is(err, ErrNilProvider) {
		t.Fatalf("Metadata(nil provider) = %v, want ErrNilProvider", err)
	}
}

func TestMetadataNeverContainsErrorMessageMaterial(t *testing.T) {
	provider := newTestProvider(t, nil)
	document, err := provider.Metadata()
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if strings.Contains(string(document), "PRIVATE KEY") {
		t.Fatal("metadata contains a private key")
	}
	if strings.Contains(string(document), testNameID) {
		t.Fatal("metadata contains assertion content")
	}
}
