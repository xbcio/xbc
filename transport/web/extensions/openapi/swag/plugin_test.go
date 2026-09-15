package swag

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const testDocument = `{
  "swagger": "2.0",
  "info": {"title": "Orders API", "version": "1.0"},
  "basePath": "/api/v1",
  "paths": {"/orders": {"get": {"summary": "List orders", "responses": {"200": {"description": "OK"}}}}}
}`

type generatedDocument string

func (d generatedDocument) ReadDoc() string { return string(d) }

type catalogStub struct{ routes []web.RouteInfo }

func (c catalogStub) All() []web.RouteInfo { return append([]web.RouteInfo(nil), c.routes...) }
func (c catalogStub) Lookup(method, path string) (web.RouteInfo, bool) {
	for _, route := range c.routes {
		if route.Method == method && route.Path == path {
			return route, true
		}
	}
	return web.RouteInfo{}, false
}

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestDefaultConfigMatchesProductionDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.InstanceName != "swagger" || cfg.JSONPath != "/swagger.json" || cfg.UIPath != "/docs" || !cfg.UIEnabled {
		t.Fatalf("defaults = %#v", cfg)
	}
}

func TestNewLoadsGeneratedDocumentAndReturnsDefensiveCopies(t *testing.T) {
	p, err := New(generatedDocument(testDocument))
	if err != nil {
		t.Fatal(err)
	}

	var document map[string]any
	if err := json.Unmarshal(p.Document(), &document); err != nil {
		t.Fatal(err)
	}
	if document["swagger"] != "2.0" {
		t.Fatalf("swagger version = %#v", document["swagger"])
	}
	if title := document["info"].(map[string]any)["title"]; title != "Orders API" {
		t.Fatalf("title = %#v", title)
	}

	copyOne := p.Document()
	copyOne[0] = 'x'
	if p.Document()[0] == 'x' {
		t.Fatal("Document exposed mutable snapshot storage")
	}
}

func TestNewRejectsMissingAndInvalidGeneratedDocuments(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil generated document accepted")
	}
	if _, err := New(generatedDocument(`{"info":`)); err == nil {
		t.Fatal("invalid generated JSON accepted")
	}
	if _, err := New(generatedDocument(`{"info":{"title":"Missing version"}}`)); err == nil {
		t.Fatal("document without a Swagger or OpenAPI version accepted")
	}
}

func TestConfiguredReaderUsesNamedSwagInstance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InstanceName = "orders"
	var requested string
	p, err := newPlugin(cfg, func(name string) (string, error) {
		requested = name
		return testDocument, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if requested != "orders" {
		t.Fatalf("requested instance = %q", requested)
	}
	if len(p.Document()) == 0 {
		t.Fatal("generated document was not retained")
	}
}

func TestConfiguredReaderErrorNamesInstance(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InstanceName = "orders"
	_, err := newPlugin(cfg, func(string) (string, error) {
		return "", errors.New("not registered")
	})
	if err == nil || !containsAll(err.Error(), "orders", "not registered") {
		t.Fatalf("error = %v", err)
	}
}

func TestRoutesReadyPreparesSwaggerUIWithFinalTransportPaths(t *testing.T) {
	p, err := New(generatedDocument(testDocument))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RoutesReady(catalogStub{routes: []web.RouteInfo{
		{Method: http.MethodGet, Path: "/api/v1/swagger.json", Name: "swag.document"},
		{Method: http.MethodGet, Path: "/api/v1/docs", Name: "swag.ui"},
	}}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/docs", nil)
	response := httptest.NewRecorder()
	if err := p.serveUI(request.Context(), enginetest.NewCtx(response, request)); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("UI status = %d, body = %s", response.Code, response.Body.String())
	}
	if !containsAll(response.Body.String(), "/api/v1/swagger.json", "Orders API") {
		t.Fatalf("UI does not reference final document path and title: %s", response.Body.String())
	}
}

func TestDocumentHandlerServesGeneratedJSON(t *testing.T) {
	p, err := New(generatedDocument(testDocument))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/swagger.json", nil)
	response := httptest.NewRecorder()
	if err := p.serveDocument(request.Context(), enginetest.NewCtx(response, request)); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || response.Body.String() != testDocument {
		t.Fatalf("document response = %d %q", response.Code, response.Body.String())
	}
}

func TestConfigRejectsInvalidValues(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InstanceName = " "
	if _, err := newPlugin(cfg, func(string) (string, error) { return testDocument, nil }); err == nil {
		t.Fatal("empty instance name accepted")
	}

	cfg = DefaultConfig()
	cfg.UIPath = "/docs"
	cfg.JSONPath = "/docs/swagger.json"
	if _, err := newPlugin(cfg, func(string) (string, error) { return testDocument, nil }); err == nil {
		t.Fatal("overlapping UI and JSON routes accepted")
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !contains(value, part) {
			return false
		}
	}
	return true
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
