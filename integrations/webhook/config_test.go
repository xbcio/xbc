package webhook

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/plugin"
)

func TestDefaultConfigAndNormalization(t *testing.T) {
	want := Config{
		Workers:          4,
		QueueSize:        1024,
		Backpressure:     BackpressureBlock,
		MaxAttempts:      5,
		RequestTimeout:   10 * time.Second,
		MaxPayloadBytes:  1 << 20,
		MaxResponseBytes: 64 << 10,
		AllowHTTP:        false,
		MaxRedirects:     5,
		InitialBackoff:   500 * time.Millisecond,
		MaxBackoff:       30 * time.Second,
		Jitter:           0.2,
		MaxRetryAfter:    30 * time.Second,
	}
	if got := DefaultConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultConfig() = %#v, want %#v", got, want)
	}
	if err := want.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() error = %v", err)
	}

	partial := Config{
		Backpressure:  " REJECT ",
		MaxRedirects:  0,
		Jitter:        0,
		MaxRetryAfter: 0,
	}
	got, err := prepareConfig(partial)
	if err != nil {
		t.Fatalf("prepareConfig() error = %v", err)
	}
	if got.Workers != want.Workers || got.QueueSize != want.QueueSize || got.MaxAttempts != want.MaxAttempts {
		t.Fatalf("normalized defaults = %#v", got)
	}
	if got.Backpressure != BackpressureReject || got.MaxRedirects != 0 || got.Jitter != 0 || got.MaxRetryAfter != 0 {
		t.Fatalf("normalized explicit zero values = %#v", got)
	}
}

func TestConfigValidationRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"workers", func(c *Config) { c.Workers = -1 }},
		{"queue", func(c *Config) { c.QueueSize = -1 }},
		{"backpressure", func(c *Config) { c.Backpressure = "drop-oldest" }},
		{"attempts", func(c *Config) { c.MaxAttempts = -1 }},
		{"request timeout", func(c *Config) { c.RequestTimeout = -time.Second }},
		{"payload limit", func(c *Config) { c.MaxPayloadBytes = -1 }},
		{"response limit", func(c *Config) { c.MaxResponseBytes = -1 }},
		{"redirects", func(c *Config) { c.MaxRedirects = -1 }},
		{"initial backoff", func(c *Config) { c.InitialBackoff = -time.Millisecond }},
		{"maximum backoff", func(c *Config) { c.MaxBackoff = time.Millisecond; c.InitialBackoff = time.Second }},
		{"negative jitter", func(c *Config) { c.Jitter = -0.1 }},
		{"large jitter", func(c *Config) { c.Jitter = 1.1 }},
		{"retry after", func(c *Config) { c.MaxRetryAfter = -time.Second }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func TestConfigFieldsHaveBindingMetadata(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Tag.Get("yaml") == "" {
			t.Errorf("Config.%s has no yaml tag", field.Name)
		}
		if field.Tag.Get("default") == "" {
			t.Errorf("Config.%s has no default tag", field.Name)
		}
	}
}

func TestDefinitionBundleDefaultsAndTypedExports(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"webhook": map[string]any{
				"primary":   map[string]any{},
				"secondary": map[string]any{"workers": 2},
			},
		},
	}, "XBC_WEBHOOK_TEST_")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), Bundle(), plugin.BundleOf(Definition())},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	if plan.DefinitionCount() != 1 {
		t.Fatalf("canonical Definition count = %d, want 1", plan.DefinitionCount())
	}
	wantOrder := []plugin.Identity{
		{Plugin: Key, Instance: "primary"},
		{Plugin: Key, Instance: "secondary"},
	}
	if got := plan.Order(); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("plan order = %#v, want %#v", got, wantOrder)
	}
	clientType := reflect.TypeOf((*Client)(nil))
	dispatcherType := reflect.TypeOf((*Dispatcher)(nil)).Elem()
	if got := plan.Contracts(clientType); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("*Client exports = %#v, want %#v", got, wantOrder)
	}
	if got := plan.Contracts(dispatcherType); !reflect.DeepEqual(got, wantOrder) {
		t.Fatalf("Dispatcher exports = %#v, want %#v", got, wantOrder)
	}

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	primary, ok := constructed.Instance(wantOrder[0])
	if !ok {
		t.Fatal("primary webhook instance missing")
	}
	secondary, ok := constructed.Instance(wantOrder[1])
	if !ok {
		t.Fatal("secondary webhook instance missing")
	}
	primaryClient := primary.Primary().(*Client)
	secondaryClient := secondary.Primary().(*Client)
	if primaryClient == secondaryClient {
		t.Fatal("named instances share one *Client")
	}
	if !reflect.DeepEqual(primaryClient.cfg, DefaultConfig()) {
		t.Fatalf("effective default config = %#v, want %#v", primaryClient.cfg, DefaultConfig())
	}
	if secondaryClient.cfg.Workers != 2 {
		t.Fatalf("bound secondary workers = %d, want 2", secondaryClient.cfg.Workers)
	}
	deadline, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := constructed.Unwind(deadline, time.Second, nil); err != nil {
		t.Fatalf("Unwind() error = %v", err)
	}
}
