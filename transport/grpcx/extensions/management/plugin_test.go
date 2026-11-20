package management

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// buildPlan runs the real planning seam over the Bundle and an environment, so
// what these tests assert is what an operator's configuration actually
// produces: section activation, key decoding, and plan-time refusals.
func buildPlan(t *testing.T, environment map[string]any) (*assembly.Plan, error) {
	t.Helper()
	env, err := config.NewEnvironment(environment, "")
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	return assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     env,
	})
}

// constructInstance plans and constructs the management instance, failing the
// test when either step or the instance itself is missing.
func constructInstance(t *testing.T, host *bootHost, environment map[string]any) *Plugin {
	t.Helper()
	built, err := buildPlan(t, environment)
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("the management plane was not constructed")
	}
	primary, ok := instance.Primary().(*Plugin)
	if !ok {
		t.Fatalf("management primary = %T, want *Plugin", instance.Primary())
	}
	return primary
}

// TestTheSectionActivatesThePlaneAndDecodesItsKeys pins the configured path end
// to end: the section's presence activates the plugin, every documented key
// decodes, and the declared address is the one the listener binds.
func TestTheSectionActivatesThePlaneAndDecodesItsKeys(t *testing.T) {
	host := newBootHost()
	t.Cleanup(host.shutdown)

	addr := freeAddress(t)
	plugin := constructInstance(t, host, map[string]any{
		"plugins": map[string]any{
			"grpc-management": map[string]any{
				"addr":         addr,
				"allow_remote": false,
				"pprof":        true,
				"tls":          map[string]any{"min_version": "1.3"},
			},
		},
	})

	if plugin.config.Addr != addr {
		t.Errorf("decoded addr = %q, want %q", plugin.config.Addr, addr)
	}
	if plugin.config.AllowRemote {
		t.Error("decoded allow_remote = true, want false")
	}
	if !plugin.config.Pprof {
		t.Error("decoded pprof = false, want true")
	}
	if plugin.config.TLS.MinVersion != "1.3" {
		t.Errorf("decoded tls.min_version = %q, want 1.3", plugin.config.TLS.MinVersion)
	}
	if plugin.gatherer != nil {
		t.Error("the plane found a metrics registry without plugins.metrics being selected")
	}
}

// TestTheSectionIsTheOnlyActivation pins WhenConfigured: a plan that never
// names the section contains no management listener at all.
func TestTheSectionIsTheOnlyActivation(t *testing.T) {
	built, err := buildPlan(t, map[string]any{"plugins": map[string]any{"metrics": map[string]any{}}})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	for _, identity := range built.Order() {
		if identity.Plugin == Key {
			t.Fatalf("the management plane activated without its section: %v", identity)
		}
	}
	if len(built.Order()) == 0 {
		t.Fatal("the plan is empty; the guard above would pass vacuously")
	}
}

// TestThePlaneExposesTheSelectedMetricsRegistry is the wiring test for the
// optional input: with plugins.metrics selected the scrape renders that
// plugin's registry -- the build_info series it registers is the evidence --
// and without it the route does not exist.
func TestThePlaneExposesTheSelectedMetricsRegistry(t *testing.T) {
	t.Run("selected", func(t *testing.T) {
		host := newBootHost()
		t.Cleanup(host.shutdown)
		plugin := constructInstance(t, host, map[string]any{
			"plugins": map[string]any{
				"metrics":         map[string]any{},
				"grpc-management": map[string]any{"addr": freeAddress(t)},
			},
		})
		if plugin.gatherer == nil {
			t.Fatal("the plane resolved no registry although plugins.metrics is selected")
		}

		if err := plugin.Start(host.contextFor(Key)); err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		host.releaseTraffic()
		waitForServing(t, plugin.addr(), "/metrics")

		status, body, err := fetch(plugin.addr(), "/metrics", 2*time.Second)
		if err != nil {
			t.Fatalf("fetch(/metrics) = %v", err)
		}
		if status != 200 {
			t.Fatalf("fetch(/metrics) status = %d, want 200", status)
		}
		if !strings.Contains(body, "xbc_build_info") {
			t.Errorf("the scrape does not render the neutral registry:\n%s", body)
		}

		stopContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := plugin.Stop(stopContext); err != nil {
			t.Fatalf("Stop() = %v", err)
		}
	})

	t.Run("not selected", func(t *testing.T) {
		host := newBootHost()
		t.Cleanup(host.shutdown)
		plugin := constructInstance(t, host, map[string]any{
			"plugins": map[string]any{"grpc-management": map[string]any{"addr": freeAddress(t)}},
		})
		if plugin.gatherer != nil {
			t.Fatal("the plane resolved a registry although no metrics plugin is selected")
		}
		if plugin.config.Pprof {
			t.Fatal("pprof is on without being configured")
		}
	})
}

// TestAPlannedSectionWithoutAnAddressFailsThePlan pins the startup refusal for
// the section that asks for a listener and names no address.
func TestAPlannedSectionWithoutAnAddressFailsThePlan(t *testing.T) {
	_, err := buildPlan(t, map[string]any{
		"plugins": map[string]any{"grpc-management": map[string]any{"pprof": true}},
	})
	if err == nil {
		t.Fatal("a section without an address planned successfully")
	}
	if !strings.Contains(err.Error(), "addr must be a non-empty address") {
		t.Errorf("plan error = %v, want the address refusal", err)
	}
}

// TestARemoteAddressWithoutConsentOrTLSFailsThePlan is the composition-seam
// half of the binding rule: the refusals a directly validated Config raises
// must reach an operator through planning a real configuration, which is where
// a startup actually fails.
func TestARemoteAddressWithoutConsentOrTLSFailsThePlan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote map[string]any
		want   string
	}{
		{
			name:   "without allow_remote",
			remote: map[string]any{"addr": "0.0.0.0:9091"},
			want:   "not loopback",
		},
		{
			name:   "allow_remote without TLS",
			remote: map[string]any{"addr": "0.0.0.0:9091", "allow_remote": true},
			want:   "allow_remote requires tls.cert_file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildPlan(t, map[string]any{
				"plugins": map[string]any{"grpc-management": tc.remote},
			})
			if err == nil {
				t.Fatal("a remotely reachable management address planned successfully")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("plan error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
