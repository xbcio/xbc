package saml

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/xbcio/xbc/extensions/authentication/federation"
)

// Fixed fixture values shared by the tests. The clock is fixed so every
// validity window is deterministic.
var (
	testNow         = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	testRequestID   = "_request0123456789abcdef"
	testResponseID  = "_response0123456789abcdef"
	testAssertionID = "_assertion0123456789abcdef"
	testNameID      = "user-12345"
	testMail        = "alice@example.com"
	testDisplayName = "Alice Example"

	testCertificateSerial int64
)

// Signing keys and certificates are package-level test handles, generated once
// in TestMain. Rotated is the certificate an IdP publishes during a rotation;
// untrusted signed a valid-looking assertion that must never be accepted.
var (
	idpKey               *rsa.PrivateKey
	idpCertificate       *x509.Certificate
	rotatedKey           *rsa.PrivateKey
	rotatedCertificate   *x509.Certificate
	untrustedKey         *rsa.PrivateKey
	untrustedCertificate *x509.Certificate
	spKey                *rsa.PrivateKey
	spCertificate        *x509.Certificate
)

func TestMain(m *testing.M) {
	idpKey, idpCertificate = mustTestKeyPair("idp")
	rotatedKey, rotatedCertificate = mustTestKeyPair("idp-rotated")
	untrustedKey, untrustedCertificate = mustTestKeyPair("untrusted")
	spKey, spCertificate = mustTestKeyPair("sp")
	os.Exit(m.Run())
}

// mustTestKeyPair generates one self-signed RSA-2048 key pair valid around
// testNow.
func mustTestKeyPair(name string) (*rsa.PrivateKey, *x509.Certificate) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(fmt.Sprintf("generate %s key: %v", name, err))
	}
	testCertificateSerial++
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(testCertificateSerial),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             testNow.Add(-24 * time.Hour),
		NotAfter:              testNow.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		panic(fmt.Sprintf("create %s certificate: %v", name, err))
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		panic(fmt.Sprintf("parse %s certificate: %v", name, err))
	}
	return key, certificate
}

// testKeyStore adapts a key and certificate to the goxmldsig X509KeyStore
// interface so the test signer can embed its certificate in KeyInfo.
type testKeyStore struct {
	key         *rsa.PrivateKey
	certificate []byte
}

func (s testKeyStore) GetKeyPair() (*rsa.PrivateKey, []byte, error) {
	return s.key, s.certificate, nil
}

// testConfig is the valid baseline configuration every test starts from.
func testConfig() Config {
	return Config{
		Name:            "corp",
		Issuer:          "https://idp.example.com/entity",
		SSOURL:          "https://idp.example.com/sso",
		EntityID:        "https://sp.example.com/entity",
		ACSURL:          "https://sp.example.com/acs",
		IdPCertificates: []*x509.Certificate{idpCertificate},
		NameIDFormats:   []string{NameIDFormatPersistent},
		ClaimAttributes: map[string]string{
			"email":        "mail",
			"display_name": "displayName",
		},
		Now: func() time.Time { return testNow },
	}
}

// newTestProvider builds a provider from testConfig with optional mutations.
func newTestProvider(t *testing.T, mutate func(*Config)) *Provider {
	t.Helper()
	config := testConfig()
	if mutate != nil {
		mutate(&config)
	}
	provider, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

// testReplayCache is a controllable ReplayCache used to isolate Verify from
// the in-memory implementation.
type testReplayCache struct {
	mu     sync.Mutex
	claims map[string]struct{}
	err    error
}

func (c *testReplayCache) Claim(_ context.Context, id string, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	if c.claims == nil {
		c.claims = make(map[string]struct{})
	}
	if _, exists := c.claims[id]; exists {
		return ErrReplay
	}
	c.claims[id] = struct{}{}
	return nil
}

// responseOptions describes one synthetic SAML response. The zero value is
// replaced by defaultResponseOptions in buildResponseDocument.
type responseOptions struct {
	responseID           string
	assertionID          string
	responseInResponseTo string
	destination          string
	statusCode           string
	issuer               string
	assertionIssuer      string
	issueInstant         time.Time

	nameID           string
	nameIDFormat     string
	omitNameIDFormat bool
	omitNameID       bool

	subjectConfirmationMethod string
	recipient                 string
	confirmationInResponseTo  string
	confirmationNotBefore     *time.Time
	confirmationNotOnOrAfter  *time.Time

	conditionsNotBefore    *time.Time
	conditionsNotOnOrAfter *time.Time
	audience               string

	attributes []testAttribute

	omitResponseIssuer      bool
	omitAssertionIssuer     bool
	omitStatus              bool
	omitAssertion           bool
	omitConditions          bool
	omitAudienceRestriction bool
	omitSubject             bool
	omitSubjectConfirmation bool
	omitAttributeStatement  bool
	extraResponseXML        string
	extraAssertionXML       string

	signResponse       bool
	omitSignature      bool
	signingKey         *rsa.PrivateKey
	signingCertificate *x509.Certificate
	hash               crypto.Hash
}

type testAttribute struct {
	name   string
	values []string
}

// defaultResponseOptions returns a valid response bound to testRequestID.
func defaultResponseOptions() responseOptions {
	notBefore := testNow.Add(-5 * time.Minute)
	notOnOrAfter := testNow.Add(5 * time.Minute)
	return responseOptions{
		responseID:                testResponseID,
		assertionID:               testAssertionID,
		responseInResponseTo:      testRequestID,
		destination:               testConfig().ACSURL,
		statusCode:                statusSuccess,
		issuer:                    testConfig().Issuer,
		assertionIssuer:           testConfig().Issuer,
		issueInstant:              testNow,
		nameID:                    testNameID,
		nameIDFormat:              NameIDFormatPersistent,
		subjectConfirmationMethod: bearerConfirmation,
		recipient:                 testConfig().ACSURL,
		confirmationInResponseTo:  testRequestID,
		confirmationNotBefore:     &notBefore,
		confirmationNotOnOrAfter:  &notOnOrAfter,
		conditionsNotBefore:       &notBefore,
		conditionsNotOnOrAfter:    &notOnOrAfter,
		audience:                  testConfig().EntityID,
		attributes: []testAttribute{
			{name: "mail", values: []string{testMail}},
			{name: "displayName", values: []string{testDisplayName}},
		},
		signingKey:         idpKey,
		signingCertificate: idpCertificate,
	}
}

// buildResponseDocument renders and parses the unsigned document described by
// mutate applied to the defaults.
func buildResponseDocument(t *testing.T, mutate func(*responseOptions)) (*etree.Document, responseOptions) {
	t.Helper()
	options := defaultResponseOptions()
	if mutate != nil {
		mutate(&options)
	}
	document := etree.NewDocument()
	document.ReadSettings = etree.ReadSettings{PreserveDuplicateAttrs: true}
	if err := document.ReadFromString(options.xml()); err != nil {
		t.Fatalf("parse test response: %v", err)
	}
	return document, options
}

// buildSignedResponse renders, signs, and serializes the described response.
func buildSignedResponse(t *testing.T, mutate func(*responseOptions)) []byte {
	t.Helper()
	document, options := buildResponseDocument(t, mutate)
	if !options.omitSignature {
		target := findTestElement(t, document.Root(), assertionNamespace, "Assertion")
		if options.signResponse {
			target = document.Root()
		}
		signed := signTestElement(t, target, options)
		attachSigned(t, document, target, signed)
	}
	return serializeTestDocument(t, document)
}

// signTestElement signs target with the configured key, hash, and certificate.
func signTestElement(t *testing.T, target *etree.Element, options responseOptions) *etree.Element {
	t.Helper()
	context := dsig.NewDefaultSigningContext(testKeyStore{
		key:         options.signingKey,
		certificate: options.signingCertificate.Raw,
	})
	if options.hash != 0 {
		context.Hash = options.hash
	}
	signed, err := context.SignEnveloped(target)
	if err != nil {
		t.Fatalf("sign test element: %v", err)
	}
	// SignEnveloped appends the Signature with a raw slice append, leaving its
	// cached parent and index stale. Re-attaching it through AddChild makes the
	// signed document well-formed for validation.
	for position, child := range signed.ChildElements() {
		if child.Tag == "Signature" && child.NamespaceURI() == dsigNamespace {
			signed.RemoveChildAt(position)
			signed.AddChild(child)
			break
		}
	}
	return signed
}

// attachSigned replaces target in its document with the signed copy.
func attachSigned(t *testing.T, document *etree.Document, target, signed *etree.Element) {
	t.Helper()
	parent := target.Parent()
	if parent == nil {
		document.SetRoot(signed)
		return
	}
	index := target.Index()
	parent.RemoveChildAt(index)
	parent.InsertChildAt(index, signed)
}

// serializeTestDocument renders a document for Verify.
func serializeTestDocument(t *testing.T, document *etree.Document) []byte {
	t.Helper()
	data, err := document.WriteToBytes()
	if err != nil {
		t.Fatalf("serialize test document: %v", err)
	}
	return data
}

// findTestElement returns the single descendant in the given namespace with
// the tag, failing the test when it is absent or ambiguous.
func findTestElement(t *testing.T, root *etree.Element, namespace, tag string) *etree.Element {
	t.Helper()
	var matches []*etree.Element
	var walk func(*etree.Element)
	walk = func(element *etree.Element) {
		for _, child := range element.ChildElements() {
			if child.Tag == tag && child.NamespaceURI() == namespace {
				matches = append(matches, child)
			}
			walk(child)
		}
	}
	walk(root)
	if len(matches) != 1 {
		t.Fatalf("expected exactly one %s element, found %d", tag, len(matches))
	}
	return matches[0]
}

// firstTestElement returns the first descendant in the given namespace with
// the tag, for fixtures that legitimately repeat an element.
func firstTestElement(t *testing.T, root *etree.Element, namespace, tag string) *etree.Element {
	t.Helper()
	var found *etree.Element
	var walk func(*etree.Element)
	walk = func(element *etree.Element) {
		for _, child := range element.ChildElements() {
			if found == nil && child.Tag == tag && child.NamespaceURI() == namespace {
				found = child
			}
			walk(child)
		}
	}
	walk(root)
	if found == nil {
		t.Fatalf("no %s element found", tag)
	}
	return found
}

// xml renders the response document.
func (o responseOptions) xml() string {
	var builder strings.Builder
	builder.WriteString(`<samlp:Response xmlns:samlp="` + protocolNamespace + `" xmlns:saml="` + assertionNamespace + `"`)
	fmt.Fprintf(&builder, ` ID="%s" Version="2.0" IssueInstant="%s"`, o.responseID, o.issueInstant.UTC().Format(time.RFC3339))
	if o.destination != "" {
		fmt.Fprintf(&builder, ` Destination="%s"`, o.destination)
	}
	if o.responseInResponseTo != "" {
		fmt.Fprintf(&builder, ` InResponseTo="%s"`, o.responseInResponseTo)
	}
	builder.WriteString(">")
	if !o.omitResponseIssuer {
		fmt.Fprintf(&builder, `<saml:Issuer>%s</saml:Issuer>`, o.issuer)
	}
	if !o.omitStatus {
		fmt.Fprintf(&builder, `<samlp:Status><samlp:StatusCode Value="%s"/></samlp:Status>`, o.statusCode)
	}
	if !o.omitAssertion {
		builder.WriteString(o.assertionXML())
	}
	builder.WriteString(o.extraResponseXML)
	builder.WriteString(`</samlp:Response>`)
	return builder.String()
}

func (o responseOptions) assertionXML() string {
	var builder strings.Builder
	// The assertion repeats the namespace declarations so this fixture covers
	// the self-contained form most IdPs emit. Verification also accepts an
	// assertion that inherits both declarations from the Response; that form
	// has its own test.
	fmt.Fprintf(&builder, `<saml:Assertion xmlns:saml="%s" xmlns:samlp="%s" ID="%s" Version="2.0" IssueInstant="%s">`,
		assertionNamespace, protocolNamespace, o.assertionID, o.issueInstant.UTC().Format(time.RFC3339))
	if !o.omitAssertionIssuer {
		fmt.Fprintf(&builder, `<saml:Issuer>%s</saml:Issuer>`, o.assertionIssuer)
	}
	if !o.omitSubject {
		builder.WriteString(`<saml:Subject>`)
		if !o.omitNameID {
			if o.omitNameIDFormat {
				fmt.Fprintf(&builder, `<saml:NameID>%s</saml:NameID>`, o.nameID)
			} else {
				fmt.Fprintf(&builder, `<saml:NameID Format="%s">%s</saml:NameID>`, o.nameIDFormat, o.nameID)
			}
		}
		if !o.omitSubjectConfirmation {
			fmt.Fprintf(&builder, `<saml:SubjectConfirmation Method="%s">`, o.subjectConfirmationMethod)
			builder.WriteString(`<saml:SubjectConfirmationData`)
			if o.recipient != "" {
				fmt.Fprintf(&builder, ` Recipient="%s"`, o.recipient)
			}
			if o.confirmationInResponseTo != "" {
				fmt.Fprintf(&builder, ` InResponseTo="%s"`, o.confirmationInResponseTo)
			}
			if o.confirmationNotBefore != nil {
				fmt.Fprintf(&builder, ` NotBefore="%s"`, o.confirmationNotBefore.UTC().Format(time.RFC3339))
			}
			if o.confirmationNotOnOrAfter != nil {
				fmt.Fprintf(&builder, ` NotOnOrAfter="%s"`, o.confirmationNotOnOrAfter.UTC().Format(time.RFC3339))
			}
			builder.WriteString(`/></saml:SubjectConfirmation>`)
		}
		builder.WriteString(`</saml:Subject>`)
	}
	if !o.omitConditions {
		builder.WriteString(`<saml:Conditions`)
		if o.conditionsNotBefore != nil {
			fmt.Fprintf(&builder, ` NotBefore="%s"`, o.conditionsNotBefore.UTC().Format(time.RFC3339))
		}
		if o.conditionsNotOnOrAfter != nil {
			fmt.Fprintf(&builder, ` NotOnOrAfter="%s"`, o.conditionsNotOnOrAfter.UTC().Format(time.RFC3339))
		}
		builder.WriteString(`>`)
		if !o.omitAudienceRestriction {
			fmt.Fprintf(&builder, `<saml:AudienceRestriction><saml:Audience>%s</saml:Audience></saml:AudienceRestriction>`, o.audience)
		}
		builder.WriteString(`</saml:Conditions>`)
	}
	fmt.Fprintf(&builder, `<saml:AuthnStatement AuthnInstant="%s" SessionIndex="session-1"><saml:AuthnContext><saml:AuthnContextClassRef>urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport</saml:AuthnContextClassRef></saml:AuthnContext></saml:AuthnStatement>`, o.issueInstant.UTC().Format(time.RFC3339))
	if !o.omitAttributeStatement {
		builder.WriteString(`<saml:AttributeStatement>`)
		for _, attribute := range o.attributes {
			fmt.Fprintf(&builder, `<saml:Attribute Name="%s">`, attribute.name)
			for _, value := range attribute.values {
				fmt.Fprintf(&builder, `<saml:AttributeValue>%s</saml:AttributeValue>`, value)
			}
			builder.WriteString(`</saml:Attribute>`)
		}
		builder.WriteString(`</saml:AttributeStatement>`)
	}
	builder.WriteString(o.extraAssertionXML)
	builder.WriteString(`</saml:Assertion>`)
	return builder.String()
}

// responseInput builds the callback input for a response bound to the default
// request and ACS values.
func responseInput(response []byte) ResponseInput {
	return ResponseInput{
		ResponseXML:       response,
		ExpectedRequestID: testRequestID,
		ACSURL:            testConfig().ACSURL,
	}
}

// mustVerify asserts that Verify accepts the response and returns the identity.
func mustVerify(t *testing.T, provider *Provider, input ResponseInput) federation.ExternalIdentity {
	t.Helper()
	identity, err := Verify(t.Context(), provider, input)
	if err != nil {
		t.Fatalf("Verify: unexpected error: %v", err)
	}
	return identity
}

// mustRejectResponse asserts that Verify fails with want and that the error
// carries none of the fixture's sensitive values.
func mustRejectResponse(t *testing.T, provider *Provider, input ResponseInput, want error) error {
	t.Helper()
	identity, err := Verify(t.Context(), provider, input)
	if !errors.Is(err, want) {
		t.Fatalf("Verify: error = %v, want %v", err, want)
	}
	if identity.Subject != "" || identity.Claims != nil {
		t.Fatalf("Verify returned a partial identity on failure: %+v", identity)
	}
	assertNoSensitiveText(t, err.Error())
	return err
}

// newTestDocument parses fixture XML without the production hardening, so
// tests can build documents the parser would otherwise reject.
func newTestDocument(t *testing.T, data []byte) *etree.Document {
	t.Helper()
	document := etree.NewDocument()
	if err := document.ReadFromBytes(data); err != nil {
		t.Fatalf("parse fixture document: %v", err)
	}
	return document
}

// assertNoSensitiveText checks that a diagnostic carries no assertion content,
// NameID, attribute value, certificate, or key material.
func assertNoSensitiveText(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{
		testNameID,
		testMail,
		testDisplayName,
		"BEGIN PRIVATE KEY",
		"BEGIN RSA PRIVATE KEY",
		"MIIC",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("diagnostic %q contains sensitive value %q", text, secret)
		}
	}
}
