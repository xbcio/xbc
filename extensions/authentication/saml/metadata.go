package saml

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/beevik/etree"
	"github.com/xbcio/xbc/extensions/authentication/federation/outbound"
)

// IDPMetadata is the part of an IdP metadata document this module consumes.
// Loading metadata never mutates a live provider: an application builds a new
// Config from these values, constructs a Provider, and stores it through
// Registry.Put.
//
// Metadata signatures are not verified. Trust comes from fetching the document
// from the configured HTTPS location through the outbound policy; an
// application that requires signed metadata must verify it before building the
// configuration.
type IDPMetadata struct {
	// EntityID is the IdP entityID, which becomes Config.Issuer.
	EntityID string
	// SSOURL is the HTTP-Redirect single-sign-on endpoint, which becomes
	// Config.SSOURL.
	SSOURL string
	// Certificates are the assertion-signing certificates, which become
	// Config.IdPCertificates. During a rotation the document may list both the
	// old and the new certificate.
	Certificates []*x509.Certificate
	// NameIDFormats lists the formats the IdP advertises. It is informational;
	// Config.NameIDFormats is still an explicit application choice.
	NameIDFormats []string
}

// LoadMetadata explicitly fetches and parses an IdP metadata document through
// the shared outbound policy. It is the only network call in this package and
// is never made by New or by a registry write.
//
// A refresh is this call followed by New and Registry.Put. The loader requires
// exactly one entity descriptor, exactly one IdP descriptor, at least one
// signing certificate, and exactly one HTTP-Redirect SSO endpoint; ambiguity
// is rejected instead of resolved.
func LoadMetadata(ctx context.Context, metadataURL string, policy outbound.Policy) (IDPMetadata, error) {
	if ctx == nil {
		return IDPMetadata{}, ErrNilContext
	}
	endpoint, err := parseEndpoint(metadataURL, policy.AllowHTTP, true)
	if err != nil {
		return IDPMetadata{}, fmt.Errorf("%w: metadata URL is invalid", ErrInvalidConfig)
	}
	client, err := outbound.NewClient(policy)
	if err != nil {
		return IDPMetadata{}, fmt.Errorf("%w: outbound policy is invalid", ErrInvalidConfig)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return IDPMetadata{}, fmt.Errorf("%w: metadata request could not be built", ErrUnavailable)
	}
	request.Header.Set("Accept", "application/samlmetadata+xml, application/xml;q=0.9")
	response, err := client.Do(request)
	if err != nil {
		return IDPMetadata{}, fmt.Errorf("%w: metadata request failed", ErrUnavailable)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return IDPMetadata{}, fmt.Errorf("%w: metadata endpoint returned status %d", ErrUnavailable, response.StatusCode)
	}
	body, err := outbound.ReadBody(response, maxMetadataBytes)
	if err != nil {
		if errors.Is(err, outbound.ErrBodyTooLarge) {
			return IDPMetadata{}, fmt.Errorf("%w: metadata is above the size limit", ErrInvalidResponse)
		}
		return IDPMetadata{}, fmt.Errorf("%w: metadata body could not be read", ErrUnavailable)
	}
	root, err := parseXML(body, maxMetadataBytes)
	if err != nil {
		return IDPMetadata{}, err
	}
	return parseIDPMetadata(root, policy)
}

// parseIDPMetadata extracts the consumed fields from a metadata document.
func parseIDPMetadata(root *etree.Element, policy outbound.Policy) (IDPMetadata, error) {
	var descriptor *etree.Element
	switch {
	case root.Tag == "EntityDescriptor" && inNamespace(root, metadataNamespace):
		descriptor = root
	case root.Tag == "EntitiesDescriptor" && inNamespace(root, metadataNamespace):
		descriptors := elementsByTagDescendants(root, metadataNamespace, "EntityDescriptor")
		if len(descriptors) != 1 {
			return IDPMetadata{}, invalidResponse("metadata must contain exactly one entity descriptor")
		}
		descriptor = descriptors[0]
	default:
		return IDPMetadata{}, invalidResponse("metadata root is not an entity descriptor")
	}

	entityID, err := requiredAttribute(descriptor, "entityID")
	if err != nil || !validIdentifier(entityID, maxEntityIDBytes) {
		return IDPMetadata{}, invalidResponse("metadata entityID is missing or malformed")
	}
	idp, err := oneChild(descriptor, metadataNamespace, "IDPSSODescriptor")
	if err != nil {
		return IDPMetadata{}, invalidResponse("metadata has no IdP descriptor")
	}

	certificates := make([]*x509.Certificate, 0, 2)
	seen := make(map[string]struct{})
	for _, keyDescriptor := range childElements(idp, metadataNamespace, "KeyDescriptor") {
		if use, present := noNamespaceAttribute(keyDescriptor, "use"); present && use != "" && use != "signing" {
			continue
		}
		certificateElements := elementsByTagDescendants(keyDescriptor, dsigNamespace, "X509Certificate")
		if len(certificateElements) != 1 {
			return IDPMetadata{}, invalidResponse("metadata key descriptor is ambiguous")
		}
		encoded := strings.Join(strings.Fields(certificateElements[0].Text()), "")
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return IDPMetadata{}, invalidResponse("metadata certificate is not valid base64")
		}
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			return IDPMetadata{}, invalidResponse("metadata certificate is not a valid certificate")
		}
		if _, duplicate := seen[string(certificate.Raw)]; duplicate {
			continue
		}
		seen[string(certificate.Raw)] = struct{}{}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return IDPMetadata{}, invalidResponse("metadata has no signing certificate")
	}

	ssoURL := ""
	for _, service := range childElements(idp, metadataNamespace, "SingleSignOnService") {
		binding, _ := noNamespaceAttribute(service, "Binding")
		if binding != bindingHTTPRedirect {
			continue
		}
		if ssoURL != "" {
			return IDPMetadata{}, invalidResponse("metadata has more than one redirect SSO endpoint")
		}
		location, err := requiredAttribute(service, "Location")
		if err != nil {
			return IDPMetadata{}, invalidResponse("metadata SSO endpoint has no location")
		}
		normalized, err := parseEndpoint(location, policy.AllowHTTP, false)
		if err != nil {
			return IDPMetadata{}, invalidResponse("metadata SSO location is not an allowed URL")
		}
		ssoURL = normalized
	}
	if ssoURL == "" {
		return IDPMetadata{}, invalidResponse("metadata has no redirect SSO endpoint")
	}

	formats := make([]string, 0, 4)
	for _, format := range childElements(idp, metadataNamespace, "NameIDFormat") {
		text, err := elementText(format, "NameIDFormat")
		if err != nil {
			return IDPMetadata{}, err
		}
		if !slices.Contains(formats, text) {
			formats = append(formats, text)
		}
	}

	return IDPMetadata{
		EntityID:      entityID,
		SSOURL:        ssoURL,
		Certificates:  certificates,
		NameIDFormats: formats,
	}, nil
}

// Metadata renders this service provider's metadata document: entityID,
// assertion consumer service, NameID formats, and the public signing
// certificate. It never contains private key material -- the module holds a
// private key only as the optional AuthnRequest signer, and that field is not
// rendered here.
func (p *Provider) Metadata() ([]byte, error) {
	if err := p.usable(); err != nil {
		return nil, err
	}
	document := etree.NewDocument()
	document.CreateProcInst("xml", `version="1.0" encoding="UTF-8"`)
	entity := document.CreateElement("md:EntityDescriptor")
	entity.CreateAttr("xmlns:md", metadataNamespace)
	entity.CreateAttr("xmlns:ds", dsigNamespace)
	entity.CreateAttr("entityID", p.entityID)
	descriptor := entity.CreateElement("md:SPSSODescriptor")
	descriptor.CreateAttr("AuthnRequestsSigned", strconv.FormatBool(p.spPrivateKey != nil))
	descriptor.CreateAttr("WantAssertionsSigned", "true")
	descriptor.CreateAttr("protocolSupportEnumeration", protocolNamespace)
	if p.spCertificate != nil {
		keyDescriptor := descriptor.CreateElement("md:KeyDescriptor")
		keyDescriptor.CreateAttr("use", "signing")
		keyInfo := keyDescriptor.CreateElement("ds:KeyInfo")
		x509Data := keyInfo.CreateElement("ds:X509Data")
		certificate := x509Data.CreateElement("ds:X509Certificate")
		certificate.SetText(base64.StdEncoding.EncodeToString(p.spCertificate.Raw))
	}
	for _, format := range p.nameIDFormats {
		element := descriptor.CreateElement("md:NameIDFormat")
		element.SetText(format)
	}
	service := descriptor.CreateElement("md:AssertionConsumerService")
	service.CreateAttr("Binding", bindingHTTPPost)
	service.CreateAttr("Location", p.acsURL)
	service.CreateAttr("index", "0")
	service.CreateAttr("isDefault", "true")
	return document.WriteToBytes()
}
