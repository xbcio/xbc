package saml

import (
	"crypto"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/russellhaering/goxmldsig/etreeutils"
)

func TestVerifyAcceptsAssertionSignedResponse(t *testing.T) {
	provider := newTestProvider(t, nil)
	identity := mustVerify(t, provider, responseInput(buildSignedResponse(t, nil)))

	if identity.Protocol != protocolName {
		t.Fatalf("Protocol = %q, want %q", identity.Protocol, protocolName)
	}
	if identity.Provider != "corp" {
		t.Fatalf("Provider = %q, want corp", identity.Provider)
	}
	if identity.Issuer != testConfig().Issuer {
		t.Fatalf("Issuer = %q, want %q", identity.Issuer, testConfig().Issuer)
	}
	if identity.Subject != testNameID {
		t.Fatalf("Subject = %q, want %q", identity.Subject, testNameID)
	}
	if identity.Claims["email"] != testMail {
		t.Fatalf("email claim = %#v, want %q", identity.Claims["email"], testMail)
	}
	if identity.Claims["display_name"] != testDisplayName {
		t.Fatalf("display_name claim = %#v, want %q", identity.Claims["display_name"], testDisplayName)
	}
	if !identity.AuthTime.Equal(testNow) {
		t.Fatalf("AuthTime = %s, want %s", identity.AuthTime, testNow)
	}
}

func TestVerifyAcceptsResponseSignedResponse(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.signResponse = true
	})
	identity := mustVerify(t, provider, responseInput(response))
	if identity.Subject != testNameID {
		t.Fatalf("Subject = %q, want %q", identity.Subject, testNameID)
	}
}

func TestVerifyAcceptsBothResponseAndAssertionSigned(t *testing.T) {
	// The module requires exactly one signature, so a dual-signed response is
	// an ambiguity rather than a stronger statement.
	provider := newTestProvider(t, nil)
	document, options := buildResponseDocument(t, nil)
	assertion := findTestElement(t, document.Root(), assertionNamespace, "Assertion")
	signedAssertion := signTestElement(t, assertion, options)
	attachSigned(t, document, assertion, signedAssertion)
	signedResponse := signTestElement(t, document.Root(), options)
	document.SetRoot(signedResponse)
	mustRejectResponse(t, provider, responseInput(serializeTestDocument(t, document)), ErrInvalidResponse)
}

func TestVerifyAcceptsCertificateRotation(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.IdPCertificates = []*x509.Certificate{idpCertificate, rotatedCertificate}
	})
	old := buildSignedResponse(t, nil)
	newer := buildSignedResponse(t, func(options *responseOptions) {
		options.assertionID = "_assertionfedcba9876543210"
		options.signingKey = rotatedKey
		options.signingCertificate = rotatedCertificate
	})
	mustVerify(t, provider, responseInput(old))
	mustVerify(t, provider, responseInput(newer))
}

func TestVerifyRejectsUntrustedSigningCertificate(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.signingKey = untrustedKey
		options.signingCertificate = untrustedCertificate
	})
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}

func TestVerifyRejectsTamperedAssertion(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, nil)
	tampered := strings.Replace(string(response), testDisplayName, "Mallory Example", 1)
	if tampered == string(response) {
		t.Fatal("tampering did not change the response")
	}
	mustRejectResponse(t, provider, responseInput([]byte(tampered)), ErrInvalidResponse)
}

func TestVerifyAcceptsDefaultNamespaceAssertion(t *testing.T) {
	// Some IdPs emit the assertion with a default namespace rather than a
	// prefix. Verification must depend on namespace URIs, never on prefixes.
	// The rewrite happens before signing because canonicalization covers the
	// namespace declarations themselves.
	provider := newTestProvider(t, nil)
	document, options := buildResponseDocument(t, nil)
	assertion := findTestElement(t, document.Root(), assertionNamespace, "Assertion")
	// The assertion still declares the prefixes the Response declares; the
	// inherited-declaration form is covered by its own test.
	assertion.CreateAttr("xmlns", assertionNamespace)
	assertion.Space = ""
	setNamespaceToDefault(assertion)
	attachSigned(t, document, assertion, signTestElement(t, assertion, options))
	mustVerify(t, provider, responseInput(serializeTestDocument(t, document)))
}

func setNamespaceToDefault(element *etree.Element) {
	if element.Space == "saml" {
		element.Space = ""
	}
	for _, child := range element.ChildElements() {
		setNamespaceToDefault(child)
	}
}

func TestVerifyAcceptsArbitraryNamespacePrefixes(t *testing.T) {
	provider := newTestProvider(t, nil)
	document, options := buildResponseDocument(t, nil)
	renamePrefix(document.Root(), "samlp", "p")
	renamePrefix(document.Root(), "saml", "a")
	assertion := findTestElement(t, document.Root(), assertionNamespace, "Assertion")
	attachSigned(t, document, assertion, signTestElement(t, assertion, options))
	mustVerify(t, provider, responseInput(serializeTestDocument(t, document)))
}

func TestVerifyAcceptsAssertionInheritingResponseNamespaces(t *testing.T) {
	// The assertion declares no namespaces of its own: both prefixes are in
	// scope from the Response. Canonicalization covers that inherited set, so
	// verification must reproduce it on the subtree it hands to the library.
	provider := newTestProvider(t, nil)
	document, options := buildResponseDocument(t, nil)
	assertion := findTestElement(t, document.Root(), assertionNamespace, "Assertion")
	attributes := make([]etree.Attr, 0, len(assertion.Attr))
	for _, attribute := range assertion.Attr {
		if attribute.Space == "xmlns" || (attribute.Space == "" && attribute.Key == "xmlns") {
			continue
		}
		attributes = append(attributes, attribute)
	}
	assertion.Attr = attributes
	parentContext, err := etreeutils.NSBuildParentContext(assertion)
	if err != nil {
		t.Fatalf("build namespace context: %v", err)
	}
	detached, err := etreeutils.NSDetatch(parentContext, assertion)
	if err != nil {
		t.Fatalf("detach assertion: %v", err)
	}
	signed := signTestElement(t, detached, options)
	assertion.AddChild(detachSignature(t, signed))
	mustVerify(t, provider, responseInput(serializeTestDocument(t, document)))
}

// renamePrefix rewrites one namespace prefix throughout the tree, leaving the
// namespace URI and the declarations untouched.
func renamePrefix(element *etree.Element, from, to string) {
	if element.Space == from {
		element.Space = to
	}
	for index := range element.Attr {
		if element.Attr[index].Space == "xmlns" && element.Attr[index].Key == from {
			element.Attr[index].Key = to
		}
	}
	for _, child := range element.ChildElements() {
		renamePrefix(child, from, to)
	}
}

func TestVerifyRejectsUnsignedResponse(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.omitSignature = true
	})
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}

func TestVerifyRejectsSHA1Signature(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.hash = crypto.SHA1
	})
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}

func TestVerifyRejectsNilContext(t *testing.T) {
	provider := newTestProvider(t, nil)
	if _, err := Verify(nil, provider, responseInput(buildSignedResponse(t, nil))); !errors.Is(err, ErrNilContext) {
		t.Fatalf("Verify(nil context) = %v, want ErrNilContext", err)
	}
}

func TestVerifyRejectsNilProvider(t *testing.T) {
	if _, err := Verify(t.Context(), nil, ResponseInput{}); !errors.Is(err, ErrNilProvider) {
		t.Fatalf("Verify(nil provider) = %v, want ErrNilProvider", err)
	}
}

func TestVerifyRejectsUninitializedProvider(t *testing.T) {
	provider := &Provider{}
	if _, err := Verify(t.Context(), provider, ResponseInput{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Verify(zero provider) = %v, want ErrInvalidConfig", err)
	}
}

func TestVerifyRejectsMalformedInput(t *testing.T) {
	provider := newTestProvider(t, nil)
	valid := buildSignedResponse(t, nil)
	cases := map[string]ResponseInput{
		"missing expected request id": {ResponseXML: valid, ExpectedRequestID: "", ACSURL: testConfig().ACSURL},
		"malformed expected request id": {
			ResponseXML: valid, ExpectedRequestID: "-not-an-id", ACSURL: testConfig().ACSURL,
		},
		"wrong acs url": {
			ResponseXML: valid, ExpectedRequestID: testRequestID, ACSURL: "https://sp.example.com/other",
		},
		"empty document": {ExpectedRequestID: testRequestID, ACSURL: testConfig().ACSURL},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			mustRejectResponse(t, provider, input, ErrInvalidResponse)
		})
	}
}

func TestVerifyRejectsResponseConditions(t *testing.T) {
	cases := map[string]func(*responseOptions){
		"status is not success": func(options *responseOptions) {
			options.statusCode = "urn:oasis:names:tc:SAML:2.0:status:Requester"
		},
		"destination mismatch": func(options *responseOptions) {
			options.destination = "https://sp.example.com/other"
		},
		"destination missing": func(options *responseOptions) {
			options.destination = ""
		},
		"response in response to mismatch": func(options *responseOptions) {
			options.responseInResponseTo = "_someoneelsesrequest"
		},
		"response in response to missing": func(options *responseOptions) {
			options.responseInResponseTo = ""
		},
		"response issuer mismatch": func(options *responseOptions) {
			options.issuer = "https://evil.example.com/entity"
		},
		"response issuer missing": func(options *responseOptions) {
			options.omitResponseIssuer = true
		},
		"assertion issuer mismatch": func(options *responseOptions) {
			options.assertionIssuer = "https://evil.example.com/entity"
		},
		"assertion issuer missing": func(options *responseOptions) {
			options.omitAssertionIssuer = true
		},
		"recipient mismatch": func(options *responseOptions) {
			options.recipient = "https://sp.example.com/other"
		},
		"recipient missing": func(options *responseOptions) {
			options.recipient = ""
		},
		"confirmation in response to mismatch": func(options *responseOptions) {
			options.confirmationInResponseTo = "_someoneelsesrequest"
		},
		"confirmation in response to missing": func(options *responseOptions) {
			options.confirmationInResponseTo = ""
		},
		"audience mismatch": func(options *responseOptions) {
			options.audience = "https://other.example.com/entity"
		},
		"audience restriction missing": func(options *responseOptions) {
			options.omitAudienceRestriction = true
		},
		"conditions missing": func(options *responseOptions) {
			options.omitConditions = true
		},
		"conditions expiry missing": func(options *responseOptions) {
			options.conditionsNotOnOrAfter = nil
		},
		"confirmation expiry missing": func(options *responseOptions) {
			options.confirmationNotOnOrAfter = nil
		},
		"conditions expired": func(options *responseOptions) {
			expired := testNow.Add(-time.Minute)
			options.conditionsNotOnOrAfter = &expired
		},
		"conditions not yet valid": func(options *responseOptions) {
			future := testNow.Add(time.Minute)
			options.conditionsNotBefore = &future
		},
		"confirmation expired": func(options *responseOptions) {
			expired := testNow.Add(-time.Minute)
			options.confirmationNotOnOrAfter = &expired
		},
		"confirmation not yet valid": func(options *responseOptions) {
			future := testNow.Add(time.Minute)
			options.confirmationNotBefore = &future
		},
		"inverted conditions window": func(options *responseOptions) {
			notBefore := testNow.Add(time.Minute)
			notOnOrAfter := testNow.Add(-time.Minute)
			options.conditionsNotBefore = &notBefore
			options.conditionsNotOnOrAfter = &notOnOrAfter
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			provider := newTestProvider(t, nil)
			mustRejectResponse(t, provider, responseInput(buildSignedResponse(t, mutate)), ErrInvalidResponse)
		})
	}
}

func TestVerifyRejectsUnacceptableSubject(t *testing.T) {
	cases := map[string]func(*responseOptions){
		"transient nameid format": func(options *responseOptions) {
			options.nameIDFormat = NameIDFormatTransient
		},
		"email nameid format": func(options *responseOptions) {
			options.nameIDFormat = NameIDFormatEmail
		},
		"unspecified nameid format not configured": func(options *responseOptions) {
			options.omitNameIDFormat = true
		},
		"empty nameid": func(options *responseOptions) {
			options.nameID = ""
		},
		"missing nameid": func(options *responseOptions) {
			options.omitNameID = true
		},
		"non bearer confirmation": func(options *responseOptions) {
			options.subjectConfirmationMethod = assertionNamespace + ":cm:holder-of-key"
		},
		"missing subject": func(options *responseOptions) {
			options.omitSubject = true
		},
		"missing subject confirmation": func(options *responseOptions) {
			options.omitSubjectConfirmation = true
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			provider := newTestProvider(t, nil)
			mustRejectResponse(t, provider, responseInput(buildSignedResponse(t, mutate)), ErrInvalidResponse)
		})
	}
}

func TestVerifyAcceptsConfiguredUnspecifiedNameID(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.NameIDFormats = []string{NameIDFormatPersistent, NameIDFormatUnspecified}
	})
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.omitNameIDFormat = true
	})
	identity := mustVerify(t, provider, responseInput(response))
	if identity.Subject != testNameID {
		t.Fatalf("Subject = %q, want %q", identity.Subject, testNameID)
	}
}

func TestVerifyRejectsMissingConfiguredAttribute(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.attributes = []testAttribute{{name: "mail", values: []string{testMail}}}
	})
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}

func TestVerifyMapsMultipleAttributeValues(t *testing.T) {
	provider := newTestProvider(t, func(config *Config) {
		config.ClaimAttributes = map[string]string{"groups": "groups"}
	})
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.attributes = []testAttribute{
			{name: "groups", values: []string{"engineering", "operations"}},
		}
	})
	identity := mustVerify(t, provider, responseInput(response))
	groups, ok := identity.Claims["groups"].([]string)
	if !ok || len(groups) != 2 || groups[0] != "engineering" || groups[1] != "operations" {
		t.Fatalf("groups claim = %#v, want two values", identity.Claims["groups"])
	}
	groups[0] = "mutated"
	second := mustVerify(t, provider, responseInput(buildSignedResponse(t, func(options *responseOptions) {
		options.assertionID = "_assertionfedcba9876543210"
		options.attributes = []testAttribute{
			{name: "groups", values: []string{"engineering", "operations"}},
		}
	})))
	if second.Claims["groups"].([]string)[0] != "engineering" {
		t.Fatal("claims share a slice across verifications")
	}
}

func TestVerifyDoesNotShareClaimMapWithCaller(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		options.assertionID = "_assertionfedcba9876543210"
	})
	first := mustVerify(t, provider, responseInput(buildSignedResponse(t, nil)))
	first.Claims["email"] = "tampered@example.com"
	second := mustVerify(t, provider, responseInput(response))
	if second.Claims["email"] != testMail {
		t.Fatalf("email claim = %#v after caller mutation, want %q", second.Claims["email"], testMail)
	}
}

func TestVerifyRejectsMalformedConditionTimestamps(t *testing.T) {
	provider := newTestProvider(t, nil)
	valid := buildSignedResponse(t, nil)
	response := strings.Replace(string(valid), "2026-10-19T12:05:00Z", "not-a-time", 1)
	if response == string(valid) {
		t.Fatal("fixture no longer contains the expected timestamp")
	}
	mustRejectResponse(t, provider, responseInput([]byte(response)), ErrInvalidResponse)
}
