package swagx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStandaloneHandler(t *testing.T, opts ...HandlerOption) http.Handler {
	t.Helper()
	handler, err := NewHandler(generatedDocument(testDocument), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// mountedHandler mounts a standalone handler the way a caller would: beneath a
// prefix with http.StripPrefix, or at the root of the mux.
func mountedHandler(handler http.Handler, prefix string) http.Handler {
	mux := http.NewServeMux()
	if prefix == "" {
		mux.Handle("/", handler)
		return mux
	}
	mux.Handle(prefix+"/", http.StripPrefix(prefix, handler))
	return mux
}

func serve(t *testing.T, handler http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, target, nil))
	return response
}

func TestStandaloneHandlerServesUIAndDocumentUnderPrefix(t *testing.T) {
	handler := mountedHandler(newStandaloneHandler(t, WithBasePath("/swagger")), "/swagger")

	for _, target := range []string{"/swagger/", "/swagger/index.html"} {
		response := serve(t, handler, http.MethodGet, target)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", target, response.Code, response.Body.String())
		}
		if body := response.Body.String(); !containsAll(body, "/swagger/doc.json", "Orders API") {
			t.Fatalf("%s does not reference the document URL and title: %s", target, body)
		}
	}

	response := serve(t, handler, http.MethodGet, "/swagger/doc.json")
	if response.Code != http.StatusOK {
		t.Fatalf("document status = %d", response.Code)
	}
	if response.Body.String() != testDocument {
		t.Fatalf("document body = %q", response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json; charset=utf-8" {
		t.Fatalf("document Content-Type = %q", contentType)
	}
	if nosniff := response.Header().Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Fatalf("document X-Content-Type-Options = %q", nosniff)
	}
}

func TestStandaloneHandlerWorksAtRootAndCustomPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{name: "root", prefix: ""},
		{name: "single segment", prefix: "/swagger"},
		{name: "nested", prefix: "/api/v1/docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := mountedHandler(newStandaloneHandler(t, WithBasePath(tc.prefix)), tc.prefix)
			documentURL := tc.prefix + "/doc.json"

			ui := serve(t, handler, http.MethodGet, tc.prefix+"/")
			if ui.Code != http.StatusOK {
				t.Fatalf("UI status = %d, body = %s", ui.Code, ui.Body.String())
			}
			if body := ui.Body.String(); !contains(body, documentURL) {
				t.Fatalf("UI does not reference %q: %s", documentURL, body)
			}

			document := serve(t, handler, http.MethodGet, documentURL)
			if document.Code != http.StatusOK || document.Body.String() != testDocument {
				t.Fatalf("document response = %d %q", document.Code, document.Body.String())
			}
		})
	}
}

func TestStandaloneHandlerAcceptsMuxMountWithPrefix(t *testing.T) {
	handler := newStandaloneHandler(t, WithBasePath("/swagger"))
	mux := http.NewServeMux()
	mux.Handle("/swagger/", handler)

	ui := serve(t, mux, http.MethodGet, "/swagger/")
	if ui.Code != http.StatusOK || !contains(ui.Body.String(), "/swagger/doc.json") {
		t.Fatalf("UI response = %d %s", ui.Code, ui.Body.String())
	}
	document := serve(t, mux, http.MethodGet, "/swagger/doc.json")
	if document.Code != http.StatusOK || document.Body.String() != testDocument {
		t.Fatalf("document response = %d %q", document.Code, document.Body.String())
	}
}

func TestStandaloneHandlerServesEmbeddedUIAssets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{name: "root", prefix: ""},
		{name: "prefix", prefix: "/swagger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := mountedHandler(newStandaloneHandler(t, WithBasePath(tc.prefix)), tc.prefix)
			response := serve(t, handler, http.MethodGet, tc.prefix+"/swagger-ui.css")
			if response.Code != http.StatusOK {
				t.Fatalf("asset status = %d", response.Code)
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/css") {
				t.Fatalf("asset Content-Type = %q", contentType)
			}
		})
	}
}

func TestStandaloneHandlerRejectsNonReadMethods(t *testing.T) {
	handler := mountedHandler(newStandaloneHandler(t, WithBasePath("/swagger")), "/swagger")

	for _, method := range []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodOptions,
	} {
		response := serve(t, handler, method, "/swagger/doc.json")
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s status = %d", method, response.Code)
		}
		if allow := response.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Fatalf("%s Allow = %q", method, allow)
		}
	}

	if response := serve(t, handler, http.MethodHead, "/swagger/doc.json"); response.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", response.Code)
	}
}

func TestStandaloneHandlerReturnsNotFoundForUnknownPaths(t *testing.T) {
	handler := mountedHandler(newStandaloneHandler(t, WithBasePath("/swagger")), "/swagger")

	for _, target := range []string{
		"/swagger/unknown.txt",
		"/swagger/nested/doc.json",
		"/swagger/doc.json/extra",
	} {
		if response := serve(t, handler, http.MethodGet, target); response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", target, response.Code)
		}
	}
}

func TestStandaloneHandlerSupportsCustomDocumentNameAndTitle(t *testing.T) {
	handler := mountedHandler(newStandaloneHandler(t,
		WithBasePath("/docs"),
		WithDocumentName("openapi.json"),
		WithTitle("Custom API"),
	), "/docs")

	ui := serve(t, handler, http.MethodGet, "/docs/")
	if ui.Code != http.StatusOK {
		t.Fatalf("UI status = %d", ui.Code)
	}
	if body := ui.Body.String(); !containsAll(body, "/docs/openapi.json", "Custom API") {
		t.Fatalf("UI does not reference the custom document URL and title: %s", body)
	}

	if response := serve(t, handler, http.MethodGet, "/docs/openapi.json"); response.Code != http.StatusOK || response.Body.String() != testDocument {
		t.Fatalf("custom document response = %d %q", response.Code, response.Body.String())
	}
	if response := serve(t, handler, http.MethodGet, "/docs/doc.json"); response.Code != http.StatusNotFound {
		t.Fatalf("default document name still served: %d", response.Code)
	}
}

func TestStandaloneHandlerPersistsAuthorizationOnRequest(t *testing.T) {
	without := mountedHandler(newStandaloneHandler(t, WithBasePath("/swagger")), "/swagger")
	if body := serve(t, without, http.MethodGet, "/swagger/").Body.String(); contains(body, "persistAuthorization") {
		t.Fatalf("default UI enables authorization persistence: %s", body)
	}

	with := mountedHandler(newStandaloneHandler(t,
		WithBasePath("/swagger"),
		WithPersistAuthorization(true),
	), "/swagger")
	if body := serve(t, with, http.MethodGet, "/swagger/").Body.String(); !contains(body, "persistAuthorization: true") {
		t.Fatalf("UI does not enable authorization persistence: %s", body)
	}
}

func TestNewHandlerRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewHandler(nil); err == nil {
		t.Fatal("nil document accepted")
	}
	if _, err := NewHandler(generatedDocument(`{"info":`)); err == nil {
		t.Fatal("invalid document accepted")
	}
	if _, err := NewHandler(generatedDocument(`{"info":{"title":"Missing version"}}`)); err == nil {
		t.Fatal("document without a Swagger or OpenAPI version accepted")
	}

	for _, basePath := range []string{"swagger", "/swagger?x=1", "/swagger#top", "/swagger/*"} {
		if _, err := NewHandler(generatedDocument(testDocument), WithBasePath(basePath)); err == nil {
			t.Fatalf("invalid base path %q accepted", basePath)
		}
	}
	for _, name := range []string{"", " ", "nested/doc.json", `nested\doc.json`, ".", "..", "index.html"} {
		if _, err := NewHandler(generatedDocument(testDocument), WithDocumentName(name)); err == nil {
			t.Fatalf("invalid document name %q accepted", name)
		}
	}
}
