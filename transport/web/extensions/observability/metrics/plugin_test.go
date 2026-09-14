package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

const currentRouteKeyForTest = "xbc/web.currentRoute"

func init() { gin.SetMode(gin.TestMode) }

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

func TestNewBuildsPrivateRegistry(t *testing.T) {
	p := New()
	if p.Registry() == nil {
		t.Fatal("New() did not build a private registry")
	}
}

func TestPrivateRegistryNeverPollutesDefaultRegistry(t *testing.T) {
	before, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	New()
	after, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("default registry changed from %d to %d families", len(before), len(after))
	}
	for _, family := range after {
		if strings.HasPrefix(family.GetName(), "xbc_http_") {
			t.Fatalf("private collector %q leaked into DefaultGatherer", family.GetName())
		}
	}
}

func TestRecordsRouteTemplateAndBoundedLabels(t *testing.T) {
	p := New()
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, web.RouteInfo{Method: http.MethodGet, Path: "/users/:id", Name: "users.get"})
		c.Next()
	})
	engine.Use(web.Handle(p.Handler()))
	engine.GET("/users/:id", func(c *gin.Context) { c.Status(http.StatusCreated) })

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/users/42", nil))
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d", response.Code)
	}

	families, err := p.Registry().inner.Gather()
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

func TestRegistryRejectsDuplicatesWithoutPanic(t *testing.T) {
	registry := newRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "business_events_total", Help: "events"})
	if err := registry.Register(counter); err != nil {
		t.Fatal(err)
	}
	var duplicate prometheus.AlreadyRegisteredError
	if err := registry.Register(counter); err == nil || !errors.As(err, &duplicate) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := registry.Register(nil); err == nil {
		t.Fatal("nil collector accepted")
	}
}

func TestConcurrentRequestsAndGather(t *testing.T) {
	p := New()
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(currentRouteKeyForTest, web.RouteInfo{Method: http.MethodGet, Path: "/items/:id"})
		c.Next()
	})
	engine.Use(web.Handle(p.Handler()))
	engine.GET("/items/:id", func(c *gin.Context) { c.Status(http.StatusNoContent) })

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
			if _, err := p.Registry().inner.Gather(); err != nil {
				t.Errorf("Gather: %v", err)
			}
		}()
	}
	wg.Wait()

	families, err := p.Registry().inner.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "xbc_http_requests_total" {
			if got := family.Metric[0].GetCounter().GetValue(); got != count {
				t.Fatalf("requests = %v, want %d", got, count)
			}
			return
		}
	}
	t.Fatal("request counter not found")
}
