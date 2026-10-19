package saml

import (
	"testing"

	"github.com/beevik/etree"
)

// directChildren returns the direct children in the namespace with the tag.
func directChildren(element *etree.Element, namespace, tag string) []*etree.Element {
	var matches []*etree.Element
	for _, child := range element.ChildElements() {
		if child.Tag == tag && child.NamespaceURI() == namespace {
			matches = append(matches, child)
		}
	}
	return matches
}

// signatureOf returns the single ds:Signature child of element.
func signatureOf(t *testing.T, element *etree.Element) *etree.Element {
	t.Helper()
	signatures := directChildren(element, dsigNamespace, "Signature")
	if len(signatures) != 1 {
		t.Fatalf("expected one signature child, found %d", len(signatures))
	}
	return signatures[0]
}

// detachSignature removes the signature child from element and returns it.
func detachSignature(t *testing.T, element *etree.Element) *etree.Element {
	t.Helper()
	signature := signatureOf(t, element)
	for position, child := range element.ChildElements() {
		if child == signature {
			element.RemoveChildAt(position)
			return signature
		}
	}
	t.Fatal("signature child not found")
	return nil
}

// referenceElement returns the single ds:Reference inside the signature.
func referenceElement(t *testing.T, signature *etree.Element) *etree.Element {
	t.Helper()
	references := []*etree.Element{}
	var walk func(*etree.Element)
	walk = func(element *etree.Element) {
		for _, child := range element.ChildElements() {
			if child.Tag == "Reference" && child.NamespaceURI() == dsigNamespace {
				references = append(references, child)
			}
			walk(child)
		}
	}
	walk(signature)
	if len(references) != 1 {
		t.Fatalf("expected one reference, found %d", len(references))
	}
	return references[0]
}

func TestVerifyRejectsTwoAssertions(t *testing.T) {
	// The classic signature-wrapping setup: the signed assertion and a second
	// attacker-chosen assertion in the same response.
	document, options := buildResponseDocument(t, func(options *responseOptions) {
		options.extraResponseXML = `<saml:Assertion xmlns:saml="` + assertionNamespace + `" ID="_evilassertion" Version="2.0"><saml:Issuer>` + testConfig().Issuer + `</saml:Issuer></saml:Assertion>`
	})
	assertions := directChildren(document.Root(), assertionNamespace, "Assertion")
	if len(assertions) != 2 {
		t.Fatalf("expected two assertions, found %d", len(assertions))
	}
	attachSigned(t, document, assertions[0], signTestElement(t, assertions[0], options))
	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsNestedAssertion(t *testing.T) {
	document, options := buildResponseDocument(t, func(options *responseOptions) {
		options.extraAssertionXML = `<saml:Assertion ID="_evilassertion" Version="2.0"/>`
	})
	assertions := directChildren(document.Root(), assertionNamespace, "Assertion")
	if len(assertions) != 1 {
		t.Fatalf("expected one direct assertion, found %d", len(assertions))
	}
	attachSigned(t, document, assertions[0], signTestElement(t, assertions[0], options))
	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsAssertionOutsideDirectPosition(t *testing.T) {
	document, options := buildResponseDocument(t, nil)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	attachSigned(t, document, assertion, signTestElement(t, assertion, options))
	signed := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	document.Root().RemoveChild(signed)
	extensions := document.Root().CreateElement("samlp:Extensions")
	extensions.Space = "samlp"
	extensions.CreateAttr("xmlns:samlp", protocolNamespace)
	extensions.AddChild(signed)

	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsSignatureMovedToAnotherAssertion(t *testing.T) {
	// The signature references the original assertion ID while sitting inside a
	// different assertion. Trust must follow the reference, not the parent.
	document, options := buildResponseDocument(t, nil)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	signed := signTestElement(t, assertion, options)
	signature := detachSignature(t, signed)
	signed.CreateAttr("ID", "_evilassertion")
	signed.AddChild(signature)
	attachSigned(t, document, assertion, signed)

	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsDuplicateID(t *testing.T) {
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.extraResponseXML = `<samlp:Extensions xmlns:samlp="` + protocolNamespace + `" ID="` + testAssertionID + `"/>`
	})
	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}

func TestVerifyRejectsTwoSignatures(t *testing.T) {
	document, options := buildResponseDocument(t, nil)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	attachSigned(t, document, assertion, signTestElement(t, assertion, options))
	extra := document.Root().CreateElement("ds:Signature")
	extra.Space = "ds"
	extra.CreateAttr("xmlns:ds", dsigNamespace)
	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsSignatureInsideObject(t *testing.T) {
	document, options := buildResponseDocument(t, nil)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	// The Object exists before signing, so only the signature's attachment
	// point is wrong; a rejection cannot come from a digest mismatch.
	object := assertion.CreateElement("ds:Object")
	object.Space = "ds"
	object.CreateAttr("xmlns:ds", dsigNamespace)
	signed := signTestElement(t, assertion, options)
	objectInSigned := directChildren(signed, dsigNamespace, "Object")[0]
	objectInSigned.AddChild(detachSignature(t, signed))
	attachSigned(t, document, assertion, signed)

	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsEncryptedAssertion(t *testing.T) {
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.extraResponseXML = `<saml:EncryptedAssertion xmlns:saml="` + assertionNamespace + `"/>`
	})
	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}

func TestVerifyRejectsForeignElementInsideSignature(t *testing.T) {
	document, options := buildResponseDocument(t, nil)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	signed := signTestElement(t, assertion, options)
	signature := signatureOf(t, signed)
	foreign := signature.CreateElement("foo:Extra")
	foreign.Space = "foo"
	foreign.CreateAttr("xmlns:foo", "urn:foreign")
	attachSigned(t, document, assertion, signed)

	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsRetargetedReferenceOnSignedAssertion(t *testing.T) {
	document, options := buildResponseDocument(t, nil)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	signed := signTestElement(t, assertion, options)
	referenceElement(t, signatureOf(t, signed)).CreateAttr("URI", "#"+testResponseID)
	attachSigned(t, document, assertion, signed)

	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsRetargetedReferenceOnSignedResponse(t *testing.T) {
	document, options := buildResponseDocument(t, func(options *responseOptions) {
		options.signResponse = true
	})
	signed := signTestElement(t, document.Root(), options)
	referenceElement(t, signatureOf(t, signed)).CreateAttr("URI", "#"+testAssertionID)
	document.SetRoot(signed)

	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyRejectsSignatureOnUnrelatedElement(t *testing.T) {
	// A signature attached to Extensions references the response ID. Its parent
	// is neither the response nor the assertion, so it is an ambiguity.
	document, options := buildResponseDocument(t, func(options *responseOptions) {
		options.extraResponseXML = `<samlp:Extensions xmlns:samlp="` + protocolNamespace + `" ID="_extensions"/>`
	})
	extensions := directChildren(document.Root(), protocolNamespace, "Extensions")[0]
	signed := signTestElement(t, extensions, options)
	document.Root().RemoveChild(extensions)
	document.Root().AddChild(signed)
	// Retarget the reference to the response so only the attachment is wrong.
	referenceElement(t, signatureOf(t, signed)).CreateAttr("URI", "#"+testResponseID)

	provider := newTestProvider(t, nil)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}
