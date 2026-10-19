package saml

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/russellhaering/goxmldsig/etreeutils"
)

// responseDocument is a structurally validated response. It is produced before
// signature verification and never treated as trusted until
// verifyDocumentSignature succeeds.
type responseDocument struct {
	response    *etree.Element
	assertion   *etree.Element
	signature   *etree.Element
	signed      *etree.Element
	signedID    string
	assertionID string

	issuer  string
	subject string
	claims  map[string]any

	conditionsNotBefore    time.Time
	hasConditionsNotBefore bool
	conditionsNotOnOrAfter time.Time

	confirmationNotBefore    time.Time
	hasConfirmationNotBefore bool
	confirmationNotOnOrAfter time.Time
}

// verifyResponse performs parsing, structural validation, signature
// verification, and condition checks. It returns the verified assertion and
// the instant until which its ID must be remembered.
func verifyResponse(provider *Provider, input ResponseInput, now time.Time) (verifiedAssertion, time.Time, error) {
	if err := validateResponseInput(provider, input); err != nil {
		return verifiedAssertion{}, time.Time{}, err
	}
	root, err := parseXML(input.ResponseXML, maxResponseBytes)
	if err != nil {
		return verifiedAssertion{}, time.Time{}, err
	}
	document, err := parseResponseDocument(root, provider, input)
	if err != nil {
		return verifiedAssertion{}, time.Time{}, err
	}
	if err := verifyDocumentSignature(document, provider, now); err != nil {
		return verifiedAssertion{}, time.Time{}, err
	}
	expires, err := document.validateTimes(now, provider.leeway)
	if err != nil {
		return verifiedAssertion{}, time.Time{}, err
	}
	return verifiedAssertion{
		assertionID: document.assertionID,
		issuer:      provider.issuer,
		subject:     document.subject,
		claims:      document.claims,
	}, expires, nil
}

// validateResponseInput checks the values the login transaction binds before
// any XML work happens.
func validateResponseInput(provider *Provider, input ResponseInput) error {
	if !validIdentifier(input.ExpectedRequestID, maxIDLength) {
		return fmt.Errorf("%w: expected request ID is missing or malformed", ErrInvalidResponse)
	}
	if input.ACSURL != provider.acsURL {
		return fmt.Errorf("%w: assertion consumer URL does not match the configuration", ErrInvalidResponse)
	}
	return nil
}

// parseResponseDocument validates the response shape. Its central rule is that
// exactly one Assertion exists, exactly one Signature exists, and the
// Signature is a direct child of the element its Reference names: the node
// that is verified is the node that is consumed.
func parseResponseDocument(root *etree.Element, provider *Provider, input ResponseInput) (*responseDocument, error) {
	document := &responseDocument{response: root}
	if root.Tag != "Response" || !inNamespace(root, protocolNamespace) {
		return nil, invalidResponse("root element is not a SAML response")
	}
	version, err := requiredAttribute(root, "Version")
	if err != nil || version != samlVersion {
		return nil, invalidResponse("response version is not 2.0")
	}
	responseID, err := requiredAttribute(root, "ID")
	if err != nil {
		return nil, invalidResponse("response ID is missing")
	}
	destination, err := requiredAttribute(root, "Destination")
	if err != nil || destination != provider.acsURL {
		return nil, invalidResponse("response destination does not match the ACS URL")
	}
	responseInResponseTo, err := requiredAttribute(root, "InResponseTo")
	if err != nil || responseInResponseTo != input.ExpectedRequestID {
		return nil, invalidResponse("response does not answer this login")
	}

	status, err := oneChild(root, protocolNamespace, "Status")
	if err != nil {
		return nil, invalidResponse("response status is missing")
	}
	statusCode, err := oneChild(status, protocolNamespace, "StatusCode")
	if err != nil {
		return nil, invalidResponse("response status code is missing")
	}
	statusValue, err := requiredAttribute(statusCode, "Value")
	if err != nil || statusValue != statusSuccess {
		return nil, invalidResponse("response status is not success")
	}

	responseIssuer, err := oneChild(root, assertionNamespace, "Issuer")
	if err != nil {
		return nil, invalidResponse("response issuer is missing")
	}
	issuer, err := elementText(responseIssuer, "Issuer")
	if err != nil || issuer != provider.issuer {
		return nil, invalidResponse("response issuer does not match the configuration")
	}

	assertions := childElements(root, assertionNamespace, "Assertion")
	if len(assertions) != 1 {
		return nil, invalidResponse("response does not contain exactly one assertion")
	}
	if nested := elementsByTagDescendants(root, assertionNamespace, "Assertion"); len(nested) != 1 {
		return nil, invalidResponse("response contains an ambiguous assertion tree")
	}
	for _, tag := range []string{"EncryptedAssertion", "EncryptedAttribute", "EncryptedID"} {
		if len(elementsByTagDescendants(root, assertionNamespace, tag)) != 0 {
			return nil, invalidResponse("encrypted SAML content is not supported")
		}
	}
	if hasEncryptionElement(root) {
		return nil, invalidResponse("encrypted SAML content is not supported")
	}
	assertion := assertions[0]
	document.assertion = assertion

	assertionVersion, err := requiredAttribute(assertion, "Version")
	if err != nil || assertionVersion != samlVersion {
		return nil, invalidResponse("assertion version is not 2.0")
	}
	assertionID, err := requiredAttribute(assertion, "ID")
	if err != nil {
		return nil, invalidResponse("assertion ID is missing")
	}
	document.assertionID = assertionID
	document.issuer = provider.issuer

	assertionIssuer, err := oneChild(assertion, assertionNamespace, "Issuer")
	if err != nil {
		return nil, invalidResponse("assertion issuer is missing")
	}
	assertionIssuerText, err := elementText(assertionIssuer, "Issuer")
	if err != nil || assertionIssuerText != provider.issuer {
		return nil, invalidResponse("assertion issuer does not match the configuration")
	}

	signatures := elementsByTagDescendants(root, dsigNamespace, "Signature")
	if len(signatures) != 1 {
		return nil, invalidResponse("response does not contain exactly one signature")
	}
	signature := signatures[0]
	switch signature.Parent() {
	case root:
		document.signed = root
		document.signedID = responseID
	case assertion:
		document.signed = assertion
		document.signedID = assertionID
	default:
		return nil, invalidResponse("signature is not attached to the response or assertion")
	}
	document.signature = signature

	if err := parseSubject(assertion, provider, input, document); err != nil {
		return nil, err
	}
	if err := parseConditions(assertion, provider, document); err != nil {
		return nil, err
	}
	claims, err := parseClaims(assertion, provider)
	if err != nil {
		return nil, err
	}
	document.claims = claims
	if len(childElements(assertion, assertionNamespace, "AuthnStatement")) > 1 {
		return nil, invalidResponse("assertion contains more than one authentication statement")
	}
	return document, nil
}

// parseSubject validates the NameID and the bearer subject confirmation.
func parseSubject(assertion *etree.Element, provider *Provider, input ResponseInput, document *responseDocument) error {
	subjectElement, err := oneChild(assertion, assertionNamespace, "Subject")
	if err != nil {
		return invalidResponse("assertion subject is missing")
	}
	nameID, err := oneChild(subjectElement, assertionNamespace, "NameID")
	if err != nil {
		return invalidResponse("assertion NameID is missing")
	}
	format, present := noNamespaceAttribute(nameID, "Format")
	if !present || format == "" {
		format = NameIDFormatUnspecified
	}
	if !slices.Contains(provider.nameIDFormats, format) {
		return invalidResponse("NameID format is not allowed")
	}
	subject, err := elementText(nameID, "NameID")
	if err != nil || len(subject) > maxNameIDLength {
		return invalidResponse("NameID is empty or malformed")
	}
	document.subject = subject

	confirmation, err := oneChild(subjectElement, assertionNamespace, "SubjectConfirmation")
	if err != nil {
		return invalidResponse("subject confirmation is missing")
	}
	method, err := requiredAttribute(confirmation, "Method")
	if err != nil || method != bearerConfirmation {
		return invalidResponse("subject confirmation method is not bearer")
	}
	data, err := oneChild(confirmation, assertionNamespace, "SubjectConfirmationData")
	if err != nil {
		return invalidResponse("subject confirmation data is missing")
	}
	recipient, err := requiredAttribute(data, "Recipient")
	if err != nil || recipient != provider.acsURL {
		return invalidResponse("subject confirmation recipient does not match the ACS URL")
	}
	inResponseTo, err := requiredAttribute(data, "InResponseTo")
	if err != nil || inResponseTo != input.ExpectedRequestID {
		return invalidResponse("subject confirmation does not answer this login")
	}
	notOnOrAfter, err := requiredAttribute(data, "NotOnOrAfter")
	if err != nil {
		return invalidResponse("subject confirmation has no expiry")
	}
	document.confirmationNotOnOrAfter, err = parseSAMLTime(notOnOrAfter)
	if err != nil {
		return err
	}
	if notBefore, present := noNamespaceAttribute(data, "NotBefore"); present && notBefore != "" {
		document.confirmationNotBefore, err = parseSAMLTime(notBefore)
		if err != nil {
			return err
		}
		document.hasConfirmationNotBefore = true
	}
	return nil
}

// parseConditions validates the audience restriction and the assertion
// validity window.
func parseConditions(assertion *etree.Element, provider *Provider, document *responseDocument) error {
	conditions, err := oneChild(assertion, assertionNamespace, "Conditions")
	if err != nil {
		return invalidResponse("assertion conditions are missing")
	}
	notOnOrAfter, err := requiredAttribute(conditions, "NotOnOrAfter")
	if err != nil {
		return invalidResponse("assertion conditions have no expiry")
	}
	document.conditionsNotOnOrAfter, err = parseSAMLTime(notOnOrAfter)
	if err != nil {
		return err
	}
	if notBefore, present := noNamespaceAttribute(conditions, "NotBefore"); present && notBefore != "" {
		document.conditionsNotBefore, err = parseSAMLTime(notBefore)
		if err != nil {
			return err
		}
		document.hasConditionsNotBefore = true
	}

	restrictions := childElements(conditions, assertionNamespace, "AudienceRestriction")
	if len(restrictions) == 0 {
		return invalidResponse("assertion has no audience restriction")
	}
	for _, restriction := range restrictions {
		audiences := childElements(restriction, assertionNamespace, "Audience")
		if len(audiences) == 0 {
			return invalidResponse("audience restriction is empty")
		}
		matched := false
		for _, audience := range audiences {
			value, err := elementText(audience, "Audience")
			if err != nil {
				return err
			}
			if value == provider.entityID {
				matched = true
			}
		}
		if !matched {
			return invalidResponse("assertion audience does not name this service provider")
		}
	}
	return nil
}

// parseClaims maps the configured SAML attributes into claim values. Every
// configured attribute must be present; a missing attribute fails the login
// instead of being guessed.
func parseClaims(assertion *etree.Element, provider *Provider) (map[string]any, error) {
	attributes := make(map[string][]string)
	for _, statement := range childElements(assertion, assertionNamespace, "AttributeStatement") {
		for _, attribute := range childElements(statement, assertionNamespace, "Attribute") {
			name, err := requiredAttribute(attribute, "Name")
			if err != nil || !validAttributeName(name) {
				return nil, invalidResponse("assertion attribute has no valid name")
			}
			if _, duplicate := attributes[name]; duplicate {
				return nil, invalidResponse("assertion repeats an attribute name")
			}
			if len(attributes) >= maxAttributes {
				return nil, invalidResponse("assertion carries too many attributes")
			}
			values := childElements(attribute, assertionNamespace, "AttributeValue")
			if len(values) == 0 || len(values) > maxAttributeVals {
				return nil, invalidResponse("assertion attribute has no usable values")
			}
			parsed := make([]string, 0, len(values))
			for _, value := range values {
				text, err := elementText(value, "AttributeValue")
				if err != nil || len(text) > maxAttributeLen {
					return nil, invalidResponse("assertion attribute value is malformed")
				}
				parsed = append(parsed, text)
			}
			attributes[name] = parsed
		}
	}

	claims := make(map[string]any, len(provider.claimAttributes))
	for claim, attribute := range provider.claimAttributes {
		values, present := attributes[attribute]
		if !present || len(values) == 0 {
			return nil, invalidResponse("assertion is missing a configured attribute")
		}
		if len(values) == 1 {
			claims[claim] = values[0]
			continue
		}
		claims[claim] = append([]string(nil), values...)
	}
	return claims, nil
}

// verifyDocumentSignature verifies the single signature against the element
// its reference names. inspectSignature enforces the algorithm, transform, and
// reference shape first; goxmldsig then performs canonicalization and digest
// verification with the trusted certificate set.
func verifyDocumentSignature(document *responseDocument, provider *Provider, now time.Time) error {
	if err := inspectSignature(document.signature, document.signedID, provider); err != nil {
		return err
	}
	// The library canonicalizes a detached copy of the signed element. Namespace
	// declarations inherited from an ancestor -- the response above an assertion,
	// for example -- are in scope for canonicalization but are not attributes of
	// the element itself, so the copy would lose them and every signature over
	// such an assertion would fail. Materialize the in-scope declarations on the
	// copy first. This changes no namespace node: C14N renders the same in-scope
	// set whether a declaration sits on the element or on an ancestor.
	parentContext, err := etreeutils.NSBuildParentContext(document.signed)
	if err != nil {
		return invalidResponse("signature verification failed")
	}
	signed, err := etreeutils.NSDetatch(parentContext, document.signed)
	if err != nil {
		return invalidResponse("signature verification failed")
	}
	validation := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: provider.idpCertificates})
	validation.Clock = dsig.NewFakeClockAt(now)
	if _, err := validation.Validate(signed); err != nil {
		return invalidResponse("signature verification failed")
	}
	return nil
}

// inspectSignature enforces the signature shape before the library sees it:
// exactly one SignedInfo, one SignatureValue, and one Reference naming the
// signed element; SHA-2 signature and digest algorithms from the allowlist;
// and exactly the enveloped-signature plus canonicalization transform chain.
func inspectSignature(signature *etree.Element, signedID string, provider *Provider) error {
	var signedInfo *etree.Element
	signedInfoCount := 0
	signatureValueCount := 0
	keyInfoCount := 0
	for _, child := range signature.ChildElements() {
		if !inNamespace(child, dsigNamespace) {
			return invalidResponse("signature contains a foreign element")
		}
		switch child.Tag {
		case "SignedInfo":
			signedInfoCount++
			signedInfo = child
		case "SignatureValue":
			signatureValueCount++
			if len(child.ChildElements()) != 0 || strings.TrimSpace(child.Text()) == "" {
				return invalidResponse("signature value is empty")
			}
		case "KeyInfo":
			keyInfoCount++
		case "Object":
			return invalidResponse("signature contains an object")
		default:
			return invalidResponse("signature contains an unexpected element")
		}
	}
	if signedInfoCount != 1 || signatureValueCount != 1 || keyInfoCount > 1 {
		return invalidResponse("signature shape is ambiguous")
	}

	canonicalizationCount := 0
	signatureMethodCount := 0
	referenceCount := 0
	var reference *etree.Element
	for _, child := range signedInfo.ChildElements() {
		if !inNamespace(child, dsigNamespace) {
			return invalidResponse("SignedInfo contains a foreign element")
		}
		switch child.Tag {
		case "CanonicalizationMethod":
			canonicalizationCount++
			algorithm, err := requiredAttribute(child, "Algorithm")
			if err != nil {
				return invalidResponse("SignedInfo canonicalization method is missing")
			}
			if _, ok := allowedCanonicalizations[algorithm]; !ok {
				return invalidResponse("SignedInfo canonicalization algorithm is not allowed")
			}
		case "SignatureMethod":
			signatureMethodCount++
			algorithm, err := requiredAttribute(child, "Algorithm")
			if err != nil {
				return invalidResponse("signature method is missing")
			}
			if _, ok := provider.signatureAlgSet[algorithm]; !ok {
				return invalidResponse("signature algorithm is not allowed")
			}
		case "Reference":
			referenceCount++
			reference = child
		default:
			return invalidResponse("SignedInfo contains an unexpected element")
		}
	}
	if canonicalizationCount != 1 || signatureMethodCount != 1 || referenceCount != 1 {
		return invalidResponse("SignedInfo shape is ambiguous")
	}

	uri, err := requiredAttribute(reference, "URI")
	if err != nil || uri != "#"+signedID {
		return invalidResponse("signature reference does not name the signed element")
	}
	transformsCount := 0
	digestMethodCount := 0
	digestValueCount := 0
	for _, child := range reference.ChildElements() {
		if !inNamespace(child, dsigNamespace) {
			return invalidResponse("signature reference contains a foreign element")
		}
		switch child.Tag {
		case "Transforms":
			transformsCount++
			transforms := childElements(child, dsigNamespace, "Transform")
			if len(transforms) != 2 {
				return invalidResponse("signature reference transform chain is not allowed")
			}
			first, err := requiredAttribute(transforms[0], "Algorithm")
			if err != nil || first != envelopedSignature {
				return invalidResponse("signature reference does not start with the enveloped transform")
			}
			second, err := requiredAttribute(transforms[1], "Algorithm")
			if err != nil {
				return invalidResponse("signature reference canonicalization is missing")
			}
			if _, ok := allowedCanonicalizations[second]; !ok {
				return invalidResponse("signature reference canonicalization is not allowed")
			}
		case "DigestMethod":
			digestMethodCount++
			algorithm, err := requiredAttribute(child, "Algorithm")
			if err != nil {
				return invalidResponse("digest method is missing")
			}
			if _, ok := allowedDigestAlgorithms[algorithm]; !ok {
				return invalidResponse("digest algorithm is not allowed")
			}
		case "DigestValue":
			digestValueCount++
			if len(child.ChildElements()) != 0 || strings.TrimSpace(child.Text()) == "" {
				return invalidResponse("digest value is empty")
			}
		default:
			return invalidResponse("signature reference contains an unexpected element")
		}
	}
	if transformsCount != 1 || digestMethodCount != 1 || digestValueCount != 1 {
		return invalidResponse("signature reference shape is ambiguous")
	}
	return nil
}

// validateTimes applies the condition windows with the configured leeway and
// returns the instant the assertion ID must stay in the replay cache.
func (d *responseDocument) validateTimes(now time.Time, leeway time.Duration) (time.Time, error) {
	if d.hasConditionsNotBefore {
		if d.conditionsNotOnOrAfter.Before(d.conditionsNotBefore) {
			return time.Time{}, invalidResponse("assertion validity window is inverted")
		}
		if now.Before(d.conditionsNotBefore.Add(-leeway)) {
			return time.Time{}, invalidResponse("assertion is not yet valid")
		}
	}
	if !now.Before(d.conditionsNotOnOrAfter.Add(leeway)) {
		return time.Time{}, invalidResponse("assertion has expired")
	}
	if d.hasConfirmationNotBefore && now.Before(d.confirmationNotBefore.Add(-leeway)) {
		return time.Time{}, invalidResponse("subject confirmation is not yet valid")
	}
	if !now.Before(d.confirmationNotOnOrAfter.Add(leeway)) {
		return time.Time{}, invalidResponse("subject confirmation has expired")
	}
	expires := d.conditionsNotOnOrAfter
	if d.confirmationNotOnOrAfter.Before(expires) {
		expires = d.confirmationNotOnOrAfter
	}
	return expires.Add(leeway), nil
}

// hasEncryptionElement reports whether any XML encryption element appears.
// Encryption is out of scope, so an encrypted assertion or attribute is a
// rejection rather than something to skip.
func hasEncryptionElement(root *etree.Element) bool {
	var found bool
	var walk func(element *etree.Element)
	walk = func(element *etree.Element) {
		for _, child := range element.ChildElements() {
			if child.Tag == "EncryptedData" || child.Tag == "EncryptedKey" {
				found = true
			}
			walk(child)
		}
	}
	walk(root)
	return found
}

// invalidResponse builds the static error every structural failure returns.
func invalidResponse(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidResponse, reason)
}
