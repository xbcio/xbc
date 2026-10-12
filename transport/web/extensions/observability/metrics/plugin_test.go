package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	dto "github.com/prometheus/client_model/go"

	metricscore "github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

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

func TestNewRegistersBuiltInInstrumentsIntoNeutralRegistry(t *testing.T) {
	core := newCoreForTest(t)
	value, err := New(core)
	if err != nil {
		t.Fatal(err)
	}

	// The vector instruments only materialize once a label combination exists;
	// one request through the middleware is enough to expose all three.
	engine := enginetest.New()
	engine.Use(value.Handler())
	engine.GET("/ping", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusOK)
		return nil
	})
	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ping", nil))

	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"xbc_http_requests_total",
		"xbc_http_request_duration_seconds",
		"xbc_http_requests_in_flight",
	} {
		if !hasFamily(families, name) {
			t.Fatalf("neutral registry did not gather %s", name)
		}
	}
}

func TestNewRejectsMissingInstallation(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("New(nil) succeeded")
	}
	if _, err := New(&metricscore.Plugin{}); err == nil {
		t.Fatal("New() accepted a metrics plugin without a registry")
	}
}

func TestRecordsRouteTemplateAndBoundedLabels(t *testing.T) {
	p, core := newPluginForTest(t)
	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		c.Set(currentRouteKeyForTest, web.RouteInfo{Method: http.MethodGet, Path: "/users/:id", Name: "users.get"})
		c.Next()
		return nil
	})
	engine.Use(p.Handler())
	engine.GET("/users/{id}", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusCreated)
		return nil
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/users/42", nil))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d", response.Code)
	}

	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["route"] == "/users/42" {
				t.Fatal("raw URL leaked into metric labels")
			}
			if labels["route"] == "/users/:id" && labels["status_class"] == "2xx" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("route template and bounded status class labels were not recorded")
	}
}

func TestConcurrentRequestsAndGather(t *testing.T) {
	p, core := newPluginForTest(t)
	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		c.Set(currentRouteKeyForTest, web.RouteInfo{Method: http.MethodGet, Path: "/items/:id"})
		c.Next()
		return nil
	})
	engine.Use(p.Handler())
	engine.GET("/items/{id}", func(_ context.Context, c *web.Ctx) error {
		c.Status(http.StatusNoContent)
		return nil
	})

	const count = 100
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/items/1", nil))
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := core.Registry().Gather(); err != nil {
				t.Errorf("Gather: %v", err)
			}
		}()
	}
	wg.Wait()

	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "xbc_http_requests_total" {
			if got := len(family.GetMetric()); got != 1 {
				t.Fatalf("request counter has %d series, want 1", got)
			}
			metric := family.GetMetric()[0]
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["route"] != "/items/:id" {
				t.Fatalf("route label = %q, want the frozen template", labels["route"])
			}
			if got := metric.GetCounter().GetValue(); got != count {
				t.Fatalf("requests = %v, want %d", got, count)
			}
			return
		}
	}
	t.Fatal("request counter not found")
}

func newCoreForTest(t *testing.T) *metricscore.Plugin {
	t.Helper()
	core, err := metricscore.New(metricscore.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return core
}

func newPluginForTest(t *testing.T) (*Plugin, *metricscore.Plugin) {
	t.Helper()
	core := newCoreForTest(t)
	value, err := New(core)
	if err != nil {
		t.Fatal(err)
	}
	return value, core
}

func hasFamily(families []*dto.MetricFamily, name string) bool {
	for _, family := range families {
		if family.GetName() == name {
			return true
		}
	}
	return false
}
