package grpc

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestDefinitionAndBundleAreCanonicalOpaqueHandles(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	if reflect.DeepEqual(Bundle(), plugin.Bundle{}) || !reflect.DeepEqual(Bundle(), Bundle()) {
		t.Fatal("Bundle() must return one non-empty canonical composition")
	}
	var got []pluginmodel.Definition
	for _, entry := range pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle())) {
		got = append(got, entry.Definition)
	}
	if want := []pluginmodel.Definition{pluginmodel.Definition(Definition())}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Bundle() definitions = %v, want only the server", got)
	}
}

func TestNewIsSideEffectFreeWithDefaults(t *testing.T) {
	s := New()
	if !reflect.DeepEqual(s.cfg, DefaultConfig()) || s.Addr() != "" {
		t.Fatalf("New() = %+v", s)
	}
}

func TestReservedInterceptorKeysNameEveryAssembledStage(t *testing.T) {
	want := []plugin.Key{AuthenticationInterceptorKey, PanicBoundaryKey, StatusBoundaryKey}
	if got := ReservedInterceptorKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReservedInterceptorKeys() = %v, want %v", got, want)
	}
}

func TestNormalizeConfigFillsZeroValuesAndRejectsInvalidOnes(t *testing.T) {
	// Recovery.Stack is a bool whose false is a deliberate choice, so it is the
	// one field a zero value does not reset.
	want := DefaultConfig()
	want.Recovery.Stack = false
	got, err := normalizeConfig(Config{})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeConfig(zero) = %+v, %v; want the defaults", got, err)
	}
	invalid := map[string]func(*Config){
		"padded addr":        func(c *Config) { c.Addr = " :9090" },
		"negative recv":      func(c *Config) { c.MaxRecvMsgBytes = -1 },
		"negative send":      func(c *Config) { c.MaxSendMsgBytes = -1 },
		"negative pre-drain": func(c *Config) { c.Shutdown.PreDrainDelay = -time.Second },
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			mutate(&cfg)
			if _, err := normalizeConfig(cfg); err == nil {
				t.Fatal("normalizeConfig accepted an invalid config")
			}
		})
	}
}

func TestModuleManifestPinsGRPCWithoutLocalSubstitutions(t *testing.T) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(data)
	if !regexp.MustCompile(`(?m)^require\s+google\.golang\.org/grpc\s+v1\.81\.1\s*$`).MatchString(manifest) {
		t.Error("go.mod must pin google.golang.org/grpc v1.81.1 exactly")
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).MatchString(manifest) {
		t.Fatal("go.mod must not contain replace directives")
	}
	if strings.Contains(manifest, "github.com/xbcio/xbc/transport/web") {
		t.Fatal("the gRPC transport must not depend on the Web transport")
	}
}
