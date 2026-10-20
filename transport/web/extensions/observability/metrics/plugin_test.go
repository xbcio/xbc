package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
	value, err := New(core, &recordingAdmissionSource{})
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

// recordingAdmissionSource is a stub web.AdmissionStatsSource. It records how
// many snapshots were taken and lets the test choose the advertised values.
type recordingAdmissionSource struct {
	mu    sync.Mutex
	stats web.InFlightStats
	calls int
}

func (s *recordingAdmissionSource) InFlightStats() web.InFlightStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.stats
}

func (s *recordingAdmissionSource) set(stats web.InFlightStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = stats
}

func (s *recordingAdmissionSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// admissionMetric returns the single unlabelled metric of family name, failing
// the test unless the family exists exactly once and has the expected type.
func admissionMetric(t *testing.T, families []*dto.MetricFamily, name string, kind dto.MetricType) *dto.Metric {
	t.Helper()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		if family.GetType() != kind {
			t.Fatalf("%s type = %s, want %s", name, family.GetType(), kind)
		}
		if got := len(family.GetMetric()); got != 1 {
			t.Fatalf("%s has %d series, want 1", name, got)
		}
		metric := family.GetMetric()[0]
		if got := len(metric.GetLabel()); got != 0 {
			t.Fatalf("%s carries %d labels, want none", name, got)
		}
		return metric
	}
	t.Fatalf("family %s not gathered", name)
	return nil
}

func TestAdmissionCollectorEmitsOneSnapshotPerGather(t *testing.T) {
	core := newCoreForTest(t)
	source := &recordingAdmissionSource{}
	source.set(web.InFlightStats{Limit: 8, InFlight: 3, Rejections: 5})
	if _, err := New(core, source); err != nil {
		t.Fatal(err)
	}
	gather := func() []*dto.MetricFamily {
		t.Helper()
		families, err := core.Registry().Gather()
		if err != nil {
			t.Fatal(err)
		}
		return families
	}

	families := gather()
	if got := admissionMetric(t, families, "xbc_http_admission_limit", dto.MetricType_GAUGE).GetGauge().GetValue(); got != 8 {
		t.Fatalf("admission limit = %v, want 8", got)
	}
	if got := admissionMetric(t, families, "xbc_http_admission_in_flight", dto.MetricType_GAUGE).GetGauge().GetValue(); got != 3 {
		t.Fatalf("admission in flight = %v, want 3", got)
	}
	if got := admissionMetric(t, families, "xbc_http_admission_rejections_total", dto.MetricType_COUNTER).GetCounter().GetValue(); got != 5 {
		t.Fatalf("admission rejections = %v, want 5", got)
	}
	if got := source.callCount(); got != 1 {
		t.Fatalf("InFlightStats calls after one gather = %d, want 1", got)
	}

	// A second gather must take a fresh snapshot rather than replay cached
	// values, and still take exactly one.
	source.set(web.InFlightStats{Limit: 12, InFlight: 1, Rejections: 9})
	families = gather()
	if got := admissionMetric(t, families, "xbc_http_admission_limit", dto.MetricType_GAUGE).GetGauge().GetValue(); got != 12 {
		t.Fatalf("admission limit after update = %v, want 12", got)
	}
	if got := admissionMetric(t, families, "xbc_http_admission_in_flight", dto.MetricType_GAUGE).GetGauge().GetValue(); got != 1 {
		t.Fatalf("admission in flight after update = %v, want 1", got)
	}
	if got := admissionMetric(t, families, "xbc_http_admission_rejections_total", dto.MetricType_COUNTER).GetCounter().GetValue(); got != 9 {
		t.Fatalf("admission rejections after update = %v, want 9", got)
	}
	if got := source.callCount(); got != 2 {
		t.Fatalf("InFlightStats calls after two gathers = %d, want 2", got)
	}
}

func TestAdmissionCollectorEmitsZeroSnapshot(t *testing.T) {
	core := newCoreForTest(t)
	source := &recordingAdmissionSource{}
	if _, err := New(core, source); err != nil {
		t.Fatal(err)
	}
	families, err := core.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	if got := admissionMetric(t, families, "xbc_http_admission_limit", dto.MetricType_GAUGE).GetGauge().GetValue(); got != 0 {
		t.Fatalf("admission limit = %v, want 0", got)
	}
	if got := admissionMetric(t, families, "xbc_http_admission_in_flight", dto.MetricType_GAUGE).GetGauge().GetValue(); got != 0 {
		t.Fatalf("admission in flight = %v, want 0", got)
	}
	if got := admissionMetric(t, families, "xbc_http_admission_rejections_total", dto.MetricType_COUNTER).GetCounter().GetValue(); got != 0 {
		t.Fatalf("admission rejections = %v, want 0", got)
	}
	if got := source.callCount(); got != 1 {
		t.Fatalf("InFlightStats calls = %d, want 1", got)
	}
}

func TestNewRejectsMissingInstallation(t *testing.T) {
	if _, err := New(nil, nil); err == nil {
		t.Fatal("New(nil, nil) succeeded")
	}
	if _, err := New(&metricscore.Plugin{}, &recordingAdmissionSource{}); err == nil {
		t.Fatal("New() accepted a metrics plugin without a registry")
	}
	_, err := New(newCoreForTest(t), nil)
	if err == nil {
		t.Fatal("New() accepted a nil admission stats source")
	}
	if !strings.Contains(err.Error(), "admission stats source") {
		t.Fatalf("New() error does not name the missing input: %v", err)
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
	value, err := New(core, &recordingAdmissionSource{})
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
