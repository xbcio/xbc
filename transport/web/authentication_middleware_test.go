package web_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

type stubAuth struct {
	scheme authentication.Scheme
	result authentication.Result
	// err, when non-nil, is returned as the authenticator's own operational
	// failure instead of result -- the manager wraps it as an OperationalError
	// before the middleware ever sees it. Zero value nil, so every existing
	// literal that omits this field keeps returning result unchanged.
	err error
	// calls counts Authenticate invocations so a test can assert that a request
	// reached, or never reached, policy resolution.
	calls int
}

func (s *stubAuth) Scheme() authentication.Scheme { return s.scheme }

func (s *stubAuth) Authenticate(
	context.Context, authentication.Credential,
) (authentication.Result, error) {
	s.calls++
	return s.result, s.err
}

// buildTestAuthMiddleware wires a middleware over one scheme and one frozen
// route, mirroring what the Server does at startup.
func buildTestAuthMiddleware(
	t *testing.T,
	cfg web.SecurityConfig,
	routes []web.RouteInfo,
	extraction authentication.CredentialResult,
	authenticator *stubAuth,
) *web.AuthenticationMiddleware {
	t.Helper()
	middleware, err := web.NewAuthenticationMiddleware(
		cfg,
		[]plugin.Entry[authentication.Authenticator]{
			{
				Identity: plugin.Identity{Plugin: "jwt"},
				Value:    authenticator,
			},
		},
		[]plugin.Entry[web.CredentialExtractor]{
			{
				Identity: plugin.Identity{Plugin: "jwt"},
				Value:    &stubExtractor{scheme: "jwt", result: extraction},
			},
		},
	)
	if err != nil {
		t.Fatalf("newAuthenticationMiddleware() error = %v", err)
	}
	if err := middleware.RoutesReady(web.NewRouteCatalog(routes)); err != nil {
		t.Fatalf("RoutesReady() error = %v", err)
	}
	return middleware
}

// recordRoute stands in for recordCurrentRoute, which a Router bakes into the
// front of every flattened chain. These tests register their route on the
// engine directly, so they install the stand-in themselves -- and one of them
// relies on being able to record a route the compiled catalog does not know,
// which a real Router could never produce.
func recordRoute(route web.RouteInfo) web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		web.SetCurrentRoute(c, route)
		return nil
	}
}

func okHandler(_ context.Context, c *web.Ctx) error {
	c.Status(http.StatusOK)
	return nil
}

func notFoundHandler(_ context.Context, c *web.Ctx) error {
	c.Status(http.StatusNotFound)
	return nil
}

func runThroughAuth(
	t *testing.T,
	middleware *web.AuthenticationMiddleware,
	route web.RouteInfo,
	registerRoute bool,
) *httptest.ResponseRecorder {
	t.Helper()
	engine := enginetest.New()
	if registerRoute {
		engine.Use(recordRoute(route))
	}
	engine.Use(middleware.Handler())
	engine.GET(route.Path, okHandler)
	// This stands in for the Server's own assembly: (*Server).Start splices the
	// global chain into the NoRoute and NoMethod chains, so the middleware must
	// be listed here for this hand-built engine to have the topology a Server
	// actually dispatches against.
	engine.NoRoute([]web.Handler{middleware.Handler(), notFoundHandler})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(route.Method, route.Path, nil))
	return recorder
}

func TestAuthenticationMiddlewarePassesUnmatchedRouteWithoutResolving(t *testing.T) {
	t.Parallel()

	// The compiled table is deliberately populated with a denying route: a
	// request that matched no frozen route must fall through before any lookup,
	// so an implementation that dropped the CurrentRoute check would land in the
	// uncompiled-route branch and answer 500 instead of the engine's 404.
	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	authenticator := &stubAuth{scheme: "jwt", result: authentication.Rejected("should not run")}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{route},
		authentication.Absent(),
		authenticator,
	)

	engine := enginetest.New()
	// No recordCurrentRoute stand-in: CurrentRoute misses, exactly as it does
	// for a request the engine resolves through its NoRoute chain.
	engine.Use(middleware.Handler())
	engine.GET(route.Path, okHandler)
	engine.NoRoute([]web.Handler{middleware.Handler(), notFoundHandler})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/missing", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d from the engine's NoRoute chain", recorder.Code, http.StatusNotFound)
	}
	if authenticator.calls != 0 {
		t.Fatalf(
			"authenticator calls = %d, want 0: an unmatched route must not resolve a policy",
			authenticator.calls,
		)
	}
}

func TestAuthenticationMiddlewareFailsClosedOnUncompiledRoute(t *testing.T) {
	t.Parallel()

	// A recorded route that the compiled table does not know can only mean the
	// table is stale or was never built. That must fail closed, not release the
	// request unauthenticated.
	public := web.Public()
	authenticator := &stubAuth{scheme: "jwt", result: authentication.Rejected("should not run")}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{{Method: http.MethodGet, Path: "/health", Auth: &public}},
		authentication.Absent(),
		authenticator,
	)

	got := runThroughAuth(t, middleware, web.RouteInfo{Method: http.MethodGet, Path: "/orders"}, true)
	if got.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", got.Code, http.StatusInternalServerError)
	}
	if authenticator.calls != 0 {
		t.Fatalf("authenticator calls = %d, want 0", authenticator.calls)
	}
}

func TestAuthenticationMiddlewarePermitsPublicRoute(t *testing.T) {
	t.Parallel()

	public := web.Public()
	route := web.RouteInfo{Method: http.MethodGet, Path: "/health", Auth: &public}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{route},
		authentication.Absent(),
		&stubAuth{scheme: "jwt", result: authentication.Rejected("should not run")},
	)

	if got := runThroughAuth(t, middleware, route, true); got.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", got.Code, http.StatusOK)
	}
}

func TestAuthenticationMiddlewarePublishesPrincipalOnce(t *testing.T) {
	t.Parallel()

	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{route},
		authentication.Presented("token"),
		&stubAuth{
			scheme: "jwt",
			result: authentication.Accepted(web.Principal{Subject: "u-1", AuthMethod: "jwt"}),
		},
	)

	engine := enginetest.New()
	engine.Use(recordRoute(route))
	engine.Use(middleware.Handler())

	var seen web.Principal
	var found bool
	engine.GET("/orders", func(_ context.Context, c *web.Ctx) error {
		seen, found = web.CurrentPrincipal(c)
		c.Status(http.StatusOK)
		return nil
	})

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/orders", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !found {
		t.Fatal("framework must publish the Principal after a successful authentication")
	}
	if seen.Subject != "u-1" || seen.AuthMethod != "jwt" {
		t.Fatalf("principal = %+v, want subject u-1 / method jwt", seen)
	}
}

func TestAuthenticationMiddlewareRejectsWithSeparateChallengeHeaders(t *testing.T) {
	t.Parallel()

	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{route},
		authentication.AbsentWithChallenge(`Bearer realm="api"`),
		&stubAuth{scheme: "jwt", result: authentication.Rejected("unused")},
	)

	got := runThroughAuth(t, middleware, route, true)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got.Code, http.StatusUnauthorized)
	}
	challenges := got.Header().Values("WWW-Authenticate")
	if len(challenges) != 1 || challenges[0] != `Bearer realm="api"` {
		t.Fatalf("WWW-Authenticate = %v, want one Bearer challenge", challenges)
	}
}

// TestAuthenticationMiddlewarePublishesExemptSignalToDownstream is the
// producer-side counterpart to casbin's and tenant's exempt-consumer tests:
// until this test existed, nothing in the repository drove a request through
// the real authentication middleware and read AuthenticationExempt back from a
// downstream middleware sharing the same *web.Ctx. casbin and tenant only ever
// simulated the flag by setting the context key directly, so a producer-side
// regression -- markAuthenticationExempt deleted, or hoisted to run
// unconditionally -- left every test in the repository green. Both mutations
// are opposite-direction production incidents: the first 403s every permitted
// route, the second bypasses casbin and tenant on every protected one.
func TestAuthenticationMiddlewarePublishesExemptSignalToDownstream(t *testing.T) {
	t.Parallel()

	public := web.Public()
	permitRoute := web.RouteInfo{Method: http.MethodGet, Path: "/health", Auth: &public}
	denyRoute := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{permitRoute, denyRoute},
		authentication.Presented("token"),
		&stubAuth{
			scheme: "jwt",
			result: authentication.Accepted(web.Principal{Subject: "u-1", AuthMethod: "jwt"}),
		},
	)

	// observe stands in for a Require(AuthenticationMiddlewareKey) consumer such
	// as casbin or tenant: it reads AuthenticationExempt and CurrentPrincipal
	// from the same Ctx the authentication middleware just populated, exactly as
	// those two extensions do in production.
	observe := func(exempt, hadPrincipal *bool) web.Handler {
		return func(_ context.Context, c *web.Ctx) error {
			*exempt = web.AuthenticationExempt(c)
			_, *hadPrincipal = web.CurrentPrincipal(c)
			return nil
		}
	}
	buildEngine := func(route web.RouteInfo, registerRoute bool) (engine *enginetest.Engine, exempt, hadPrincipal *bool) {
		exempt = new(bool)
		hadPrincipal = new(bool)
		engine = enginetest.New()
		if registerRoute {
			engine.Use(recordRoute(route))
		}
		engine.Use(middleware.Handler())
		engine.Use(observe(exempt, hadPrincipal))
		engine.GET(route.Path, okHandler)
		engine.NoRoute([]web.Handler{middleware.Handler(), observe(exempt, hadPrincipal), notFoundHandler})
		return engine, exempt, hadPrincipal
	}

	t.Run("permit route", func(t *testing.T) {
		engine, exempt, _ := buildEngine(permitRoute, true)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(permitRoute.Method, permitRoute.Path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
		if !*exempt {
			t.Fatal("AuthenticationExempt() = false on a permit route, want true")
		}
	})

	t.Run("deny route authenticates", func(t *testing.T) {
		engine, exempt, hadPrincipal := buildEngine(denyRoute, true)
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(denyRoute.Method, denyRoute.Path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
		if *exempt {
			t.Fatal("AuthenticationExempt() = true on an authenticated deny route, want false: " +
				"this would bypass casbin and tenant on every protected route")
		}
		if !*hadPrincipal {
			t.Fatal("CurrentPrincipal() missing after a successful authentication")
		}
	})

	t.Run("unmatched route", func(t *testing.T) {
		unmatched := web.RouteInfo{Method: http.MethodGet, Path: "/does-not-exist"}
		// No recordCurrentRoute stand-in: CurrentRoute misses, exactly as it
		// does for a request the engine resolves through its NoRoute chain.
		engine, exempt, hadPrincipal := buildEngine(permitRoute, false)

		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(unmatched.Method, unmatched.Path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
		}
		if *exempt {
			t.Fatal("AuthenticationExempt() = true on an unmatched route, want false")
		}
		if *hadPrincipal {
			t.Fatal("CurrentPrincipal() present on an unmatched route, want absent")
		}
	})
}

// TestAuthenticationExemptContextKeyIsStable pins the exact string literal
// markAuthenticationExempt writes and AuthenticationExempt reads. casbin and
// tenant cannot import the private authenticationExemptContextKey constant, so
// each mirrors it as its own hardcoded test-only literal
// (authenticationExemptKeyForTest) to simulate an exempt request without
// driving the real middleware. Those two mirrors do catch a drift in the
// constant's value, but nothing inside this package did: every call here goes
// through the exported functions, so both sides of a value change move
// together and stay green. Hardcoding the literal independently here closes
// that gap: a change to authenticationExemptContextKey's value now fails in
// this package too, not only in the two downstream modules.
func TestAuthenticationExemptContextKeyIsStable(t *testing.T) {
	t.Parallel()

	c := enginetest.NewCtx(httptest.NewRecorder(), nil)
	c.Set("xbc/transport/web.authenticationExempt", true)
	if !web.AuthenticationExempt(c) {
		t.Fatal("AuthenticationExempt() = false after setting the documented literal " +
			`"xbc/transport/web.authenticationExempt" directly, want true`)
	}
}

// authCaptureLogger records what abortAuthenticationFailure hands to the
// resolver so a test can assert on the log the way an operator would read it.
type authCaptureLogger struct {
	mu      sync.Mutex
	entries []authCaptureEntry
}

type authCaptureEntry struct {
	msg    string
	fields []any
}

func (l *authCaptureLogger) add(msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, authCaptureEntry{msg: msg, fields: append([]any(nil), fields...)})
}

func (l *authCaptureLogger) Debug(msg string, kv ...any) { l.add(msg, kv...) }
func (l *authCaptureLogger) Info(msg string, kv ...any)  { l.add(msg, kv...) }
func (l *authCaptureLogger) Warn(msg string, kv ...any)  { l.add(msg, kv...) }
func (l *authCaptureLogger) Error(msg string, kv ...any) { l.add(msg, kv...) }
func (*authCaptureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *authCaptureLogger) With(...any) log.Logger      { return l }
func (*authCaptureLogger) Enabled(log.Level) bool        { return true }

// rendered joins each entry the way the real logger does: only err.Error() is
// encoded, never the unwrapped chain. A test asserting on this string therefore
// sees exactly what would reach the log sink.
func (l *authCaptureLogger) rendered() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, entry := range l.entries {
		b.WriteString(entry.msg)
		for _, field := range entry.fields {
			if err, ok := field.(error); ok {
				b.WriteString(" " + err.Error())
				continue
			}
			b.WriteString(" " + fmt.Sprint(field))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// runThroughAuthWithLogger drives one request with a logger-bearing error
// resolver attached, which is what the Server does at startup. Without the
// attach step resolverFor falls back to a no-op logger and nothing can be
// observed.
func runThroughAuthWithLogger(
	t *testing.T,
	middleware *web.AuthenticationMiddleware,
	route web.RouteInfo,
) (*httptest.ResponseRecorder, *authCaptureLogger) {
	t.Helper()
	logger := &authCaptureLogger{}
	resolver := web.NewErrorResolver(logger)

	engine := enginetest.New()
	engine.Use(func(_ context.Context, c *web.Ctx) error {
		resolver.Attach(c)
		return nil
	})
	engine.Use(recordRoute(route))
	engine.Use(middleware.Handler())
	engine.GET(route.Path, okHandler)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(route.Method, route.Path, nil))
	return recorder, logger
}

// TestAuthenticationFailureLogsOperationalErrorWithoutItsCause pins both halves
// of the policy for the one failure site whose cause arrives from outside the
// framework.
//
// The failing half it was written for: the middleware used to call c.Error(err)
// and rely on the error boundary to log it, but AbortProblem writes the
// response first and the engine's error drain skips a request that already has
// one. The cause reached no mapper and no log -- it evaporated.
//
// The other half is the eliding contract: authentication.OperationalError
// renders its operation and scheme but never its wrapped cause, because an
// extractor's cause may quote the credential it rejected. The marker below must
// therefore appear in neither the response nor the log; an "improved"
// implementation that unwraps the chain before logging fails here.
func TestAuthenticationFailureLogsOperationalErrorWithoutItsCause(t *testing.T) {
	t.Parallel()

	const marker = "Bearer eyJhbGciOi-LEAKED-CREDENTIAL"
	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware, err := web.NewAuthenticationMiddleware(
		web.SecurityConfig{Default: web.SecurityDeny},
		[]plugin.Entry[authentication.Authenticator]{
			{
				Identity: plugin.Identity{Plugin: "jwt"},
				Value:    &stubAuth{scheme: "jwt", result: authentication.Rejected("should not run")},
			},
		},
		[]plugin.Entry[web.CredentialExtractor]{
			{
				Identity: plugin.Identity{Plugin: "jwt"},
				Value: &stubExtractor{
					scheme: "jwt",
					err:    fmt.Errorf("malformed authorization header %q", marker),
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("newAuthenticationMiddleware() error = %v", err)
	}
	if err := middleware.RoutesReady(web.NewRouteCatalog([]web.RouteInfo{route})); err != nil {
		t.Fatalf("RoutesReady() error = %v", err)
	}

	response, logger := runThroughAuthWithLogger(t, middleware, route)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	logged := logger.rendered()
	if !strings.Contains(logged, "credential collection") || !strings.Contains(logged, `"jwt"`) {
		t.Fatalf(
			"log = %q, want the operational summary naming the failed operation and scheme: "+
				"an internal authentication failure must leave a server-side trace",
			logged,
		)
	}
	if strings.Contains(logged, marker) {
		t.Fatalf(
			"log = %q, must not contain the rejected credential %q: "+
				"only err.Error() may be logged, never the unwrapped cause",
			logged, marker,
		)
	}
	if body := response.Body.String(); strings.Contains(body, marker) {
		t.Fatalf("response body = %q, must not contain the rejected credential %q", body, marker)
	}
}

// TestAuthenticationFailureKeepsCauseOutOfResponse covers a second failure site
// -- one whose cause the framework builds itself and is therefore fully
// rendered by Error(). That makes it the site that discriminates on the
// response: the full text is available to leak, so an implementation that put
// the cause in ProblemDetail.Detail, or routed it through the OnError mapper
// chain where an application mapper could surface it, fails here.
func TestAuthenticationFailureKeepsCauseOutOfResponse(t *testing.T) {
	t.Parallel()

	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{route},
		authentication.Presented("token"),
		// A principal that is not a web.Principal: the manager accepts it, the
		// middleware cannot publish it, and the resulting error names the type.
		&stubAuth{scheme: "jwt", result: authentication.Accepted("not-a-web-principal")},
	)

	response, logger := runThroughAuthWithLogger(t, middleware, route)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	body := response.Body.String()
	if strings.Contains(body, "web.Principal") || strings.Contains(body, "not-a-web-principal") {
		t.Fatalf(
			"response body = %q, must not describe the internal cause: the response is fixed "+
				"for every cause so it cannot be used as an oracle",
			body,
		)
	}
	if !strings.Contains(body, "authentication_failed") {
		t.Fatalf("response body = %q, want the generic authentication_failed problem", body)
	}
	if logged := logger.rendered(); !strings.Contains(logged, "want web.Principal") {
		t.Fatalf(
			"log = %q, want the full cause: this one is framework-built and safe, "+
				"so eliding it would leave the failure undiagnosable",
			logged,
		)
	}
}

// TestAuthenticationFailureKeepsAuthenticatorErrorGeneric covers a third
// failure site -- the authenticator's own Authenticate error, wrapped by the
// manager into an OperationalError before this middleware ever sees it. It is
// the most dangerous of the three sites: the cause originates from a
// third-party authenticator (jwt/session/apikey), the source most likely to
// carry credential material.
//
// The failing implementation this pins: the middleware body has an error
// return now, so "return err" instead of "abortAuthenticationFailure(c, err);
// return nil" type-checks. That routes the error through Handle's AbortError
// and the OnError mapper chain -- which renders "internal_server_error", not
// the fixed "authentication_failed" problem -- and would let an
// application-registered mapper that recognizes the authenticator's error type
// turn it into a revealing response. No test before this one drove the manager
// error non-nil at this exact call site with a body assertion strict enough to
// tell the two implementations apart;
// TestAuthenticationFailureLogsOperationalErrorWithoutItsCause only checked
// status and log content, and both stay identical between "authentication_
// failed" and "internal_server_error" because OperationalError.Error() is
// safe either way it is logged.
func TestAuthenticationFailureKeepsAuthenticatorErrorGeneric(t *testing.T) {
	t.Parallel()

	const marker = "operational-secret-should-not-leak"
	route := web.RouteInfo{Method: http.MethodGet, Path: "/orders"}
	middleware := buildTestAuthMiddleware(
		t,
		web.SecurityConfig{Default: web.SecurityDeny},
		[]web.RouteInfo{route},
		authentication.Presented("token"),
		&stubAuth{scheme: "jwt", err: fmt.Errorf("verify signature: %s", marker)},
	)

	response, logger := runThroughAuthWithLogger(t, middleware, route)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	body := response.Body.String()
	if strings.Contains(body, marker) {
		t.Fatalf(
			"response body = %q, must not contain the authenticator's cause %q",
			body, marker,
		)
	}
	if !strings.Contains(body, "authentication_failed") {
		t.Fatalf(
			"response body = %q, want the generic authentication_failed problem: "+
				"routing this error through the OnError mapper chain instead of "+
				"abortAuthenticationFailure produces internal_server_error instead",
			body,
		)
	}
	// OperationalError.Error() deliberately omits the wrapped cause (see
	// authentication.OperationalError), so the log can only ever carry the
	// safe operation-and-scheme summary -- never the marker. Asserting that
	// summary is present is what rules out the failure silently swallowing
	// the error instead of logging it.
	logged := logger.rendered()
	if strings.Contains(logged, marker) {
		t.Fatalf(
			"log = %q, must not contain the authenticator's cause %q: "+
				"only the safe operation and scheme may reach the log",
			logged, marker,
		)
	}
	if !strings.Contains(logged, "authentication failed") || !strings.Contains(logged, `"jwt"`) {
		t.Fatalf(
			"log = %q, want the operational summary naming the failed operation and scheme: "+
				"an internal authentication failure must leave a server-side trace",
			logged,
		)
	}
}
