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
	// The pin is asserted by module path and version, not by directive
	// spelling: cmd/go writes a single direct requirement on its own line and
	// several in a block, so the manifest's shape changes as this module gains
	// direct dependencies while what is being pinned -- the exact version, as a
	// direct requirement, with no substitution -- does not.
	if !manifestRequires(manifest, "google.golang.org/grpc", "v1.81.1") {
		t.Error("go.mod must pin google.golang.org/grpc v1.81.1 exactly")
	}
	if regexp.MustCompile(`(?m)^\s*replace\b`).MatchString(manifest) {
		t.Fatal("go.mod must not contain replace directives")
	}
	if strings.Contains(manifest, "github.com/xbcio/xbc/transport/web") {
		t.Fatal("the gRPC transport must not depend on the Web transport")
	}
}

// manifestRequires reports whether a module manifest carries one direct
// requirement line for the path at that exact version. The line may be a bare
// require directive or an entry inside a require block. A trailing comment --
// "// indirect" above all -- means the entry does not count: a pin that has
// moved to the indirect block is no longer this module's own declaration.
func manifestRequires(manifest, path, version string) bool {
	for line := range strings.Lines(manifest) {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "require" {
			fields = fields[1:]
		}
		if len(fields) == 2 && fields[0] == path && fields[1] == version {
			return true
		}
	}
	return false
}

// TestManifestRequiresReadsBothDirectiveShapes holds the helper above to the
// spellings cmd/go actually writes, from both directions: a guard that stopped
// matching the block form would fail the pin test for the wrong reason, and one
// that accepted an indirect entry would stop guarding what the pin means.
func TestManifestRequiresReadsBothDirectiveShapes(t *testing.T) {
	const path, version = "google.golang.org/grpc", "v1.81.1"
	for _, tc := range []struct {
		name     string
		manifest string
		want     bool
	}{
		{name: "bare directive", manifest: "require " + path + " " + version + "\n", want: true},
		{name: "inside a block", manifest: "require (\n\t" + path + " " + version + "\n)\n", want: true},
		{name: "another version", manifest: "require (\n\t" + path + " v1.80.0\n)\n", want: false},
		{name: "indirect", manifest: "require (\n\t" + path + " " + version + " // indirect\n)\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := manifestRequires(tc.manifest, path, version); got != tc.want {
				t.Fatalf("manifestRequires(%q) = %t, want %t", tc.manifest, got, tc.want)
			}
		})
	}
}
