package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	"github.com/xbcio/xbc/transport/web/extensions/response/biz"
)

// The failure sources the renderer tests drive. errRendererConnection carries a
// credential on purpose: it must never reach a response body, rendered or not.
var (
	errRendererSentinel    = errors.New("renderer sentinel")
	errRendererScoped      = errors.New("renderer scoped sentinel")
	errRendererConnection  = errors.New("dial postgres://svc:private-password@db.internal:5432/app: refused")
	errRendererOrderClosed = biz.NewError("ORDER.CLOSED", "The order is closed.")
)

type rendererDTO struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

// stubRenderer is a configurable web.ErrorRenderer that records what the
// framework handed it. It is the only way to observe the normalized problem a
// renderer sees and whether a response path consulted the renderer at all.
type stubRenderer struct {
	body []byte
	ok   bool

	mu       sync.Mutex
	problems []web.ProblemDetail
	nilCtx   int
}

func (s *stubRenderer) RenderError(c *web.Ctx, problem web.ProblemDetail) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.problems = append(s.problems, problem)
	if c == nil {
		s.nilCtx++
	}
	return s.body, s.ok
}

func (s *stubRenderer) seen() []web.ProblemDetail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]web.ProblemDetail(nil), s.problems...)
}

// panicRenderer is a web.ErrorRenderer that panics on every call, standing in
// for a renderer with a defect. It counts calls so a test can pin that the
// framework asked it for a body before containing the panic.
type panicRenderer struct {
	mu    sync.Mutex
	calls int
}

func (p *panicRenderer) RenderError(*web.Ctx, web.ProblemDetail) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	panic("renderer defect")
}

func (p *panicRenderer) seen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func registerRendererRoutes(router *web.Router) {
	public := web.Public()
	router.GET("/ok", func(_ context.Context, c *web.Ctx) error { return biz.OK(c, "fine") }).Auth(public)
	router.GET("/business", func(context.Context, *web.Ctx) error { return errRendererOrderClosed }).Auth(public)
	router.GET("/unknown", func(context.Context, *web.Ctx) error { return errRendererConnection }).Auth(public)
	router.GET("/validate", func(context.Context, *web.Ctx) error {
		return web.ParamError(validator.New().Struct(rendererDTO{}), &rendererDTO{})
	}).Auth(public)
	router.GET("/panic", func(context.Context, *web.Ctx) error { panic("renderer test panic") }).Auth(public)
	router.POST("/upload", func(_ context.Context, c *web.Ctx) error { c.Status(http.StatusNoContent); return nil }).Auth(public)
	router.GET("/mapped", func(context.Context, *web.Ctx) error { return errRendererSentinel }).Auth(public)
	router.GET("/scoped", web.OnError(web.ErrorMapperFunc(mapRendererScoped)),
		func(context.Context, *web.Ctx) error { return errRendererScoped }).Auth(public)
	router.GET("/stale", func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("Content-Encoding", "gzip")
		c.SetHeader("Content-Length", "999")
		c.SetHeader("ETag", `"v1"`)
		c.SetHeader("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		return errRendererConnection
	}).Auth(public)
	router.GET("/committed", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusOK, "committed")
		web.WriteProblem(c, web.NewProblem(http.StatusTeapot, "late_problem"))
		return nil
	}).Auth(public)
	// No policy and a deny-by-default Server: the authentication middleware
	// answers this one with a 401 before the handler could run.
	router.GET("/secure", func(context.Context, *web.Ctx) error { return nil })
}

// mapRendererSentinel is a server-level mapper whose problem carries no code
// member, the shape a custom mapper is free to produce.
func mapRendererSentinel(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
	if !errors.Is(err, errRendererSentinel) {
		return web.ProblemDetail{}, false
	}
	return web.ProblemDetail{Status: http.StatusConflict, Detail: "Conflicting state."}, true
}

// mapRendererScoped is the route-scoped mapper behind /scoped; unlike the
// server-level one it runs inside a derived, request-scoped resolver.
func mapRendererScoped(_ *web.Ctx, err error) (web.ProblemDetail, bool) {
	if !errors.Is(err, errRendererScoped) {
		return web.ProblemDetail{}, false
	}
	problem := web.NewProblem(http.StatusTeapot, "scoped_teapot")
	problem.Detail = "Scoped failure."
	return problem, true
}

func rendererConfig() web.Config {
	cfg := web.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.MaxRequestBodyBytes = 16
	cfg.Security.Default = web.SecurityDeny
	return cfg
}

func rendererMiddleware(key plugin.Key, middleware web.Middleware) plugin.Entry[web.Middleware] {
	return plugin.Entry[web.Middleware]{Identity: plugin.Identity{Plugin: key}, Value: middleware}
}

// startRendererServer builds a deny-by-default Server over the neutral engine,
// starts it and prepares traffic, which is what freezes the route table the
// authentication middleware and the in-flight gate read. The runtime traffic
// gate stays closed, so no listener serves; tests dispatch through the engine.
// It does not go through newPingServer, which forces Security.Default to permit
// and would make the 401 path unreachable.
func startRendererServer(
	t *testing.T, cfg web.Config, renderer web.ErrorRenderer, middlewares ...plugin.Entry[web.Middleware],
) (*web.Server, *fakeHost) {
	t.Helper()
	host := newFakeHost()
	server := web.NewServer(cfg, enginetest.Factory{}, middlewares,
		[]plugin.Entry[web.ErrorMapper]{{Identity: plugin.Identity{Plugin: "renderertest"}, Value: web.ErrorMapperFunc(mapRendererSentinel)}},
		[]plugin.Entry[web.RouteContributor]{{Identity: plugin.Identity{Plugin: "renderertest"}, Value: fakeRouteContributor{register: registerRendererRoutes}}},
		nil,
		[]plugin.Entry[authentication.Authenticator]{{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubAuth{scheme: "jwt", result: authentication.Rejected("unused")}}},
		[]plugin.Entry[web.CredentialExtractor]{{Identity: plugin.Identity{Plugin: "jwt"}, Value: &stubExtractor{scheme: "jwt", result: authentication.Absent()}}},
	)
	if renderer != nil {
		server.SetErrorRenderer(renderer)
	}
	t.Cleanup(func() {
		if err := server.Stop(context.Background()); err != nil {
			t.Errorf("stopping renderer test server: %v", err)
		}
		host.shutdown()
	})
	ctx := contextFromHost(host)
	require.NoError(t, server.Start(ctx))
	require.NoError(t, server.OpenTraffic(ctx))
	return server, host
}

func newRendererEngine(t *testing.T, renderer web.ErrorRenderer, middlewares ...plugin.Entry[web.Middleware]) *enginetest.Engine {
	t.Helper()
	server, _ := startRendererServer(t, rendererConfig(), renderer, middlewares...)
	return testEngineOf(t, server)
}

// renderedResponse is one response as a client would see it: the status line,
// the headers snapshotted when it was committed, and the full body.
type renderedResponse struct {
	status int
	header http.Header
	body   []byte
}

func serveRenderer(engine *enginetest.Engine, method, path, body string) renderedResponse {
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	result := recorder.Result()
	defer result.Body.Close()
	payload, _ := io.ReadAll(result.Body)
	return renderedResponse{status: result.StatusCode, header: result.Header, body: payload}
}
