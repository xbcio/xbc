package saml

import (
	"errors"
	"testing"

	"github.com/beevik/etree"
)

// signedResponseWithSignatureMutation signs a valid response and then applies
// mutate to the ds:Signature element. The signature no longer verifies, so a
// rejection proves the structural inspector ran before cryptography only when
// the shape itself is the violated rule; every case below violates one.
func signedResponseWithSignatureMutation(t *testing.T, mutate func(*etree.Element)) []byte {
	t.Helper()
	document, options := buildResponseDocument(t, nil)
	assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
	signed := signTestElement(t, assertion, options)
	mutate(signatureOf(t, signed))
	attachSigned(t, document, assertion, signed)
	return serializeTestDocument(t, document)
}

func signedInfoOf(t *testing.T, signature *etree.Element) *etree.Element {
	t.Helper()
	children := directChildren(signature, dsigNamespace, "SignedInfo")
	if len(children) != 1 {
		t.Fatalf("expected one SignedInfo, found %d", len(children))
	}
	return children[0]
}

func TestVerifyRejectsMalformedSignatureShapes(t *testing.T) {
	cases := map[string]func(*testing.T, *etree.Element){
		"missing signed info": func(t *testing.T, signature *etree.Element) {
			signature.RemoveChild(signedInfoOf(t, signature))
		},
		"two signed infos": func(t *testing.T, signature *etree.Element) {
			signature.AddChild(signedInfoOf(t, signature).Copy())
		},
		"two key infos": func(t *testing.T, signature *etree.Element) {
			keyInfo := directChildren(signature, dsigNamespace, "KeyInfo")[0]
			signature.AddChild(keyInfo.Copy())
		},
		"empty signature value": func(t *testing.T, signature *etree.Element) {
			directChildren(signature, dsigNamespace, "SignatureValue")[0].SetText("")
		},
		"missing canonicalization method": func(t *testing.T, signature *etree.Element) {
			signedInfo := signedInfoOf(t, signature)
			signedInfo.RemoveChild(directChildren(signedInfo, dsigNamespace, "CanonicalizationMethod")[0])
		},
		"disallowed canonicalization method": func(t *testing.T, signature *etree.Element) {
			signedInfo := signedInfoOf(t, signature)
			directChildren(signedInfo, dsigNamespace, "CanonicalizationMethod")[0].CreateAttr("Algorithm", "urn:example:c14n")
		},
		"missing signature method": func(t *testing.T, signature *etree.Element) {
			signedInfo := signedInfoOf(t, signature)
			signedInfo.RemoveChild(directChildren(signedInfo, dsigNamespace, "SignatureMethod")[0])
		},
		"sha1 signature method": func(t *testing.T, signature *etree.Element) {
			signedInfo := signedInfoOf(t, signature)
			directChildren(signedInfo, dsigNamespace, "SignatureMethod")[0].CreateAttr("Algorithm", "http://www.w3.org/2000/09/xmldsig#rsa-sha1")
		},
		"two references": func(t *testing.T, signature *etree.Element) {
			signedInfo := signedInfoOf(t, signature)
			signedInfo.AddChild(referenceElement(t, signature).Copy())
		},
		"unexpected element in signed info": func(t *testing.T, signature *etree.Element) {
			signedInfo := signedInfoOf(t, signature)
			unexpected := signedInfo.CreateElement("ds:Object")
			unexpected.Space = "ds"
			unexpected.CreateAttr("xmlns:ds", dsigNamespace)
		},
		"missing reference uri": func(t *testing.T, signature *etree.Element) {
			referenceElement(t, signature).RemoveAttr("URI")
		},
		"missing transforms": func(t *testing.T, signature *etree.Element) {
			reference := referenceElement(t, signature)
			reference.RemoveChild(directChildren(reference, dsigNamespace, "Transforms")[0])
		},
		"wrong first transform": func(t *testing.T, signature *etree.Element) {
			reference := referenceElement(t, signature)
			transforms := directChildren(reference, dsigNamespace, "Transforms")[0]
			directChildren(transforms, dsigNamespace, "Transform")[0].CreateAttr("Algorithm", c14n11)
		},
		"disallowed second transform": func(t *testing.T, signature *etree.Element) {
			reference := referenceElement(t, signature)
			transforms := directChildren(reference, dsigNamespace, "Transforms")[0]
			directChildren(transforms, dsigNamespace, "Transform")[1].CreateAttr("Algorithm", "urn:example:c14n")
		},
		"missing digest method": func(t *testing.T, signature *etree.Element) {
			reference := referenceElement(t, signature)
			reference.RemoveChild(directChildren(reference, dsigNamespace, "DigestMethod")[0])
		},
		"sha1 digest method": func(t *testing.T, signature *etree.Element) {
			reference := referenceElement(t, signature)
			directChildren(reference, dsigNamespace, "DigestMethod")[0].CreateAttr("Algorithm", "http://www.w3.org/2000/09/xmldsig#sha1")
		},
		"empty digest value": func(t *testing.T, signature *etree.Element) {
			reference := referenceElement(t, signature)
			directChildren(reference, dsigNamespace, "DigestValue")[0].SetText("")
		},
		"unexpected element in reference": func(t *testing.T, signature *etree.Element) {
			reference := referenceElement(t, signature)
			unexpected := reference.CreateElement("ds:Object")
			unexpected.Space = "ds"
			unexpected.CreateAttr("xmlns:ds", dsigNamespace)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			provider := newTestProvider(t, nil)
			response := signedResponseWithSignatureMutation(t, func(signature *etree.Element) {
				mutate(t, signature)
			})
			mustRejectResponse(t, provider, responseInput(response), ErrInvalidResponse)
		})
	}
}

// TestInspectSignatureRejectsDisallowedShapes exercises the inspector directly
// so each allowlist is proven on its own: in a full Verify the signature
// library would mask several of these rejections.
func TestInspectSignatureRejectsDisallowedShapes(t *testing.T) {
	cases := map[string]func(*testing.T, *etree.Element){
		"single transform": func(t *testing.T, signature *etree.Element) {
			transforms := directChildren(referenceElement(t, signature), dsigNamespace, "Transforms")[0]
			transforms.RemoveChild(directChildren(transforms, dsigNamespace, "Transform")[1])
		},
		"two key infos": func(t *testing.T, signature *etree.Element) {
			signature.AddChild(directChildren(signature, dsigNamespace, "KeyInfo")[0].Copy())
		},
		"empty signature value": func(t *testing.T, signature *etree.Element) {
			directChildren(signature, dsigNamespace, "SignatureValue")[0].SetText("")
		},
		"object as signature child": func(t *testing.T, signature *etree.Element) {
			object := signature.CreateElement("ds:Object")
			object.Space = "ds"
			object.CreateAttr("xmlns:ds", dsigNamespace)
		},
		"unexpected element in signed info": func(t *testing.T, signature *etree.Element) {
			unexpected := signedInfoOf(t, signature).CreateElement("ds:Object")
			unexpected.Space = "ds"
			unexpected.CreateAttr("xmlns:ds", dsigNamespace)
		},
		"unexpected element in reference": func(t *testing.T, signature *etree.Element) {
			unexpected := referenceElement(t, signature).CreateElement("ds:Object")
			unexpected.Space = "ds"
			unexpected.CreateAttr("xmlns:ds", dsigNamespace)
		},
		"disallowed signed info canonicalization": func(t *testing.T, signature *etree.Element) {
			directChildren(signedInfoOf(t, signature), dsigNamespace, "CanonicalizationMethod")[0].CreateAttr("Algorithm", "urn:example:c14n")
		},
		"disallowed reference canonicalization": func(t *testing.T, signature *etree.Element) {
			transforms := directChildren(referenceElement(t, signature), dsigNamespace, "Transforms")[0]
			directChildren(transforms, dsigNamespace, "Transform")[1].CreateAttr("Algorithm", "urn:example:c14n")
		},
		"empty digest value": func(t *testing.T, signature *etree.Element) {
			directChildren(referenceElement(t, signature), dsigNamespace, "DigestValue")[0].SetText("")
		},
		"sha1 signature method with sha256 digest": func(t *testing.T, signature *etree.Element) {
			signedInfo := signedInfoOf(t, signature)
			directChildren(signedInfo, dsigNamespace, "SignatureMethod")[0].CreateAttr("Algorithm", "http://www.w3.org/2000/09/xmldsig#rsa-sha1")
		},
		"sha1 digest with sha256 signature method": func(t *testing.T, signature *etree.Element) {
			directChildren(referenceElement(t, signature), dsigNamespace, "DigestMethod")[0].CreateAttr("Algorithm", "http://www.w3.org/2000/09/xmldsig#sha1")
		},
		"reference names another element": func(t *testing.T, signature *etree.Element) {
			referenceElement(t, signature).CreateAttr("URI", "#_another-element")
		},
		"wrong first transform": func(t *testing.T, signature *etree.Element) {
			transforms := directChildren(referenceElement(t, signature), dsigNamespace, "Transforms")[0]
			directChildren(transforms, dsigNamespace, "Transform")[0].CreateAttr("Algorithm", c14n11)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			document, options := buildResponseDocument(t, nil)
			assertion := directChildren(document.Root(), assertionNamespace, "Assertion")[0]
			signed := signTestElement(t, assertion, options)
			signature := signatureOf(t, signed)
			mutate(t, signature)
			provider := newTestProvider(t, nil)
			if err := inspectSignature(signature, testAssertionID, provider); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("inspectSignature = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestProviderName(t *testing.T) {
	provider := newTestProvider(t, nil)
	if provider.Name() != "corp" {
		t.Fatalf("Name = %q, want corp", provider.Name())
	}
	var nilProvider *Provider
	if nilProvider.Name() != "" {
		t.Fatalf("nil provider Name = %q, want empty", nilProvider.Name())
	}
}
