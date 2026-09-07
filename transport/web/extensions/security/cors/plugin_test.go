package cors

import (
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
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
	if order.Phase != web.PhaseSecurity || len(order.After) != 0 || len(order.Before) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
}

func TestConfiguredConstructionRejectsUnsafeConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowCredentials = true
	if _, err := prepareConfig(cfg); err == nil {
		t.Fatal("prepareConfig() succeeded for wildcard origin with credentials")
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New() succeeded for wildcard origin with credentials")
	}
}
