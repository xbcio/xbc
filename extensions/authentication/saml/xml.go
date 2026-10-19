package saml

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/beevik/etree"
)

// XML namespaces defined by SAML 2.0 and XML Digital Signature.
const (
	protocolNamespace  = "urn:oasis:names:tc:SAML:2.0:protocol"
	assertionNamespace = "urn:oasis:names:tc:SAML:2.0:assertion"
	metadataNamespace  = "urn:oasis:names:tc:SAML:2.0:metadata"
	dsigNamespace      = "http://www.w3.org/2000/09/xmldsig#"
)

// Protocol values that a response and an assertion must carry.
const (
	samlVersion = "2.0"

	statusSuccess = protocolNamespace + ":status:Success"

	bindingHTTPPost     = protocolNamespace + ":bindings:HTTP-POST"
	bindingHTTPRedirect = protocolNamespace + ":bindings:HTTP-Redirect"

	bearerConfirmation = "urn:oasis:names:tc:SAML:2.0:cm:bearer"
)

// Registered NameID formats. Only the persistent format carries a guarantee
// that the identifier is stable and never reused; unspecified is a deliberate
// opt-in because the application must then accept whatever the IdP chose.
const (
	NameIDFormatPersistent  = assertionNamespace + ":nameid-format:persistent"
	NameIDFormatUnspecified = assertionNamespace + ":nameid-format:unspecified"
	NameIDFormatTransient   = assertionNamespace + ":nameid-format:transient"
	NameIDFormatEmail       = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
)

// Limits applied before and while parsing untrusted XML. They bound the work a
// single callback can cause and are deliberately not configurable: a login
// response that needs more is not a legitimate response.
const (
	maxResponseBytes = 1 << 20 // 1 MiB
	maxMetadataBytes = 1 << 20 // 1 MiB
	maxXMLDepth      = 64
	maxIDLength      = 256
	maxNameIDLength  = 1024
	maxIssuerLength  = 1024
	maxEntityIDBytes = 1024
	maxRelayState    = 1024
	maxAttributes    = 128
	maxAttributeVals = 64
	maxAttributeLen  = 4096
	maxClaimNameLen  = 128
	maxAttributeName = 1024
)

// xmlIDPattern is the XML NCName shape a SAML ID attribute must have. A
// signature reference can only name such an ID, and restricting the shape
// keeps ID lookup unambiguous.
var xmlIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// parseXML parses an untrusted document with every parser-level protection the
// package relies on:
//
//   - a byte-size limit enforced before the parser runs;
//   - an explicit, case-insensitive rejection of DOCTYPE and ENTITY
//     declarations, because encoding/xml not resolving external entities does
//     not make accepting a DTD harmless;
//   - a depth limit inside etree;
//   - duplicate-attribute preservation so duplicates can be rejected instead
//     of silently collapsed;
//   - a second well-formedness pass (ValidateInput);
//   - a post-parse walk that rejects directive nodes (including any DOCTYPE
//     that survived the scan), duplicate attributes, and duplicate or
//     malformed ID values.
//
// Errors wrap ErrInvalidResponse and never include document text.
func parseXML(data []byte, limit int) (*etree.Element, error) {
	if len(data) == 0 || len(data) > limit {
		return nil, fmt.Errorf("%w: document is empty or above the size limit", ErrInvalidResponse)
	}
	if hasDTD(data) {
		return nil, fmt.Errorf("%w: document declares a DTD or entity", ErrInvalidResponse)
	}
	document := etree.NewDocument()
	document.ReadSettings = etree.ReadSettings{
		MaxDepth:               maxXMLDepth,
		PreserveDuplicateAttrs: true,
		ValidateInput:          true,
	}
	if err := document.ReadFromBytes(data); err != nil {
		return nil, fmt.Errorf("%w: malformed XML", ErrInvalidResponse)
	}
	for _, child := range document.Child {
		if _, isDirective := child.(*etree.Directive); isDirective {
			return nil, fmt.Errorf("%w: document contains a directive", ErrInvalidResponse)
		}
	}
	root := document.Root()
	if root == nil {
		return nil, fmt.Errorf("%w: document has no root element", ErrInvalidResponse)
	}
	if err := inspectTree(root); err != nil {
		return nil, err
	}
	return root, nil
}

// hasDTD reports whether the raw document declares a DTD or an entity. The
// scan is case-insensitive and runs before parsing, so a DTD can never reach
// the XML decoder even as an unexpanded directive.
func hasDTD(data []byte) bool {
	lower := strings.ToLower(string(data))
	return strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<!entity")
}

// inspectTree rejects directive nodes, duplicate attributes, and duplicate or
// malformed ID attribute values anywhere in the tree. Duplicate IDs are the
// primitive an XML signature-wrapping attack needs to make the verified node
// and the consumed node differ.
func inspectTree(root *etree.Element) error {
	ids := make(map[string]struct{})
	var walk func(element *etree.Element) error
	walk = func(element *etree.Element) error {
		seen := make(map[string]struct{}, len(element.Attr))
		for _, attribute := range element.Attr {
			// Resolve the prefix so a:name and b:name bound to the same
			// namespace are recognized as the duplicate they are.
			key := attribute.NamespaceURI() + "\x00" + attribute.Key
			if attribute.Space == "xmlns" {
				key = "xmlns\x00" + attribute.Key
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%w: duplicate attribute", ErrInvalidResponse)
			}
			seen[key] = struct{}{}
			if attribute.Space == "" && attribute.Key == "ID" {
				value := attribute.Value
				if value == "" || len(value) > maxIDLength || !xmlIDPattern.MatchString(value) {
					return fmt.Errorf("%w: malformed ID attribute", ErrInvalidResponse)
				}
				if _, duplicate := ids[value]; duplicate {
					return fmt.Errorf("%w: duplicate ID attribute", ErrInvalidResponse)
				}
				ids[value] = struct{}{}
			}
		}
		for _, child := range element.Child {
			switch typed := child.(type) {
			case *etree.Element:
				if err := walk(typed); err != nil {
					return err
				}
			case *etree.Directive:
				return fmt.Errorf("%w: document contains a directive", ErrInvalidResponse)
			}
		}
		return nil
	}
	return walk(root)
}

// inNamespace reports whether element is in the given namespace URI. etree
// stores the prefix in Element.Space; NamespaceURI resolves it through the
// xmlns declarations, so prefix rebinding cannot change the answer.
func inNamespace(element *etree.Element, namespace string) bool {
	return element.NamespaceURI() == namespace
}

// childElements returns the direct children in namespace with tag.
func childElements(element *etree.Element, namespace, tag string) []*etree.Element {
	var matches []*etree.Element
	for _, child := range element.ChildElements() {
		if child.Tag == tag && inNamespace(child, namespace) {
			matches = append(matches, child)
		}
	}
	return matches
}

// oneChild returns the single direct child in namespace with tag. An absent or
// ambiguous child is ErrInvalidResponse.
func oneChild(element *etree.Element, namespace, tag string) (*etree.Element, error) {
	matches := childElements(element, namespace, tag)
	if len(matches) != 1 {
		return nil, fmt.Errorf("%w: expected exactly one %s element", ErrInvalidResponse, tag)
	}
	return matches[0], nil
}

// elementsByTagDescendants returns every descendant (excluding element itself)
// in namespace with tag, in document order.
func elementsByTagDescendants(element *etree.Element, namespace, tag string) []*etree.Element {
	var matches []*etree.Element
	var walk func(current *etree.Element)
	walk = func(current *etree.Element) {
		for _, child := range current.ChildElements() {
			if child.Tag == tag && inNamespace(child, namespace) {
				matches = append(matches, child)
			}
			walk(child)
		}
	}
	walk(element)
	return matches
}

// noNamespaceAttribute returns the value of an unprefixed attribute and
// whether it is present.
func noNamespaceAttribute(element *etree.Element, key string) (string, bool) {
	for _, attribute := range element.Attr {
		if attribute.Space == "" && attribute.Key == key {
			return attribute.Value, true
		}
	}
	return "", false
}

// requiredAttribute returns an unprefixed attribute that must be present and
// non-empty.
func requiredAttribute(element *etree.Element, key string) (string, error) {
	value, present := noNamespaceAttribute(element, key)
	if !present || value == "" {
		return "", fmt.Errorf("%w: missing %s attribute", ErrInvalidResponse, key)
	}
	return value, nil
}

// elementText returns the trimmed character data of an element that must not
// contain child elements. AttributeValue and NameID are the shapes this guards.
func elementText(element *etree.Element, what string) (string, error) {
	if len(element.ChildElements()) != 0 {
		return "", fmt.Errorf("%w: %s contains nested XML", ErrInvalidResponse, what)
	}
	text := strings.TrimSpace(element.Text())
	if text == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrInvalidResponse, what)
	}
	if !utf8.ValidString(text) {
		return "", fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidResponse, what)
	}
	return text, nil
}

// validIdentifier reports whether a configured identifier is non-empty,
// bounded, and free of whitespace or control characters. It is used for
// entity IDs, which are URIs or application-chosen names.
func validIdentifier(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return utf8.ValidString(value)
}

// validURLText reports whether a configured value can be a URL, without
// whitespace or control characters.
func validURLText(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return utf8.ValidString(value)
}
