package swagger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

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

func publicAuth() *web.AuthPolicy {
	policy := web.Public()
	return &policy
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
	if cfg.JSONPath != "/openapi.json" || cfg.UIPath != "/docs" || !cfg.UIEnabled {
		t.Fatalf("defaults = %#v", cfg)
	}
}

func TestRoutesReadyPublishesMetadataAndPathParameters(t *testing.T) {
	p := New()
	err := p.RoutesReady(catalogStub{routes: []web.RouteInfo{
		{Method: http.MethodGet, Path: "/v1/users/:id", Name: "users.get", Perm: "users:read", Idempotent: true},
		{Method: http.MethodGet, Path: "/health", Name: "health", Auth: publicAuth()},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var document map[string]any
	if err := json.Unmarshal(p.Document(), &document); err != nil {
		t.Fatal(err)
	}
	paths := document["paths"].(map[string]any)
	users := paths["/v1/users/{id}"].(map[string]any)["get"].(map[string]any)
	if users["operationId"] != "users.get" || users["x-xbc-permission"] != "users:read" || users["x-xbc-idempotent"] != true {
		t.Fatalf("users operation = %#v", users)
	}
	if _, ok := users["security"]; !ok {
		t.Fatal("protected operation has no bearer security")
	}
	health := paths["/health"].(map[string]any)["get"].(map[string]any)
	if _, ok := health["security"]; ok {
		t.Fatal("public operation unexpectedly requires security")
	}
	assertProblemContract(t, document, users)
}

func TestProblemDetailSchemaExistsWhenBearerAuthenticationIsDisabled(t *testing.T) {
	p := New()
	p.settings.bearerAuth = false
	if err := p.RoutesReady(catalogStub{routes: []web.RouteInfo{{Method: http.MethodPost, Path: "/orders", Name: "orders.create"}}}); err != nil {
		t.Fatal(err)
	}

	var document map[string]any
	if err := json.Unmarshal(p.Document(), &document); err != nil {
		t.Fatal(err)
	}
	components := document["components"].(map[string]any)
	if _, exists := components["securitySchemes"]; exists {
		t.Fatal("disabled bearer authentication emitted a security scheme")
	}
	operation := document["paths"].(map[string]any)["/orders"].(map[string]any)["post"].(map[string]any)
	assertProblemContract(t, document, operation)
}

func assertProblemContract(t *testing.T, document map[string]any, operation map[string]any) {
	t.Helper()
	components, ok := document["components"].(map[string]any)
	if !ok {
		t.Fatal("document has no components")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("components has no schemas")
	}
	problem, ok := schemas["ProblemDetail"].(map[string]any)
	if !ok || problem["type"] != "object" {
		t.Fatalf("ProblemDetail schema = %#v", schemas["ProblemDetail"])
	}
	required := problem["required"].([]any)
	if len(required) != 3 || required[0] != "type" || required[1] != "title" || required[2] != "status" {
		t.Fatalf("ProblemDetail required fields = %#v", required)
	}
	properties := problem["properties"].(map[string]any)
	errorsProperty, ok := properties["errors"].(map[string]any)
	if !ok {
		t.Fatal("ProblemDetail schema does not expose validation field errors")
	}
	items := errorsProperty["items"].(map[string]any)
	if ref := items["$ref"]; ref != "#/components/schemas/FieldError" {
		t.Fatalf("ProblemDetail errors ref = %#v", ref)
	}
	fieldError, ok := schemas["FieldError"].(map[string]any)
	if !ok || fieldError["type"] != "object" {
		t.Fatalf("FieldError schema = %#v", schemas["FieldError"])
	}
	fieldProperties := fieldError["properties"].(map[string]any)
	if _, ok := fieldProperties["code"]; !ok {
		t.Fatal("FieldError schema does not expose the validation code")
	}
	if _, exists := fieldProperties["rule"]; exists {
		t.Fatal("FieldError schema still exposes the obsolete validation rule")
	}

	responses := operation["responses"].(map[string]any)
	defaultResponse := responses["default"].(map[string]any)
	content := defaultResponse["content"].(map[string]any)
	problemMediaType := content["application/problem+json"].(map[string]any)
	ref := problemMediaType["schema"].(map[string]any)["$ref"]
	if ref != "#/components/schemas/ProblemDetail" {
		t.Fatalf("default problem response ref = %#v", ref)
	}
}

func TestDuplicateOperationIDFailsWithoutReplacingSnapshot(t *testing.T) {
	p := New()
	good := catalogStub{routes: []web.RouteInfo{{Method: http.MethodGet, Path: "/one", Name: "one"}}}
	if err := p.RoutesReady(good); err != nil {
		t.Fatal(err)
	}
	before := string(p.Document())
	bad := catalogStub{routes: []web.RouteInfo{
		{Method: http.MethodGet, Path: "/one", Name: "duplicate"},
		{Method: http.MethodPost, Path: "/two", Name: "duplicate"},
	}}
	if err := p.RoutesReady(bad); err == nil {
		t.Fatal("duplicate operation ID accepted")
	}
	if got := string(p.Document()); got != before {
		t.Fatal("failed rebuild replaced last valid snapshot")
	}
}

func TestDocumentHandlerReadinessAndDefensiveCopy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := New()
	request := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	p.serveDocument(ctx)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status before RoutesReady = %d", response.Code)
	}

	if err := p.RoutesReady(catalogStub{}); err != nil {
		t.Fatal(err)
	}
	copyOne := p.Document()
	copyOne[0] = 'x'
	if p.Document()[0] == 'x' {
		t.Fatal("Document exposed mutable snapshot storage")
	}
}

func TestConfigRejectsOverlappingRoutes(t *testing.T) {
	cfg := DefaultConfig()
	cfg.UIPath = "/docs"
	cfg.JSONPath = "/docs/openapi.json"
	if _, err := newPlugin(cfg); err == nil {
		t.Fatal("overlapping UI and JSON routes accepted")
	}
}
