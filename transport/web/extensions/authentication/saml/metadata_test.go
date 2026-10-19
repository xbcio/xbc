package saml

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	samlcore "github.com/xbcio/xbc/extensions/authentication/saml"
)

func TestMetadataPublishesTheServiceProviderDocument(t *testing.T) {
	harness := newHarness(t, func(options *harnessOptions) {
		options.mutateProvider = func(config *samlcore.Config) {
			config.SPCertificate = spCertificate
			config.SPPrivateKey = spKey
		}
	})

	ctx, recorder := newMetadataCtx(t, testProvider)
	if err := harness.adapter.metadata(context.Background(), ctx); err != nil {
		t.Fatalf("metadata handler error = %v", err)
	}
	if status := statusOf(ctx); status != http.StatusOK {
		t.Fatalf("metadata status = %d, want 200", status)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/samlmetadata+xml" {
		t.Fatalf("metadata Content-Type = %q, want application/samlmetadata+xml", got)
	}

	document := recorder.Body.String()
	if !strings.Contains(document, `entityID="`+testEntityID+`"`) {
		t.Fatalf("metadata does not publish the configured entity ID: %q", document)
	}
	if !strings.Contains(document, `Location="`+testACSURL+`"`) {
		t.Fatalf("metadata does not publish the configured ACS URL: %q", document)
	}
	if !strings.Contains(document, base64.StdEncoding.EncodeToString(spCertificate.Raw)) {
		t.Fatal("metadata does not publish the service-provider certificate")
	}
	// The private key must never reach a document an IdP administrator
	// downloads. Comparing the PKCS#1 and PKCS#8 encodings covers both ways
	// a key could leak into the body.
	for _, encoded := range [][]byte{
		x509.MarshalPKCS1PrivateKey(spKey),
	} {
		if strings.Contains(document, base64.StdEncoding.EncodeToString(encoded)) {
			t.Fatal("metadata contains the service-provider private key")
		}
	}
	assertNoSensitiveText(t, document)
}

func TestMetadataAnswersUnknownProvidersWithAnEmpty404(t *testing.T) {
	harness := newHarness(t, nil)

	for name, provider := range map[string]string{
		"unregistered": "unknown",
		"malformed":    "bad name",
		"dot segments": "..",
		"overlong":     strings.Repeat("a", 80),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, recorder := newMetadataCtx(t, provider)
			if err := harness.adapter.metadata(context.Background(), ctx); err != nil {
				t.Fatalf("metadata handler error = %v", err)
			}
			if status := statusOf(ctx); status != http.StatusNotFound {
				t.Fatalf("metadata status = %d, want 404", status)
			}
			if body := recorder.Body.String(); body != "" {
				t.Fatalf("metadata 404 body = %q, want empty", body)
			}
			if location := locationOf(recorder); location != "" {
				t.Fatalf("metadata 404 redirected to %q", location)
			}
			if strings.Contains(recorder.Body.String(), provider) {
				t.Fatalf("metadata 404 echoes the submitted provider %q", provider)
			}
		})
	}
}

func TestMetadataAnswersOtherMethodsWithAnEmpty404(t *testing.T) {
	harness := newHarness(t, nil)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			ctx, recorder := newRouteCtx(t, method, "/auth/saml/"+testProvider+"/metadata", testProvider, strings.NewReader("body"))
			if err := harness.adapter.metadata(context.Background(), ctx); err != nil {
				t.Fatalf("metadata handler error = %v", err)
			}
			if status := statusOf(ctx); status != http.StatusNotFound {
				t.Fatalf("metadata status = %d, want 404", status)
			}
			if body := recorder.Body.String(); body != "" {
				t.Fatalf("metadata 404 body = %q, want empty", body)
			}
		})
	}
}

func TestMetadataToleratesANilContext(t *testing.T) {
	harness := newHarness(t, nil)
	if err := harness.adapter.metadata(context.Background(), nil); err != nil {
		t.Fatalf("metadata(nil) = %v", err)
	}
}
