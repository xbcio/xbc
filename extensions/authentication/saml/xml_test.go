package saml

import (
	"errors"
	"strings"
	"testing"
)

func TestParseXMLRejectsDTDAndEntities(t *testing.T) {
	cases := map[string]string{
		"doctype":            `<!DOCTYPE foo><foo/>`,
		"uppercase doctype":  `<!DOCTYPE foo SYSTEM "http://example.com/foo.dtd"><foo/>`,
		"internal entity":    `<!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]><foo>&xxe;</foo>`,
		"entity declaration": `<foo><!ENTITY a "aaaa"></foo>`,
		"leading whitespace": "  \n<!doctype foo><foo/>",
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseXML([]byte(document), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestParseXMLRejectsConservativeEntityScan(t *testing.T) {
	// The pre-parse scan is deliberately conservative: an ENTITY marker inside
	// a comment is rejected rather than reasoned about.
	document := `<foo><!-- <!ENTITY harmless "value"> --></foo>`
	if _, err := parseXML([]byte(document), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsUnknownEntity(t *testing.T) {
	if _, err := parseXML([]byte(`<foo>&unknown;</foo>`), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsOversizedDocument(t *testing.T) {
	document := `<foo>` + strings.Repeat("a", maxResponseBytes) + `</foo>`
	if _, err := parseXML([]byte(document), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsEmptyDocument(t *testing.T) {
	if _, err := parseXML(nil, maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsExcessiveDepth(t *testing.T) {
	var builder strings.Builder
	for index := 0; index < maxXMLDepth+2; index++ {
		builder.WriteString("<e>")
	}
	builder.WriteString("<leaf/>")
	for index := 0; index < maxXMLDepth+2; index++ {
		builder.WriteString("</e>")
	}
	if _, err := parseXML([]byte(builder.String()), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsDuplicateAttributes(t *testing.T) {
	document := `<foo a="1" a="2"/>`
	root, err := parseXML([]byte(document), maxResponseBytes)
	if err == nil {
		t.Fatalf("parseXML accepted duplicate attributes: %#v", root)
	}
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsNamespacedDuplicateAttributes(t *testing.T) {
	document := `<foo xmlns:a="urn:x" xmlns:b="urn:x" a:k="1" b:k="2"/>`
	if _, err := parseXML([]byte(document), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsDuplicateIDs(t *testing.T) {
	document := `<foo><bar ID="same"/><baz ID="same"/></foo>`
	if _, err := parseXML([]byte(document), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("parseXML = %v, want ErrInvalidResponse", err)
	}
}

func TestParseXMLRejectsMalformedIDs(t *testing.T) {
	for _, value := range []string{"1abc", "-abc", ".abc", "a b", ""} {
		document := `<foo ID="` + value + `"/>`
		if _, err := parseXML([]byte(document), maxResponseBytes); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("parseXML(ID=%q) = %v, want ErrInvalidResponse", value, err)
		}
	}
}

func TestParseXMLAcceptsValidDocument(t *testing.T) {
	root, err := parseXML([]byte(`<foo ID="a"><bar ID="b">text</bar></foo>`), maxResponseBytes)
	if err != nil {
		t.Fatalf("parseXML: %v", err)
	}
	if root.Tag != "foo" {
		t.Fatalf("root tag = %q, want foo", root.Tag)
	}
}

func TestVerifyRejectsDTDResponse(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, nil)
	withDTD := []byte(`<!DOCTYPE foo [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>` + string(response))
	mustRejectResponse(t, provider, responseInput(withDTD), ErrInvalidResponse)
}

func TestVerifyRejectsOversizedResponse(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, nil)
	padded := append([]byte(strings.Repeat(" ", maxResponseBytes)), response...)
	mustRejectResponse(t, provider, responseInput(padded), ErrInvalidResponse)
}

func TestVerifyRejectsDeeplyNestedResponse(t *testing.T) {
	provider := newTestProvider(t, nil)
	response := buildSignedResponse(t, func(options *responseOptions) {
		var builder strings.Builder
		for index := 0; index < maxXMLDepth+2; index++ {
			builder.WriteString("<saml:e>")
		}
		builder.WriteString("<saml:leaf/>")
		for index := 0; index < maxXMLDepth+2; index++ {
			builder.WriteString("</saml:e>")
		}
		options.extraAssertionXML = builder.String()
	})
	mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
}

// TestBearerConfirmationUsesTheRegisteredMethod pins the literal SAML 2.0
// bearer confirmation method. Deriving it from the assertion namespace produces
// urn:oasis:names:tc:SAML:2.0:assertion:cm:bearer, which no IdP emits; every
// real response would then be rejected as missing a bearer confirmation.
func TestBearerConfirmationUsesTheRegisteredMethod(t *testing.T) {
	if bearerConfirmation != "urn:oasis:names:tc:SAML:2.0:cm:bearer" {
		t.Fatalf("bearerConfirmation = %q, want urn:oasis:names:tc:SAML:2.0:cm:bearer", bearerConfirmation)
	}
}

// TestRegisteredSAMLOUIsMatchTheSpecification pins every registered URI this
// package emits or accepts to its SAML 2.0 specification literal. Tests that
// build fixtures from these constants cannot catch a wrong constant because
// both sides share the defect; this test compares against the specification
// text instead.
func TestRegisteredSAMLOUIsMatchTheSpecification(t *testing.T) {
	cases := map[string]struct {
		actual string
		want   string
	}{
		"status success":        {statusSuccess, "urn:oasis:names:tc:SAML:2.0:status:Success"},
		"HTTP-POST binding":     {bindingHTTPPost, "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"},
		"HTTP-Redirect binding": {bindingHTTPRedirect, "urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect"},
		"persistent NameID":     {NameIDFormatPersistent, "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent"},
		"unspecified NameID":    {NameIDFormatUnspecified, "urn:oasis:names:tc:SAML:2.0:nameid-format:unspecified"},
		"transient NameID":      {NameIDFormatTransient, "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			if testCase.actual != testCase.want {
				t.Fatalf("constant = %q, want %q", testCase.actual, testCase.want)
			}
		})
	}
}
