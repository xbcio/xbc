package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

type stubExtractor struct {
	scheme authentication.Scheme
	result authentication.CredentialResult
	err    error
	calls  int
}

func (s *stubExtractor) Scheme() authentication.Scheme { return s.scheme }

func (s *stubExtractor) ExtractCredential(*web.Ctx) (authentication.CredentialResult, error) {
	s.calls++
	return s.result, s.err
}

func TestNewExtractorIndexRejectsDuplicateScheme(t *testing.T) {
	t.Parallel()

	entries := []plugin.Entry[web.CredentialExtractor]{
		{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubExtractor{scheme: "jwt"}},
		{Identity: plugin.Identity{Plugin: "jwt-alt"}, Value: &stubExtractor{scheme: "jwt"}},
	}

	_, err := web.NewExtractorIndex(entries)
	if err == nil {
		t.Fatal("newExtractorIndex() error = nil, want duplicate scheme error")
	}
	if !strings.Contains(err.Error(), `"jwt"`) {
		t.Fatalf("error = %q, want substring \"jwt\"", err)
	}
}

func TestNewExtractorIndexRejectsInvalidScheme(t *testing.T) {
	t.Parallel()

	tests := map[string]authentication.Scheme{
		"empty":     "",
		"uppercase": "JWT",
	}

	for name, scheme := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			entries := []plugin.Entry[web.CredentialExtractor]{
				{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubExtractor{scheme: scheme}},
			}

			_, err := web.NewExtractorIndex(entries)
			if err == nil {
				t.Fatal("newExtractorIndex() error = nil, want invalid scheme error")
			}
		})
	}
}

func TestRequestCredentialSourceDelegatesToExtractor(t *testing.T) {
	t.Parallel()

	extractor := &stubExtractor{scheme: "jwt", result: authentication.Presented("token")}
	source := web.NewRequestCredentialSource(
		credentialCtx(),
		map[authentication.Scheme]web.CredentialExtractor{"jwt": extractor},
	)

	result, err := source.Credential(context.Background(), "jwt")
	if err != nil {
		t.Fatalf("Credential() error = %v", err)
	}
	if result.Status() != authentication.CredentialStatusPresented {
		t.Fatalf("Credential() status = %v, want presented", result.Status())
	}
	if extractor.calls != 1 {
		t.Fatalf("extractor calls = %d, want 1", extractor.calls)
	}
}

func TestRequestCredentialSourceReportsMissingExtractor(t *testing.T) {
	t.Parallel()

	source := web.NewRequestCredentialSource(
		credentialCtx(),
		map[authentication.Scheme]web.CredentialExtractor{},
	)

	_, err := source.Credential(context.Background(), "jwt")
	if err == nil {
		t.Fatal("Credential() error = nil, want missing extractor error")
	}
	if !strings.Contains(err.Error(), `"jwt"`) {
		t.Fatalf("error = %q, want substring \"jwt\"", err)
	}
}

func credentialCtx() *web.Ctx {
	return enginetest.NewCtx(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
