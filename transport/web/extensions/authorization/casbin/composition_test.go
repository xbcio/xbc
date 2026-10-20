package casbin

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xbcio/xbc"
	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// missingAuthorizerWarning is web's startup warning, repeated here rather than
// imported: it is the observable the composition test asserts on, and the web
// package keeps the constant unexported.
const missingAuthorizerWarning = "web: routes declare permissions but no middleware in the chain enforces Route.Perm"

// noopEngine satisfies web.Engine for a validate run, which assembles the
// pipeline -- including one engine per listener -- but never registers a route
// or serves a request.
type noopEngine struct{}

func (noopEngine) Handle(string, string, []web.Handler) {}
func (noopEngine) Mount(string, string, []web.Handler)  {}
func (noopEngine) NoRoute([]web.Handler)                {}
func (noopEngine) NoMethod([]web.Handler)               {}
func (noopEngine) Serve(net.Listener) error             { return nil }
func (noopEngine) Shutdown(context.Context) error       { return nil }

type noopFactory struct{}

func (noopFactory) NewEngine(web.Options) (web.Engine, error) { return noopEngine{}, nil }

// testEngineBundle exports the EngineFactory web.RequireOne input. The
// composition under test is about the ordered middleware chain, not about a
// concrete HTTP engine, so the engine is a silent stub.
func testEngineBundle() plugin.Bundle {
	definition := plugin.Define(
		"composition-test-engine",
		func(plugin.BuildContext) (noopFactory, error) { return noopFactory{}, nil },
		plugin.Options[noopFactory]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.EngineFactory](func(value noopFactory) web.EngineFactory { return value }),
			),
		},
	)
	return plugin.BundleOf(definition)
}

// testPermRoutes contributes the serving route the check keys on: one route
// carrying .Perm, the declaration the missing-authorizer warning is about.
type testPermRoutes struct{}

func (testPermRoutes) RegisterRoutes(router *web.Router) {
	// Public only keeps the authentication policy out of this test's way; the
	// permission declaration is what the authorizer check keys on, and it is
	// counted on a public route exactly like on a protected one.
	router.GET("/orders", func(context.Context, *web.Ctx) error { return nil }).
		Auth(web.Public()).
		Perm("orders:read")
}

func testPermRouteBundle() plugin.Bundle {
	definition := plugin.Define(
		"composition-test-routes",
		func(plugin.BuildContext) (testPermRoutes, error) { return testPermRoutes{}, nil },
		plugin.Options[testPermRoutes]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value testPermRoutes) web.RouteContributor { return value }),
			),
		},
	)
	return plugin.BundleOf(definition)
}

// TestComposedMiddlewareIsDetectedAsTheRouteAuthorizer is the composition-level
// half of the marker contract. The compile-time assertion in plugin.go proves
// *Plugin implements web.RouteAuthorizer; only a real composition proves the
// value the server receives through plugin.Collect[web.Middleware] carries
// that dynamic type. The framework binds the constructed primary value -- the
// ExportAs witness is a compile-time check, not a conversion -- so this test
// observes exactly the value the server's type assertion inspects. Changing
// what the Definition constructs would leave the stub-based web tests green;
// this composition is where the real product's place in the chain is pinned.
//
// The second run is the positive control: the same route table, composed
// without plugins.casbin-http, must produce the warning through the same log
// destination. Without it, a broken log capture would make the first assertion
// vacuous -- and "no warning observed" would be indistinguishable from "no
// warning written".
func TestComposedMiddlewareIsDetectedAsTheRouteAuthorizer(t *testing.T) {
	t.Cleanup(func() {
		_ = corelog.Close()
		corelog.SetLogger(corelog.Nop())
	})

	withAuthorizer := runValidate(t, true)
	withoutAuthorizer := runValidate(t, false)

	if warnings := logLinesContaining(t, withAuthorizer, missingAuthorizerWarning); len(warnings) != 0 {
		t.Fatalf("composed casbin-http middleware did not suppress the missing-authorizer warning:\n%s", strings.Join(warnings, "\n"))
	}
	if warnings := logLinesContaining(t, withoutAuthorizer, missingAuthorizerWarning); len(warnings) != 1 {
		t.Fatalf("control run produced %d missing-authorizer warnings, want exactly 1:\n%s", len(warnings), strings.Join(warnings, "\n"))
	}
}

// TestComposedMiddlewareSatisfiesRequireRouteAuthorizer is the strict variant:
// with web.security.require_route_authorizer set, the detection result is the
// difference between a successful validate and a startup failure, so the
// composition link is asserted without reading a log line at all.
func TestComposedMiddlewareSatisfiesRequireRouteAuthorizer(t *testing.T) {
	t.Cleanup(func() {
		_ = corelog.Close()
		corelog.SetLogger(corelog.Nop())
	})

	runValidateWithOptions(t, validateOptions{withAuthorizer: true, requireRouteAuthorizer: true, logToFile: false})
}

type validateOptions struct {
	withAuthorizer         bool
	requireRouteAuthorizer bool
	logToFile              bool
}

// runValidate boots a minimal application with the real casbin-http Bundle,
// validates it, and returns the log file the run wrote.
func runValidate(t *testing.T, withAuthorizer bool) string {
	t.Helper()
	return runValidateWithOptions(t, validateOptions{withAuthorizer: withAuthorizer, logToFile: true})
}

func runValidateWithOptions(t *testing.T, options validateOptions) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "application.log")
	configPath := writeValidateConfig(t, options, logPath)

	bundles := []plugin.Bundle{web.Bundle(), testEngineBundle(), testPermRouteBundle()}
	if options.withAuthorizer {
		bundles = append(bundles, Bundle())
	}
	app, err := xbc.New(xbc.WithBundles(bundles...))
	if err != nil {
		t.Fatalf("xbc.New() error = %v", err)
	}
	code, err := app.Execute(context.Background(), []string{"validate", "--config", configPath})
	if err != nil || code != 0 {
		t.Fatalf("validate = (%d, %v), want (0, nil)", code, err)
	}
	return logPath
}

func writeValidateConfig(t *testing.T, options validateOptions, logPath string) string {
	t.Helper()
	var logSection string
	if options.logToFile {
		logSection = fmt.Sprintf("  file:\n    enabled: true\n    path: %q\n", logPath)
	}
	var plugins string
	if options.withAuthorizer {
		// Both sections are required: contributing is not enabling, and the
		// middleware needs the engine section to resolve its provider.
		plugins = "  casbin:\n    request_convention: route_permission\n  casbin-http:\n    missing_permission: deny\n"
	}
	var security string
	if options.requireRouteAuthorizer {
		security = "  security:\n    require_route_authorizer: true\n"
	}
	contents := fmt.Sprintf(`xbc:
  shutdown_timeout: 2s
log:
  console:
    enabled: false
%sweb:
  addr: "127.0.0.1:0"
%splugins:
%s`, logSection, security, plugins)
	path := filepath.Join(t.TempDir(), "application.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write validate config: %v", err)
	}
	return path
}

func logLinesContaining(t *testing.T, path, fragment string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file %s: %v", path, err)
	}
	var found []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.Contains(line, fragment) {
			found = append(found, line)
		}
	}
	return found
}
