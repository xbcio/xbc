package composition_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/authentication"
	apikeycore "github.com/xbcio/xbc/extensions/authentication/apikey"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey"
)

const (
	apiKeySecret  = "0123456789abcdef0123456789abcdef"
	variantToken  = "mcp-token-value-0123456789"
	variantHeader = "X-MCP-Token"
)

const mcpScheme authentication.Scheme = "mcp-token"

var mcpKey plugin.Key = "mcp-token"

func variantDigestForTest() string { return apikeycore.HashKey(variantToken).String() }

var mcpCoreInput = plugin.RefTo[*apikeycore.Plugin](mcpKey)

// mcpCoreDefinition is what a host writes to run a customized apikey variant:
// its own key and configuration path, the core constructor, and WithScheme.
var mcpCoreDefinition = plugin.DefineConfigured(
	mcpKey,
	plugin.ConfigSpec[apikeycore.Config]{
		Defaults: apikeycore.DefaultConfig,
		Prepare: func(cfg apikeycore.Config) (apikeycore.Config, error) {
			return cfg, cfg.Validate()
		},
	},
	func(_ plugin.BuildContext, cfg apikeycore.Config) (*apikeycore.Plugin, error) {
		return apikeycore.New(cfg, apikeycore.WithScheme(mcpScheme))
	},
	plugin.Options[*apikeycore.Plugin]{
		Activation: plugin.WhenConfigured("plugins." + mcpKey.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[authentication.Authenticator](func(value *apikeycore.Plugin) authentication.Authenticator { return value }),
		),
	},
)

var mcpExtractorDefinition = plugin.Define(
	"mcp-token-http",
	func(ctx plugin.BuildContext) (*apikey.Extractor, error) {
		return apikey.New(mcpCoreInput.Get(ctx).Value)
	},
	plugin.Options[*apikey.Extractor]{
		Activation: plugin.WhenConfigured("plugins." + mcpKey.String()),
		Inputs:     plugin.Inputs(mcpCoreInput),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.CredentialExtractor](func(value *apikey.Extractor) web.CredentialExtractor { return value }),
		),
	},
)

// variantHost is the smallest plugin.RuntimeHost that can drive a real Web server
// through Start and traffic preparation.
type variantHost struct {
	execution context.Context
	cancel    context.CancelFunc
	gate      chan struct{}
	gateOnce  sync.Once
	tasks     sync.WaitGroup
}

var _ plugin.RuntimeHost = (*variantHost)(nil)

func newVariantHost() *variantHost {
	execution, cancel := context.WithCancel(context.Background())
	return &variantHost{execution: execution, cancel: cancel, gate: make(chan struct{})}
}

func (host *variantHost) ExecutionContext() context.Context { return host.execution }
func (*variantHost) Logger() log.Logger                     { return log.Nop() }
func (*variantHost) ProcessInstance() string                { return "mcp-token-http-test" }
func (*variantHost) Service() plugin.ServiceIdentity        { return plugin.ServiceIdentity{} }
func (*variantHost) IngressStopBudget() time.Duration       { return 0 }
func (host *variantHost) TrafficGate() <-chan struct{}      { return host.gate }

func (*variantHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*variantHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission {
	return nil
}

func (host *variantHost) SubmitTask(_ plugin.Identity, fn func(context.Context), _ bool) bool {
	if fn == nil {
		return false
	}
	host.tasks.Add(1)
	go func() {
		defer host.tasks.Done()
		fn(host.execution)
	}()
	return true
}

func (*variantHost) RequestShutdown(plugin.Identity, string) bool { return false }

func (host *variantHost) releaseTraffic() { host.gateOnce.Do(func() { close(host.gate) }) }

func (host *variantHost) shutdown() {
	host.cancel()
	host.tasks.Wait()
}

var variantEngineDefinition = plugin.Define(
	"mcp-token-http-test-engine",
	func(plugin.BuildContext) (enginetest.Factory, error) { return enginetest.Factory{}, nil },
	plugin.Options[enginetest.Factory]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.EngineFactory](func(value enginetest.Factory) web.EngineFactory { return value }),
		),
	},
)

type variantRoutes struct{}

func variantRoutesDefinition() plugin.Definition {
	return plugin.Define(
		"mcp-token-http-test-routes",
		func(plugin.BuildContext) (*variantRoutes, error) { return &variantRoutes{}, nil },
		plugin.Options[*variantRoutes]{
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *variantRoutes) web.RouteContributor { return value }),
			),
		},
	)
}

func (routes *variantRoutes) RegisterRoutes(router *web.Router) {
	router.GET("/apikey", routes.apiKey).Name("apikeyvariant.test.apikey").Auth(web.Accepts(apikeycore.Scheme))
	router.GET("/mcp-token", routes.mcpToken).Name("apikeyvariant.test.mcp").Auth(web.Accepts(mcpScheme))
}

func (*variantRoutes) apiKey(_ context.Context, c *web.Ctx) error { return variantWriteSubject(c) }
func (*variantRoutes) mcpToken(_ context.Context, c *web.Ctx) error {
	return variantWriteSubject(c)
}

func variantWriteSubject(c *web.Ctx) error {
	principal, ok := web.CurrentPrincipal(c)
	if !ok {
		return errors.New("mcp-token-http-test: protected route has no principal")
	}
	c.JSON(http.StatusOK, map[string]string{"subject": principal.Subject, "method": principal.AuthMethod})
	return nil
}

type variantApp struct {
	client  *http.Client
	baseURL string
}

// variantAppEnvironment builds the plugins section plus the declared scheme order.
// A nil schemes slice omits web.security.schemes entirely, which is the
// configuration the full-permutation guard must refuse once two
// authenticators are registered.
func variantAppEnvironment(schemes []any) map[string]any {
	webSection := map[string]any{"addr": "127.0.0.1:0"}
	if schemes != nil {
		webSection["security"] = map[string]any{"schemes": schemes}
	}
	return map[string]any{
		"web": webSection,
		"plugins": map[string]any{
			"apikey": map[string]any{
				"static": []any{map[string]any{
					"id":      "legacy",
					"subject": "service:legacy",
					"sha256":  apikeycore.HashKey(apiKeySecret).String(),
				}},
			},
			"mcp-token": map[string]any{
				"header":        variantHeader,
				"allow_bearer":  false,
				"bearer_scheme": "mcp-token",
				"ignore_app_id": true,
				"min_key_bytes": 16,
				"static": []any{map[string]any{
					"id":      "mcp",
					"subject": "service:mcp",
					"sha256":  variantDigestForTest(),
				}},
			},
		},
	}
}

func variantBundles() []plugin.Bundle {
	return []plugin.Bundle{
		web.Bundle(),
		apikey.Bundle(),
		plugin.BundleOf(mcpCoreDefinition),
		plugin.BundleOf(mcpExtractorDefinition),
		plugin.BundleOf(variantEngineDefinition),
		plugin.BundleOf(variantRoutesDefinition()),
	}
}

// buildVariantPlan resolves configuration and builds the composition, reporting a
// configuration error either stage rejects.
func buildVariantPlan(t *testing.T, schemes []any) (*assembly.Plan, error) {
	t.Helper()
	environment, err := config.NewEnvironment(variantAppEnvironment(schemes), "")
	if err != nil {
		t.Fatalf("config.NewEnvironment() error = %v", err)
	}
	return assembly.BuildPlan(assembly.PlanOptions{Bundles: variantBundles(), Env: environment})
}

// variantBuildError returns the configuration error a rejected composition raises at
// build time.
func variantBuildError(t *testing.T, schemes []any) error {
	t.Helper()
	_, err := buildVariantPlan(t, schemes)
	if err == nil {
		t.Fatal("BuildPlan() succeeded, want a configuration error")
	}
	return err
}

// composeVariantApp builds and constructs the application without starting it.
func composeVariantApp(t *testing.T, schemes []any) (*assembly.Instance, *variantHost) {
	t.Helper()
	built, err := buildVariantPlan(t, schemes)
	if err != nil {
		t.Fatalf("assembly.BuildPlan() error = %v", err)
	}
	host := newVariantHost()
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(host, identity)
		},
	})
	if err != nil {
		t.Fatalf("assembly.Construct() error = %v", err)
	}
	webInstance, found := constructed.Instance(plugin.Identity{Plugin: web.Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("Web server instance not constructed")
	}
	return webInstance, host
}

// startVariantApp composes the Web server, both authentication Bundles, the
// neutral test engine, and the test routes, then opens traffic.
func startVariantApp(t *testing.T, schemes []any) *variantApp {
	t.Helper()
	webInstance, host := composeVariantApp(t, schemes)
	server, ok := webInstance.Primary().(*web.Server)
	if !ok {
		t.Fatalf("Web server primary = %T", webInstance.Primary())
	}
	transport := &http.Transport{}
	app := &variantApp{
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = webInstance.StopBounded(ctx, 5*time.Second)
		host.shutdown()
		transport.CloseIdleConnections()
	})
	if err := webInstance.InvokeStart(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := webInstance.InvokeTrafficPreparation(); err != nil {
		t.Fatalf("traffic preparation error = %v", err)
	}
	host.releaseTraffic()
	variantWaitFor(t, 10*time.Second, "server to bind an address", func() bool { return server.Addr() != "" })
	app.baseURL = "http://" + server.Addr()
	return app
}

// variantStartError starts a composition expected to fail and returns the error.
func variantStartError(t *testing.T, schemes []any) error {
	t.Helper()
	webInstance, host := composeVariantApp(t, schemes)
	err := webInstance.InvokeStart()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = webInstance.StopBounded(ctx, time.Second)
	host.shutdown()
	if err == nil {
		t.Fatal("Start() succeeded, want a configuration error")
	}
	return err
}

func (app *variantApp) do(t *testing.T, path string, headers map[string][]string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, app.baseURL+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := app.client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read GET %s body: %v", path, err)
	}
	return response, string(body)
}

func variantWaitFor(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

type variantCapturedLogEntry struct {
	level string
	body  string
}

type variantCaptureLogger struct {
	mu      sync.Mutex
	entries []variantCapturedLogEntry
}

func (logger *variantCaptureLogger) record(level, message string, kv []any) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.entries = append(logger.entries, variantCapturedLogEntry{level: level, body: fmt.Sprintf("%s %v", message, kv)})
}

func (logger *variantCaptureLogger) Debug(message string, kv ...any) {
	logger.record("debug", message, kv)
}
func (logger *variantCaptureLogger) Info(message string, kv ...any) {
	logger.record("info", message, kv)
}
func (logger *variantCaptureLogger) Warn(message string, kv ...any) {
	logger.record("warn", message, kv)
}
func (logger *variantCaptureLogger) Error(message string, kv ...any) {
	logger.record("error", message, kv)
}
func (*variantCaptureLogger) Fatal(string, ...any)          {}
func (logger *variantCaptureLogger) With(...any) log.Logger { return logger }
func (*variantCaptureLogger) Enabled(log.Level) bool        { return true }

func (logger *variantCaptureLogger) snapshot() []variantCapturedLogEntry {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return append([]variantCapturedLogEntry(nil), logger.entries...)
}

func variantCaptureLogs(t *testing.T) *variantCaptureLogger {
	t.Helper()
	previous := log.L()
	capture := &variantCaptureLogger{}
	log.SetLogger(capture)
	t.Cleanup(func() { log.SetLogger(previous) })
	return capture
}

func variantAssertStatus(t *testing.T, label string, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		t.Fatalf("%s status = %d, want %d", label, response.StatusCode, want)
	}
}

func variantAssertChallenge(t *testing.T, label string, response *http.Response, want string) {
	t.Helper()
	values := response.Header.Values("WWW-Authenticate")
	if len(values) != 1 || values[0] != want {
		t.Fatalf("%s WWW-Authenticate = %#v, want [%q]", label, values, want)
	}
}

func variantAssertProblem(t *testing.T, label, body, wantDetail string) {
	t.Helper()
	if !strings.Contains(body, `"detail":"`+wantDetail+`"`) {
		t.Fatalf("%s body = %q, want problem detail %q", label, body, wantDetail)
	}
}

func variantAssertNoSecretText(t *testing.T, label, text string) {
	t.Helper()
	for _, secret := range []string{apiKeySecret, variantToken} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaked credential material: %q", label, text)
		}
	}
	digest := variantDigestForTest()
	if strings.Contains(strings.ToLower(text), digest) || strings.Contains(strings.ToLower(text), apikeycore.HashKey(apiKeySecret).String()) {
		t.Fatalf("%s leaked the token digest: %q", label, text)
	}
}

// TestAPIKeyAndAPIKeyVariantCoexistWithoutCrossAuthentication mounts both
// authenticators in one application and drives the real middleware. Each route
// names one scheme, and only that scheme's credential source may authenticate
// it: the API-key header must not satisfy the mcp-token route and the
// configured mcp-token header must not satisfy the apikey route.
func TestAPIKeyAndAPIKeyVariantCoexistWithoutCrossAuthentication(t *testing.T) {
	app := startVariantApp(t, []any{"apikey", "mcp-token"})

	apiKeyHeaders := map[string][]string{"X-API-Key": {apiKeySecret}}
	mcpHeaders := map[string][]string{variantHeader: {variantToken}}

	apikeyOK, apikeyBody := app.do(t, "/apikey", apiKeyHeaders)
	variantAssertStatus(t, "apikey route with API key", apikeyOK, http.StatusOK)
	if !strings.Contains(apikeyBody, `"subject":"service:legacy"`) || !strings.Contains(apikeyBody, `"method":"apikey"`) {
		t.Fatalf("apikey route body = %q", apikeyBody)
	}

	mcpOK, mcpBody := app.do(t, "/mcp-token", mcpHeaders)
	variantAssertStatus(t, "mcp-token route with MCP token", mcpOK, http.StatusOK)
	if !strings.Contains(mcpBody, `"subject":"service:mcp"`) || !strings.Contains(mcpBody, `"method":"mcp-token"`) {
		t.Fatalf("mcp-token route body = %q", mcpBody)
	}

	// Cross-use is refused on both sides.
	apiKeyOnMCP, body := app.do(t, "/mcp-token", apiKeyHeaders)
	variantAssertStatus(t, "mcp-token route with API key only", apiKeyOnMCP, http.StatusUnauthorized)
	variantAssertChallenge(t, "mcp-token route with API key only", apiKeyOnMCP, "mcp-token")
	variantAssertProblem(t, "mcp-token route with API key only", body, "authentication required")
	variantAssertNoSecretText(t, "cross-use rejection body", body)

	mcpOnAPIKey, body := app.do(t, "/apikey", mcpHeaders)
	variantAssertStatus(t, "apikey route with MCP token only", mcpOnAPIKey, http.StatusUnauthorized)
	variantAssertChallenge(t, "apikey route with MCP token only", mcpOnAPIKey, "Bearer")
	variantAssertProblem(t, "apikey route with MCP token only", body, "authentication required")

	// The variant ignores the app ID entirely: a padded token alongside a
	// repeated X-App-ID and a foreign Authorization still authenticates.
	ignored, _ := app.do(t, "/mcp-token", map[string][]string{
		variantHeader:   {variantToken},
		"X-App-ID":      {"a", "b"},
		"Authorization": {"Basic abc"},
	})
	variantAssertStatus(t, "mcp-token route with unrelated headers", ignored, http.StatusOK)

	// The mcp-token extractor reads exactly one header: Authorization and
	// an app-ID header are not credential sources here.
	for _, header := range []string{"Authorization", "X-App-ID", "X-API-Key"} {
		value := "Bearer " + variantToken
		if header == "X-App-ID" {
			value = "checkout"
		}
		response, body := app.do(t, "/mcp-token", map[string][]string{header: {value}})
		variantAssertStatus(t, "mcp-token route with "+header, response, http.StatusUnauthorized)
		variantAssertChallenge(t, "mcp-token route with "+header, response, "mcp-token")
		variantAssertNoSecretText(t, "mcp-token rejection body", body)
	}

	// The apikey extractor still honors Authorization for its own route when
	// AllowBearer is in effect, proving coexistence did not narrow apikey.
	bearerOK, bearerBody := app.do(t, "/apikey", map[string][]string{
		"Authorization": {"Bearer " + apiKeySecret},
	})
	variantAssertStatus(t, "apikey route with Authorization Bearer", bearerOK, http.StatusOK)
	if !strings.Contains(bearerBody, `"subject":"service:legacy"`) {
		t.Fatalf("apikey bearer body = %q", bearerBody)
	}

	// Both credentials at once: each route authenticates through its own
	// source, and the unrelated header is never inspected.
	bothForAPIKey, _ := app.do(t, "/apikey", map[string][]string{
		"X-API-Key": {apiKeySecret}, variantHeader: {variantToken},
	})
	variantAssertStatus(t, "apikey route with both credentials", bothForAPIKey, http.StatusOK)
	bothForMCP, _ := app.do(t, "/mcp-token", map[string][]string{
		"X-API-Key": {apiKeySecret}, variantHeader: {variantToken},
	})
	variantAssertStatus(t, "mcp-token route with both credentials", bothForMCP, http.StatusOK)
}

// TestAPIKeyVariantRejectionsStayGenericAndRedacted exercises the middleware's
// rendering of every mcp-token failure: malformed headers get their
// extraction-only diagnosis, wrong and short secrets collapse into one generic
// reason, and no response body contains the secret or its digest.
func TestAPIKeyVariantRejectionsStayGenericAndRedacted(t *testing.T) {
	capture := variantCaptureLogs(t)
	app := startVariantApp(t, []any{"apikey", "mcp-token"})

	tests := []struct {
		name       string
		headers    map[string][]string
		wantDetail string
	}{
		{
			name:       "wrong secret is generic",
			headers:    map[string][]string{variantHeader: {variantToken + "-wrong"}},
			wantDetail: "invalid credential",
		},
		{
			name:       "short secret is generic",
			headers:    map[string][]string{variantHeader: {"short"}},
			wantDetail: "invalid credential",
		},
		{
			name:       "duplicate header is malformed",
			headers:    map[string][]string{variantHeader: {variantToken, variantToken}},
			wantDetail: "duplicate api key header",
		},
		{
			name:       "blank header is malformed",
			headers:    map[string][]string{variantHeader: {"   "}},
			wantDetail: "empty api key header",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response, body := app.do(t, "/mcp-token", test.headers)
			variantAssertStatus(t, test.name, response, http.StatusUnauthorized)
			variantAssertChallenge(t, test.name, response, "mcp-token")
			variantAssertProblem(t, test.name, body, test.wantDetail)
			variantAssertNoSecretText(t, test.name+" body", body)
		})
	}

	allowed, _ := app.do(t, "/mcp-token", map[string][]string{variantHeader: {variantToken}})
	variantAssertStatus(t, "valid MCP token after failures", allowed, http.StatusOK)

	for _, entry := range capture.snapshot() {
		variantAssertNoSecretText(t, "log entry ("+entry.level+")", entry.body)
	}
}

// TestSchemePermutationIsEnforced mirrors the framework rule at the composition
// level: with two authenticators present, web.security.schemes must be a full
// permutation. A reversed order is accepted because order is declared, never
// inferred from plugin construction or name.
func TestSchemePermutationIsEnforced(t *testing.T) {
	t.Run("omitted with two authenticators", func(t *testing.T) {
		err := variantStartError(t, nil)
		if !strings.Contains(err.Error(), "web.security.schemes must declare the arbitration order for 2 registered authenticators") {
			t.Fatalf("Start() error = %v", err)
		}
		if strings.Contains(err.Error(), variantToken) || strings.Contains(err.Error(), variantDigestForTest()) {
			t.Fatalf("Start() error leaked credential material: %v", err)
		}
	})

	t.Run("partial permutation", func(t *testing.T) {
		err := variantStartError(t, []any{"apikey"})
		if !strings.Contains(err.Error(), `web.security.schemes is missing registered scheme "mcp-token"`) {
			t.Fatalf("Start() error = %v", err)
		}
	})

	t.Run("unknown scheme", func(t *testing.T) {
		err := variantStartError(t, []any{"apikey", "mcp-token", "mtls"})
		if !strings.Contains(err.Error(), `web.security.schemes declares unknown scheme "mtls"`) {
			t.Fatalf("Start() error = %v", err)
		}
	})

	t.Run("duplicate scheme", func(t *testing.T) {
		err := variantBuildError(t, []any{"apikey", "mcp-token", "apikey"})
		if !strings.Contains(err.Error(), `security schemes has a duplicate scheme "apikey"`) {
			t.Fatalf("BuildPlan() error = %v", err)
		}
	})

	t.Run("reversed declaration is honored", func(t *testing.T) {
		app := startVariantApp(t, []any{"mcp-token", "apikey"})
		mcpResp, _ := app.do(t, "/mcp-token", map[string][]string{variantHeader: {variantToken}})
		variantAssertStatus(t, "mcp-token route under reversed order", mcpResp, http.StatusOK)
		apiKey, _ := app.do(t, "/apikey", map[string][]string{"X-API-Key": {apiKeySecret}})
		variantAssertStatus(t, "apikey route under reversed order", apiKey, http.StatusOK)
	})
}
