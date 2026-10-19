package saml

import (
	"strconv"
	"strings"
	"testing"

	"github.com/beevik/etree"
)

// buildSignedResponseWithTree builds the default response, lets the test
// mutate the parsed tree, and only then signs the assertion, so a rejection is
// attributable to the mutated structural rule rather than a broken signature.
func buildSignedResponseWithTree(t *testing.T, mutate func(*etree.Document)) []byte {
	t.Helper()
	document, options := buildResponseDocument(t, nil)
	mutate(document)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	attachSigned(t, document, assertion, signTestElement(t, assertion, options))
	return serializeTestDocument(t, document)
}

func TestVerifyRejectsAmbiguousAssertionStructure(t *testing.T) {
	cases := map[string]func(*testing.T, *etree.Document){
		"missing response version": func(t *testing.T, document *etree.Document) {
			document.Root().RemoveAttr("Version")
		},
		"wrong response version": func(t *testing.T, document *etree.Document) {
			document.Root().CreateAttr("Version", "1.1")
		},
		"missing response id": func(t *testing.T, document *etree.Document) {
			document.Root().RemoveAttr("ID")
		},
		"missing status": func(t *testing.T, document *etree.Document) {
			document.Root().RemoveChild(directChildren(document.Root(), protocolNamespace, "Status")[0])
		},
		"missing status code": func(t *testing.T, document *etree.Document) {
			status := directChildren(document.Root(), protocolNamespace, "Status")[0]
			status.RemoveChild(directChildren(status, protocolNamespace, "StatusCode")[0])
		},
		"wrong assertion version": func(t *testing.T, document *etree.Document) {
			assertionOf(t, document).CreateAttr("Version", "1.1")
		},
		"missing assertion id": func(t *testing.T, document *etree.Document) {
			assertionOf(t, document).RemoveAttr("ID")
		},
		"two authentication statements": func(t *testing.T, document *etree.Document) {
			assertion := assertionOf(t, document)
			statement := directChildren(assertion, assertionNamespace, "AuthnStatement")[0]
			assertion.AddChild(statement.Copy())
		},
		"missing subject confirmation data": func(t *testing.T, document *etree.Document) {
			confirmation := findTestElement(t, document.Root(), assertionNamespace, "SubjectConfirmation")
			confirmation.RemoveChild(directChildren(confirmation, assertionNamespace, "SubjectConfirmationData")[0])
		},
		"two subject confirmations": func(t *testing.T, document *etree.Document) {
			subject := findTestElement(t, document.Root(), assertionNamespace, "Subject")
			confirmation := directChildren(subject, assertionNamespace, "SubjectConfirmation")[0]
			subject.AddChild(confirmation.Copy())
		},
		"confirmation method missing": func(t *testing.T, document *etree.Document) {
			confirmation := findTestElement(t, document.Root(), assertionNamespace, "SubjectConfirmation")
			confirmation.RemoveAttr("Method")
		},
		"nameid contains nested xml": func(t *testing.T, document *etree.Document) {
			nameID := findTestElement(t, document.Root(), assertionNamespace, "NameID")
			nameID.CreateElement("saml:Nested")
		},
		"nameid too long": func(t *testing.T, document *etree.Document) {
			findTestElement(t, document.Root(), assertionNamespace, "NameID").SetText(strings.Repeat("a", maxNameIDLength+1))
		},
		"encrypted attribute": func(t *testing.T, document *etree.Document) {
			assertion := assertionOf(t, document)
			encrypted := assertion.CreateElement("saml:EncryptedAttribute")
			encrypted.Space = "saml"
		},
		"encrypted id": func(t *testing.T, document *etree.Document) {
			subject := findTestElement(t, document.Root(), assertionNamespace, "Subject")
			encrypted := subject.CreateElement("saml:EncryptedID")
			encrypted.Space = "saml"
		},
		"encryption element outside the assertion": func(t *testing.T, document *etree.Document) {
			extensions := document.Root().CreateElement("samlp:Extensions")
			extensions.Space = "samlp"
			encrypted := extensions.CreateElement("ds:EncryptedKey")
			encrypted.Space = "ds"
			encrypted.CreateAttr("xmlns:ds", dsigNamespace)
		},
		"duplicate attribute names": func(t *testing.T, document *etree.Document) {
			assertion := assertionOf(t, document)
			statement := directChildren(assertion, assertionNamespace, "AttributeStatement")[0]
			attributes := directChildren(statement, assertionNamespace, "Attribute")
			statement.AddChild(attributes[0].Copy())
		},
		"attribute without values": func(t *testing.T, document *etree.Document) {
			assertion := assertionOf(t, document)
			statement := directChildren(assertion, assertionNamespace, "AttributeStatement")[0]
			attribute := statement.CreateElement("saml:Attribute")
			attribute.Space = "saml"
			attribute.CreateAttr("Name", "empty")
		},
		"attribute name with whitespace": func(t *testing.T, document *etree.Document) {
			assertion := assertionOf(t, document)
			statement := directChildren(assertion, assertionNamespace, "AttributeStatement")[0]
			attribute := statement.CreateElement("saml:Attribute")
			attribute.Space = "saml"
			attribute.CreateAttr("Name", "bad name")
			value := attribute.CreateElement("saml:AttributeValue")
			value.Space = "saml"
			value.SetText("value")
		},
		"attribute value contains nested xml": func(t *testing.T, document *etree.Document) {
			value := firstTestElement(t, document.Root(), assertionNamespace, "AttributeValue")
			value.CreateElement("saml:Nested")
		},
		"attribute value too long": func(t *testing.T, document *etree.Document) {
			value := firstTestElement(t, document.Root(), assertionNamespace, "AttributeValue")
			value.SetText(strings.Repeat("a", maxAttributeLen+1))
		},
		"too many attribute values": func(t *testing.T, document *etree.Document) {
			value := firstTestElement(t, document.Root(), assertionNamespace, "AttributeValue")
			attribute := value.Parent()
			for index := 0; index < maxAttributeVals; index++ {
				extra := attribute.CreateElement("saml:AttributeValue")
				extra.Space = "saml"
				extra.SetText("value")
			}
		},
		"too many attributes": func(t *testing.T, document *etree.Document) {
			assertion := assertionOf(t, document)
			statement := directChildren(assertion, assertionNamespace, "AttributeStatement")[0]
			for index := 0; index < maxAttributes+1; index++ {
				attribute := statement.CreateElement("saml:Attribute")
				attribute.Space = "saml"
				attribute.CreateAttr("Name", "extra"+strconv.Itoa(index))
				value := attribute.CreateElement("saml:AttributeValue")
				value.Space = "saml"
				value.SetText("value")
			}
		},
		"audience contains nested xml": func(t *testing.T, document *etree.Document) {
			findTestElement(t, document.Root(), assertionNamespace, "Audience").CreateElement("saml:Nested")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			provider := newTestProvider(t, nil)
			response := buildSignedResponseWithTree(t, func(document *etree.Document) {
				mutate(t, document)
			})
			mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
		})
	}
}

// assertionOf returns the single direct assertion of the response.
func assertionOf(t *testing.T, document *etree.Document) *etree.Element {
	t.Helper()
	assertions := directChildren(document.Root(), assertionNamespace, "Assertion")
	if len(assertions) != 1 {
		t.Fatalf("expected one assertion, found %d", len(assertions))
	}
	return assertions[0]
}

func TestVerifyEvaluatesEveryAudienceRestriction(t *testing.T) {
	provider := newTestProvider(t, nil)

	bothMatch := buildSignedResponseWithTree(t, func(document *etree.Document) {
		conditions := findTestElement(t, document.Root(), assertionNamespace, "Conditions")
		conditions.AddChild(directChildren(conditions, assertionNamespace, "AudienceRestriction")[0].Copy())
	})
	mustVerify(t, provider, responseInput(bothMatch))

	oneMismatch := buildSignedResponseWithTree(t, func(document *etree.Document) {
		conditions := findTestElement(t, document.Root(), assertionNamespace, "Conditions")
		restriction := directChildren(conditions, assertionNamespace, "AudienceRestriction")[0]
		// Mutate before copying: a detached subtree no longer resolves prefixes
		// declared on its ancestors.
		directChildren(restriction, assertionNamespace, "Audience")[0].SetText("https://other.example.com")
		conditions.AddChild(restriction.Copy())
	})
	mustRejectResponse(t, provider, responseInput(oneMismatch), ErrInvalidResponse)
}

func TestVerifyAcceptsAnyAudienceInsideOneRestriction(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponseWithTree(t, func(document *etree.Document) {
		conditions := findTestElement(t, document.Root(), assertionNamespace, "Conditions")
		restriction := directChildren(conditions, assertionNamespace, "AudienceRestriction")[0]
		first := directChildren(restriction, assertionNamespace, "Audience")[0]
		first.SetText("https://other.example.com")
		second := restriction.CreateElement("saml:Audience")
		second.Space = "saml"
		second.SetText(testConfig().EntityID)
	})
	mustVerify(t, provider, responseInput(response))
}

func TestVerifyRejectsEmptyAudienceRestriction(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponseWithTree(t, func(document *etree.Document) {
		conditions := findTestElement(t, document.Root(), assertionNamespace, "Conditions")
		restriction := directChildren(conditions, assertionNamespace, "AudienceRestriction")[0]
		for _, audience := range directChildren(restriction, assertionNamespace, "Audience") {
			restriction.RemoveChild(audience)
		}
	})
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}
