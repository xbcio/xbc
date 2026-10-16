package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
	"github.com/xbcio/xbc/transport/web/extensions/response/biz"
)

const rendererJSONContentType = "application/json; charset=utf-8"

// rendererFailureCases is one failure per path that leaves through
// WriteProblem: a handler error mapped by biz, an unmapped error, a validation
// failure, the panic boundary, the framework's own 404 and 405, the
// authentication middleware, and the body limit.
var rendererFailureCases = []struct {
	name, method, path, body string
	status                   int
	code                     string
	message                  string
}{
	{"business error", http.MethodGet, "/business", "", 422, "ORDER.CLOSED", "The order is closed."},
	{"unknown error", http.MethodGet, "/unknown", "", 500, "internal_server_error", "Internal Server Error"},
	{"validation failure", http.MethodGet, "/validate", "", 400, "validation_failed", "Request validation failed."},
	{"panic", http.MethodGet, "/panic", "", 500, "internal_server_error", "Internal Server Error"},
	{"not found", http.MethodGet, "/missing", "", 404, "not_found", "Not Found"},
	{"method not allowed", http.MethodPost, "/ok", "", 405, "method_not_allowed", "Method Not Allowed"},
	{"unauthenticated", http.MethodGet, "/secure", "", 401, "unauthenticated", ""},
	{"body too large", http.MethodPost, "/upload", "0123456789abcdefXYZ", 413, "request_body_too_large", ""},
}

func decodeRendererEnvelope(t *testing.T, response renderedResponse) map[string]any {
	t.Helper()
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(response.body, &envelope), "body: %s", response.body)
	return envelope
}

func enabledBiz() *biz.Plugin { return biz.NewWithConfig(biz.Config{FailureEnvelope: true}) }

// TestFailuresKeepTheProblemDetailWhenNoRendererIsSelected pins the zero-mount
// default (spec 2.3 item 1): with no renderer, and with biz mounted but its
// failure envelope off, every failure is the RFC 9457 body it was before the
// renderer contract existed, and the two compositions agree byte for byte.
func TestFailuresKeepTheProblemDetailWhenNoRendererIsSelected(t *testing.T) {
	bare := newRendererEngine(t, nil, rendererMiddleware(biz.Key, biz.New()))
	disabled := biz.New()
	declining := newRendererEngine(t, disabled, rendererMiddleware(biz.Key, disabled))

	for _, test := range rendererFailureCases {
		t.Run(test.name, func(t *testing.T) {
			response := serveRenderer(bare, test.method, test.path, test.body)

			assert.Equal(t, test.status, response.status)
			assert.Equal(t, web.ProblemContentType, response.header.Get("Content-Type"))
			assert.Equal(t, "no-store", response.header.Get("Cache-Control"))
			assert.Equal(t, "nosniff", response.header.Get("X-Content-Type-Options"))
			var problem web.ProblemDetail
			require.NoError(t, json.Unmarshal(response.body, &problem))
			assert.Equal(t, test.status, problem.Status)
			assert.Equal(t, test.code, problem.Properties["code"])
			assert.NotContains(t, problem.Properties, "success", "a Problem Detail must not carry the envelope's keys")

			again := serveRenderer(declining, test.method, test.path, test.body)
			assert.Equal(t, response.status, again.status)
			assert.Equal(t, response.header, again.header, "a declining renderer must not change a single header")
			assert.Equal(t, string(response.body), string(again.body), "a declining renderer must not change a single byte")
		})
	}
}

// TestEnabledFailureEnvelopeCoversEveryFailurePath is spec 2.3 item 2: with the
// envelope on, each path answers with the same status it always had, the
// framework's headers, and the success envelope's shape with success=false.
func TestEnabledFailureEnvelopeCoversEveryFailurePath(t *testing.T) {
	plugin := enabledBiz()
	engine := newRendererEngine(t, plugin, rendererMiddleware(biz.Key, plugin))

	for _, test := range rendererFailureCases {
		t.Run(test.name, func(t *testing.T) {
			response := serveRenderer(engine, test.method, test.path, test.body)

			assert.Equal(t, test.status, response.status, "the renderer decides the body only, never the status")
			assert.Equal(t, rendererJSONContentType, response.header.Get("Content-Type"))
			assert.Equal(t, "no-store", response.header.Get("Cache-Control"))
			assert.Equal(t, "nosniff", response.header.Get("X-Content-Type-Options"))

			envelope := decodeRendererEnvelope(t, response)
			assert.Equal(t, false, envelope["success"])
			assert.Equal(t, test.code, envelope["code"])
			message, _ := envelope["message"].(string)
			assert.NotEmpty(t, message)
			if test.message != "" {
				assert.Equal(t, test.message, message)
			}
			assert.Contains(t, envelope, "data", "data is always present, null when there is nothing to carry")
			for key := range envelope {
				assert.Contains(t, []string{"success", "code", "message", "data", "requestId"}, key,
					"only the envelope's own members may appear: %s", response.body)
			}
			assert.NotContains(t, string(response.body), "private-password")

			if test.name != "validation failure" {
				assert.Nil(t, envelope["data"])
				return
			}
			data, ok := envelope["data"].(map[string]any)
			require.True(t, ok, "validation data must be an object: %s", response.body)
			fieldErrors, ok := data["errors"].([]any)
			require.True(t, ok, "data.errors must be a list: %s", response.body)
			require.Len(t, fieldErrors, 2)
			for index, want := range []string{"email", "name"} {
				entry, _ := fieldErrors[index].(map[string]any)
				assert.Equal(t, want, entry["field"])
				assert.Equal(t, "required", entry["code"])
				assert.NotEmpty(t, entry["message"])
			}
		})
	}
}

// TestRendererSurvivesAnOnErrorScope is spec 2.3 item 3. A handler error raised
// inside an OnError scope is rendered by a request-scoped resolver derived from
// the Server's, so the renderer reaches that body only if the derivation carries
// it. /scoped uses a stub so the assertion is about the renderer itself, not
// about biz.
func TestRendererSurvivesAnOnErrorScope(t *testing.T) {
	stub := &stubRenderer{body: []byte(`{"rendered":"scoped"}`), ok: true}
	engine := newRendererEngine(t, stub)

	response := serveRenderer(engine, http.MethodGet, "/scoped", "")

	assert.Equal(t, http.StatusTeapot, response.status)
	assert.Equal(t, rendererJSONContentType, response.header.Get("Content-Type"))
	assert.JSONEq(t, `{"rendered":"scoped"}`, string(response.body))
	seen := stub.seen()
	require.Len(t, seen, 1)
	assert.Equal(t, "scoped_teapot", seen[0].Properties["code"])
	assert.Equal(t, http.StatusTeapot, seen[0].Status)
}

// TestFailureEnvelopeEchoesTheRequestIDOnlyWhenOneExists is spec 2.3 item 4.
func TestFailureEnvelopeEchoesTheRequestIDOnlyWhenOneExists(t *testing.T) {
	plugin := enabledBiz()
	with := newRendererEngine(t, plugin, rendererMiddleware(biz.Key, plugin),
		rendererMiddleware(requestid.Key, requestid.New()))
	without := newRendererEngine(t, plugin, rendererMiddleware(biz.Key, plugin))

	for _, test := range rendererFailureCases {
		t.Run(test.name, func(t *testing.T) {
			response := serveRenderer(with, test.method, test.path, test.body)
			envelope := decodeRendererEnvelope(t, response)
			header := response.header.Get("X-Request-ID")
			require.NotEmpty(t, header, "the requestid middleware must have run for this path")
			assert.Equal(t, header, envelope["requestId"], "the body must echo the id the response header carries")

			bare := decodeRendererEnvelope(t, serveRenderer(without, test.method, test.path, test.body))
			assert.NotContains(t, bare, "requestId", "no id means no field, not an empty one")
		})
	}
}

// TestRendererDeclineFallsBackToTheProblemDetail is spec 2.3 item 5: every way
// a renderer can decline, and the fail-safe for a code-less custom problem.
func TestRendererDeclineFallsBackToTheProblemDetail(t *testing.T) {
	t.Run("a custom mapper problem without a code is not swallowed by the envelope", func(t *testing.T) {
		plugin := enabledBiz()
		engine := newRendererEngine(t, plugin, rendererMiddleware(biz.Key, plugin))

		response := serveRenderer(engine, http.MethodGet, "/mapped", "")

		assert.Equal(t, http.StatusConflict, response.status)
		assert.Equal(t, web.ProblemContentType, response.header.Get("Content-Type"))
		var problem web.ProblemDetail
		require.NoError(t, json.Unmarshal(response.body, &problem))
		assert.Equal(t, "Conflicting state.", problem.Detail)
		assert.NotContains(t, problem.Properties, "code")
		assert.NotContains(t, problem.Properties, "success")
	})

	for _, test := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"declined with a body", []byte(`{"ignored":true}`), false},
		{"accepted with a nil body", nil, true},
		{"accepted with an empty body", []byte{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &stubRenderer{body: test.body, ok: test.ok}
			engine := newRendererEngine(t, stub)

			response := serveRenderer(engine, http.MethodGet, "/missing", "")

			assert.Equal(t, http.StatusNotFound, response.status)
			assert.Equal(t, web.ProblemContentType, response.header.Get("Content-Type"))
			assert.NotContains(t, string(response.body), "ignored")
			require.Len(t, stub.seen(), 1, "the renderer must have been asked before it declined")
		})
	}
}

// TestRendererReceivesTheNormalizedProblem pins what a renderer is handed.
func TestRendererReceivesTheNormalizedProblem(t *testing.T) {
	stub := &stubRenderer{body: []byte(`{"r":1}`), ok: true}
	engine := newRendererEngine(t, stub)

	response := serveRenderer(engine, http.MethodGet, "/missing?secret=query", "")

	assert.Equal(t, http.StatusNotFound, response.status)
	assert.JSONEq(t, `{"r":1}`, string(response.body))
	seen := stub.seen()
	require.Len(t, seen, 1)
	assert.Equal(t, "about:blank", seen[0].Type)
	assert.Equal(t, "Not Found", seen[0].Title)
	assert.Equal(t, http.StatusNotFound, seen[0].Status)
	assert.Equal(t, "/missing", seen[0].Instance)
	assert.Equal(t, "not_found", seen[0].Properties["code"])
	assert.Zero(t, stub.nilCtx, "the renderer must be handed the request's context")
}

// TestManagementPlaneKeepsTheProblemDetail is spec 2.3 item 6. The management
// plane has its own short chain with no resolver attached, so the renderer the
// serving plane carries must not reach it.
func TestManagementPlaneKeepsTheProblemDetail(t *testing.T) {
	stub := &stubRenderer{body: []byte(`{"rendered":"serving"}`), ok: true}
	cfg := rendererConfig()
	cfg.Management.Addr = "127.0.0.1:0"
	server, host := startRendererServer(t, cfg, stub)
	host.releaseTraffic()
	waitForServing(t, server.Addr(), "/ok")
	waitForServing(t, server.ManagementAddr(), "/anything")

	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + server.ManagementAddr() + "/missing")
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	assert.Equal(t, web.ProblemContentType, response.Header.Get("Content-Type"))
	assert.NotContains(t, string(body), "rendered")
	assert.Empty(t, stub.seen(), "the management plane must never consult the renderer")

	status, served, err := fetch(server.Addr(), "/missing", 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status)
	assert.JSONEq(t, `{"rendered":"serving"}`, served, "the serving plane, by contrast, does")
}

// TestSuccessResponsesAreUntouchedByTheRenderer is spec 2.3 item 7.
func TestSuccessResponsesAreUntouchedByTheRenderer(t *testing.T) {
	plugin := enabledBiz()
	envelope := newRendererEngine(t, plugin, rendererMiddleware(biz.Key, plugin))
	stub := &stubRenderer{body: []byte(`{"rendered":true}`), ok: true}
	intercepting := newRendererEngine(t, stub, rendererMiddleware(biz.Key, biz.New()))
	plain := newRendererEngine(t, nil, rendererMiddleware(biz.Key, biz.New()))

	want := `{"success":true,"code":"0","message":"","data":"fine"}`
	for name, response := range map[string]renderedResponse{
		"envelope on":    serveRenderer(envelope, http.MethodGet, "/ok", ""),
		"accepting stub": serveRenderer(intercepting, http.MethodGet, "/ok", ""),
		"no renderer":    serveRenderer(plain, http.MethodGet, "/ok", ""),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, http.StatusOK, response.status)
			assert.Equal(t, rendererJSONContentType, response.header.Get("Content-Type"))
			assert.Equal(t, want, string(response.body))
		})
	}

	empty := serveRenderer(intercepting, http.MethodPost, "/upload", "small")
	assert.Equal(t, http.StatusNoContent, empty.status)
	assert.Empty(t, empty.body)
	assert.Empty(t, stub.seen(), "a 2xx response must never reach the renderer")
}

// TestRendererIsNotGlobalState is spec 2.3 item 8. web.New binds a fixed
// address, so a Server built through NewServer without SetErrorRenderer stands
// in for it: what matters is that a renderer-bearing Server built first leaves
// nothing behind for a bare one, or for an engine no Server ever attached.
func TestRendererIsNotGlobalState(t *testing.T) {
	stub := &stubRenderer{body: []byte(`{"rendered":"first"}`), ok: true}
	first := newRendererEngine(t, stub)
	require.JSONEq(t, `{"rendered":"first"}`, string(serveRenderer(first, http.MethodGet, "/missing", "").body))
	require.Len(t, stub.seen(), 1)

	second := newRendererEngine(t, nil)
	response := serveRenderer(second, http.MethodGet, "/missing", "")
	assert.Equal(t, web.ProblemContentType, response.header.Get("Content-Type"))
	assert.NotContains(t, string(response.body), "rendered")

	// No resolver is attached, so this request resolves through the package-level
	// fallback: the one resolver every unattached request shares.
	bare := enginetest.New()
	bare.GET("/boom", func(context.Context, *web.Ctx) error { return errRendererSentinel })
	recorded := performRequest(bare, http.MethodGet, "/boom")
	assert.Equal(t, web.ProblemContentType, recorded.Header().Get("Content-Type"))
	assert.Len(t, stub.seen(), 1, "no other Server or engine may have reached the first one's renderer")
}

// TestRenderedFailuresKeepTheFrameworkHeaderHygiene pins that a rendered body
// changes the Content-Type and nothing else about the headers.
func TestRenderedFailuresKeepTheFrameworkHeaderHygiene(t *testing.T) {
	stub := &stubRenderer{body: []byte(`{"rendered":true}`), ok: true}
	for name, engine := range map[string]struct {
		contentType string
		response    renderedResponse
	}{
		"rendered":  {rendererJSONContentType, serveRenderer(newRendererEngine(t, stub), http.MethodGet, "/stale", "")},
		"defaulted": {web.ProblemContentType, serveRenderer(newRendererEngine(t, nil), http.MethodGet, "/stale", "")},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, http.StatusInternalServerError, engine.response.status)
			assert.Equal(t, engine.contentType, engine.response.header.Get("Content-Type"))
			assert.Equal(t, "no-store", engine.response.header.Get("Cache-Control"))
			assert.Equal(t, "nosniff", engine.response.header.Get("X-Content-Type-Options"))
			for _, stale := range []string{"Content-Encoding", "Content-Length", "ETag", "Last-Modified"} {
				assert.Empty(t, engine.response.header.Values(stale), "%s must not survive into a failure body", stale)
			}
			assert.NotContains(t, strings.ToLower(string(engine.response.body)), "private-password")
		})
	}
}

// TestRendererIsNotAskedAboutACommittedResponse pins the Written guard: a
// response already on the wire is neither rendered nor appended to.
func TestRendererIsNotAskedAboutACommittedResponse(t *testing.T) {
	stub := &stubRenderer{body: []byte(`{"rendered":true}`), ok: true}
	engine := newRendererEngine(t, stub)

	response := serveRenderer(engine, http.MethodGet, "/committed", "")

	assert.Equal(t, http.StatusOK, response.status)
	assert.Equal(t, "committed", string(response.body))
	assert.Empty(t, stub.seen())
}

// TestInFlightRefusalKeepsTheProblemDetailWhateverRendererIsSelected pins the
// documented exception: the admission gate is the first stage of the chain and
// answers before the error resolver is attached, so its 503 never reaches the
// renderer. The fixture mirrors startBlockedServer but installs the renderer
// before Start, which is when the Server reads it.
func TestInFlightRefusalKeepsTheProblemDetailWhateverRendererIsSelected(t *testing.T) {
	stub := &stubRenderer{body: []byte(`{"rendered":"overloaded"}`), ok: true}
	state := newBlockedHandler()
	server, ctx, _ := newPingServer(t, blockedConfig(t, 1), serverInputs{
		routes: []plugin.Entry[web.RouteContributor]{{
			Identity: plugin.Identity{Plugin: "inflight-renderer"},
			Value: fakeRouteContributor{register: func(router *web.Router) {
				router.GET("/block", state.handle)
			}},
		}},
	})
	server.SetErrorRenderer(stub)
	require.NoError(t, server.Start(ctx))

	pressure := runBlockedRequests(t, blockedServer{server: server, engine: testEngineOf(t, server), state: state}, 3)

	require.Equal(t, 2, pressure.refused, "the run must have produced refusals to inspect")
	for _, response := range pressure.responses {
		if response.Code != http.StatusServiceUnavailable {
			continue
		}
		assert.Equal(t, web.ProblemContentType, response.Header().Get("Content-Type"))
		assert.Contains(t, response.Body.String(), `"code":"server_overloaded"`)
		assert.NotContains(t, response.Body.String(), "rendered")
	}
	assert.Empty(t, stub.seen(), "the gate answers before the renderer could be consulted")
}

// TestRendererPanicIsContainedAndFallsBackToTheProblemDetail is spec 2.3 item
// 9. A renderer is application code reached from inside the panic boundary's
// own recovery, so one that panics must not cost the request its response:
// the panic is contained, the failure keeps its status, and the body is the
// default Problem Detail. The request completing at all is the containment.
func TestRendererPanicIsContainedAndFallsBackToTheProblemDetail(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		want int
		code string
	}{
		{"not found", "/missing", http.StatusNotFound, "not_found"},
		{"business error", "/business", http.StatusUnprocessableEntity, "ORDER.CLOSED"},
		{"panic boundary", "/panic", http.StatusInternalServerError, "internal_server_error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// biz is mounted with its envelope off, purely for the mapper that
			// turns the /business error into a 422; the panicking renderer is
			// the Server's, so the 422 body is the one it must not be able to
			// cost the request.
			renderer := &panicRenderer{}
			engine := newRendererEngine(t, renderer, rendererMiddleware(biz.Key, biz.New()))

			response := serveRenderer(engine, http.MethodGet, test.path, "")

			assert.Equal(t, test.want, response.status, "a contained renderer panic must preserve the failure's status")
			assert.Equal(t, web.ProblemContentType, response.header.Get("Content-Type"))
			assert.Equal(t, "no-store", response.header.Get("Cache-Control"))
			assert.Equal(t, "nosniff", response.header.Get("X-Content-Type-Options"))
			var problem web.ProblemDetail
			require.NoError(t, json.Unmarshal(response.body, &problem))
			assert.Equal(t, test.want, problem.Status)
			assert.Equal(t, test.code, problem.Properties["code"])
			assert.NotContains(t, problem.Properties, "success")
			assert.GreaterOrEqual(t, renderer.seen(), 1, "the framework must have consulted the renderer it then contained")
		})
	}
}
