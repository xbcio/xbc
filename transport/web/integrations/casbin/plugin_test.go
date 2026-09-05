package casbin

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

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

func TestNewAppliesDefaultsAndMiddlewareContract(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p.Order().Phase != web.PhaseAuth {
		t.Fatalf("Order().Phase = %v, want PhaseAuth", p.Order().Phase)
	}
	after := p.Order().After
	if len(after) != 1 || after[0] != web.Prefer(jwtKey) {
		t.Fatalf("Order().After = %v, want [Prefer(jwt)]", after)
	}
	if p.Handler() == nil {
		t.Fatal("Handler() = nil")
	}
}

func TestNewBuildsEnforcerImmediatelyAndStopDeactivatesIt(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	enforcer, ok := p.Enforcer()
	if !ok || enforcer == nil {
		t.Fatal("New() did not build an active enforcer")
	}

	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	host.close()
	if _, ok := p.Enforcer(); ok {
		t.Fatal("Enforcer remained active after Stop")
	}
}

func TestNewRejectsInvalidModelFile(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ModelFile = "missing-model.conf"
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "model_file") {
		t.Fatalf("New() error = %v, want model_file failure", err)
	}

	cfg.ModelFile = ""
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New() with corrected config error = %v", err)
	}
	if _, ok := p.Enforcer(); !ok {
		t.Fatal("New() with corrected config did not build an active enforcer")
	}
}

func TestLifecycleOperationErrorsAndIdempotentStop(t *testing.T) {
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := p.start(nil); err == nil {
		t.Fatal("start(nil) error = nil")
	}

	host := newTestHost()
	if err := p.start(testContext(host)); err != nil {
		t.Fatal(err)
	}
	if err := p.start(testContext(host)); err == nil {
		t.Fatal("second start() error = nil")
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if !errors.Is(p.LoadPolicy(), ErrStopped) {
		t.Fatalf("LoadPolicy() after Stop = %v, want ErrStopped", p.LoadPolicy())
	}
	if err := p.start(testContext(host)); !errors.Is(err, ErrStopped) {
		t.Fatalf("start() after Stop = %v, want ErrStopped", err)
	}
	host.close()
}
