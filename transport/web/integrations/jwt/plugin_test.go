package jwt

import (
	"reflect"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

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

func TestNewRejectsInvalidConfigAndAppliesDefaults(t *testing.T) {
	if _, err := New(DefaultConfig()); err == nil {
		t.Fatal("New(DefaultConfig()) error = nil, want a secret validation failure")
	}

	cfg := DefaultConfig()
	cfg.Secret = testSecret
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.compiled.algorithm != "HS256" || p.compiled.expire != 2*time.Hour ||
		p.compiled.header != "Authorization" || p.compiled.scheme != "Bearer" {
		t.Fatalf("New defaults = %#v", p.compiled.normalizedConfig)
	}
	if p.Order().Phase != web.PhaseAuth {
		t.Fatalf("Order().Phase = %v, want PhaseAuth", p.Order().Phase)
	}
	if p.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
}
