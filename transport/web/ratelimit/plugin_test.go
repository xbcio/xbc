package ratelimit

import (
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/cors"
)

func TestDefinitionAndMiddlewareMetadata(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	order := middleware.Order()
	if order.Phase != web.PhaseSecurity || len(order.After) != 1 || len(order.Before) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
	if ref := order.After[0]; ref.Key() != cors.Key || ref.InstanceName() != "" || ref.Required() {
		t.Fatalf("CORS order reference = %#v, want optional CORS key", ref)
	}
}

func TestConfiguredConstructionRejectsInvalidConfiguration(t *testing.T) {
	cfg := Config{Rate: 10, Burst: 0, Scope: ScopeGlobal}
	if _, err := prepareConfig(cfg); err == nil {
		t.Fatal("prepareConfig() succeeded with zero burst")
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New() succeeded with zero burst")
	}
}
