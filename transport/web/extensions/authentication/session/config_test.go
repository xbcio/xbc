package session

import (
	"reflect"
	"strings"
	"testing"

	"github.com/xbcio/xbc/config"
	sessioncore "github.com/xbcio/xbc/extensions/authentication/session"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestDefaultConfigSelectsOnlyTheCookieCarrier(t *testing.T) {
	config := DefaultConfig()
	if !reflect.DeepEqual(config.Carriers, []string{CarrierCookie}) {
		t.Fatalf("DefaultConfig().Carriers = %v, want the cookie carrier alone", config.Carriers)
	}
	if config.Header.Name != "Authorization" || config.Header.Scheme != "Session" {
		t.Fatalf("DefaultConfig().Header = %+v, want Authorization/Session", config.Header)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v", err)
	}
}

func TestNormalizeCarriers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		values  []string
		want    []string
		wantErr string
	}{
		{name: "empty selects cookie", values: nil, want: []string{CarrierCookie}},
		{name: "blank entries select cookie", values: []string{" ", ""}, want: []string{CarrierCookie}},
		{
			name:   "order and case are preserved after normalization",
			values: []string{" Header ", "Cookie"},
			want:   []string{CarrierHeader, CarrierCookie},
		},
		{name: "unknown carrier", values: []string{"query"}, wantErr: "unknown carrier"},
		{name: "duplicate carrier", values: []string{"cookie", "COOKIE"}, wantErr: "more than once"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeCarriers(test.values)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("normalizeCarriers(%v) = %v, %v; want error containing %q", test.values, got, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeCarriers(%v) error = %v", test.values, err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalizeCarriers(%v) = %v, want %v", test.values, got, test.want)
			}
		})
	}
}

func TestNormalizeConfigHeaderDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	normalized, err := normalizeConfig(Config{Carriers: []string{CarrierHeader}})
	if err != nil {
		t.Fatalf("normalizeConfig(default header) error = %v", err)
	}
	if normalized.header != DefaultHeaderCarrierConfig() {
		t.Fatalf("header = %+v, want %+v", normalized.header, DefaultHeaderCarrierConfig())
	}

	for name, config := range map[string]Config{
		"invalid header name":   {Header: HeaderConfig{Name: "bad header"}},
		"invalid header scheme": {Header: HeaderConfig{Scheme: "bad\nscheme"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := normalizeConfig(config); err == nil {
				t.Fatalf("normalizeConfig(%+v) = nil error", config)
			}
		})
	}
}

// TestDefinitionOwnsOptionalConfigWithoutChangingActivation pins both halves of
// the compatibility promise: the plugin now owns plugins.session-http, and it
// still activates on plugins.session alone.
func TestDefinitionOwnsOptionalConfigWithoutChangingActivation(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	if descriptor.Config == nil {
		t.Fatal("Definition declares no configuration; plugins.session-http would reject its keys")
	}
	if descriptor.Config.Type != reflect.TypeOf(Config{}) {
		t.Fatalf("config type = %v, want %v", descriptor.Config.Type, reflect.TypeOf(Config{}))
	}
	if descriptor.Activation.Kind != pluginmodel.ActivationConfigured || descriptor.Activation.Path != "plugins.session" {
		t.Fatalf("activation = %+v, want WhenConfigured(plugins.session)", descriptor.Activation)
	}
}

// TestCarrierSectionAloneDoesNotActivateTheAdapter keeps the activation
// semantics the plugin had before it owned a section: session-http is a
// companion of session, never a standalone switch.
func TestCarrierSectionAloneDoesNotActivateTheAdapter(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"session-http": map[string]any{"carriers": []any{"header"}},
		},
	}, "")
	if err != nil {
		t.Fatalf("config.NewEnvironment() error = %v", err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: []plugin.Bundle{Bundle()}, Env: environment})
	if err != nil {
		t.Fatalf("assembly.BuildPlan() error = %v", err)
	}
	if order := plan.Order(); len(order) != 0 {
		t.Fatalf("enabled identities = %v, want none when plugins.session is absent", order)
	}
	disabled := make(map[plugin.Key]bool)
	for _, entry := range plan.DisabledDetail() {
		disabled[entry.Key] = true
	}
	if !disabled[sessioncore.Key] || !disabled[Key] {
		t.Fatalf("disabled = %v, want both %q and %q", disabled, sessioncore.Key, Key)
	}
}

// TestSessionSectionActivatesBothPluginsWithTheDefaultCookieChain pins the
// unchanged composition: configuring plugins.session alone constructs the
// adapter with the historical cookie-only behavior, and plugins.session-http
// stays an optional overlay.
func TestSessionSectionActivatesBothPluginsWithTheDefaultCookieChain(t *testing.T) {
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{"session": map[string]any{}},
	}, "")
	if err != nil {
		t.Fatalf("config.NewEnvironment() error = %v", err)
	}
	plan, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: []plugin.Bundle{Bundle()}, Env: environment})
	if err != nil {
		t.Fatalf("assembly.BuildPlan() error = %v", err)
	}
	enabled := make(map[plugin.Key]bool)
	for _, identity := range plan.Order() {
		enabled[identity.Plugin] = true
	}
	if !enabled[sessioncore.Key] || !enabled[Key] {
		t.Fatalf("enabled = %v, want both %q and %q", enabled, sessioncore.Key, Key)
	}
	identity := plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance}
	if path := plan.InstanceConfigPath(identity); path != "plugins.session-http" {
		t.Fatalf("config path = %q, want plugins.session-http", path)
	}
	if path, configured := plan.InstanceActivationPath(identity); !configured || path != "plugins.session" {
		t.Fatalf("activation path = %q, configured=%v; want plugins.session", path, configured)
	}
}
