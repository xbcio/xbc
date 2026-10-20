package enginetest

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xbcio/xbc/transport/web"
)

// conformanceClient issues one request per conformance check and never follows
// a redirect. A 3xx must be observed as the response itself: the Engine port
// forbids an adapter from answering with one, so following it would hide the
// very behavior the suite exists to catch.
var conformanceClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       10 * time.Second,
}

// RunConformance drives factory through the Engine contract both the
// production adapter and this test engine must satisfy.
//
// Every subtest builds its own engine through factory, registers through the
// port only (Handle, Mount, NoRoute, NoMethod), serves it on a loopback
// listener, issues real requests, and shuts the engine down. Real connections
// matter: the HTTP layer, not the chain, suppresses a HEAD response body, and a
// matcher-produced redirect is only visible as a response when the client is
// not allowed to follow it.
//
// Route paths are literals. Pattern syntax is the one permanent difference
// between adapters (ServeMux patterns versus gin wildcards), so it is out of
// the suite's scope by design.
func RunConformance(t *testing.T, factory web.EngineFactory) {
	t.Helper()

	t.Run("exact route", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Handle(http.MethodGet, "/hello", markerChain("get", http.StatusOK, "hello"))
		base := conformanceServe(t, engine)

		response, body := conformanceDo(t, http.MethodGet, base+"/hello")
		requireResponse(t, response, body, http.StatusOK, "get", "hello")
	})

	t.Run("implied HEAD", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Handle(http.MethodGet, "/hello", markerChain("get", http.StatusOK, "hello"))
		base := conformanceServe(t, engine)

		// The status and the header come from the GET chain; the body is
		// suppressed by the HTTP layer, never by the chain.
		response, body := conformanceDo(t, http.MethodHead, base+"/hello")
		requireResponse(t, response, body, http.StatusOK, "get", "")
	})

	t.Run("explicit HEAD overrides", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Handle(http.MethodGet, "/hello", markerChain("get", http.StatusOK, "hello"))
		engine.Handle(http.MethodHead, "/hello", markerChain("head", http.StatusOK, "hello"))
		base := conformanceServe(t, engine)

		response, body := conformanceDo(t, http.MethodHead, base+"/hello")
		requireResponse(t, response, body, http.StatusOK, "head", "")

		// An explicit HEAD registration replaces only the implied half; the GET
		// registration must keep serving GET.
		response, body = conformanceDo(t, http.MethodGet, base+"/hello")
		requireResponse(t, response, body, http.StatusOK, "get", "hello")
	})

	t.Run("method mismatch", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		only := markerChain("only", http.StatusOK, "only")
		engine.Handle(http.MethodGet, "/only", only)
		engine.Handle(http.MethodDelete, "/only", only)
		engine.NoMethod(markerChain("no-method", http.StatusMethodNotAllowed, "no-method"))
		base := conformanceServe(t, engine)

		response, body := conformanceDo(t, http.MethodPost, base+"/only")
		requireResponse(t, response, body, http.StatusMethodNotAllowed, "no-method", "no-method")
		requireAllowSet(t, response, http.MethodGet, http.MethodHead, http.MethodDelete)
	})

	t.Run("unmatched path", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Handle(http.MethodGet, "/hello", markerChain("get", http.StatusOK, "hello"))
		engine.NoRoute(markerChain("no-route", http.StatusNotFound, "no-route"))
		base := conformanceServe(t, engine)

		response, body := conformanceDo(t, http.MethodGet, base+"/absent")
		requireResponse(t, response, body, http.StatusNotFound, "no-route", "no-route")
		requireNoAllow(t, response)
	})

	t.Run("trailing slash is not a redirect", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Handle(http.MethodGet, "/exact", markerChain("exact", http.StatusOK, "exact"))
		engine.NoRoute(markerChain("no-route", http.StatusNotFound, "no-route"))
		base := conformanceServe(t, engine)

		response, body := conformanceDo(t, http.MethodGet, base+"/exact/")
		requireNotRedirect(t, response)
		requireResponse(t, response, body, http.StatusNotFound, "no-route", "no-route")
	})

	t.Run("unclean path is not a redirect", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Handle(http.MethodGet, "/b", markerChain("b", http.StatusOK, "b"))
		engine.NoRoute(markerChain("no-route", http.StatusNotFound, "no-route"))
		base := conformanceServe(t, engine)

		for _, unclean := range []struct {
			name   string
			target string
		}{
			{name: "dot-dot segment", target: "/a/../b"},
			{name: "doubled slash", target: "//b"},
		} {
			t.Run(unclean.name, func(t *testing.T) {
				response, body := conformanceDo(t, http.MethodGet, base+unclean.target)
				requireNotRedirect(t, response)
				requireResponse(t, response, body, http.StatusNotFound, "no-route", "no-route")
				requireNoAllow(t, response)
			})
		}
	})

	t.Run("mount coverage", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Mount(http.MethodGet, "/flow", markerChain("flow", http.StatusOK, "flow"))
		base := conformanceServe(t, engine)

		// HEAD covers both mount patterns: the bare prefix is its own
		// registration in every adapter (gin's exact route and its catch-all
		// are separate patterns), so a suite that only exercised the subtree
		// would miss a missing implied HEAD on /flow itself.
		requests := []struct {
			method string
			target string
			body   string
		}{
			{method: http.MethodGet, target: "/flow", body: "flow"},
			{method: http.MethodGet, target: "/flow/", body: "flow"},
			{method: http.MethodGet, target: "/flow/child", body: "flow"},
			{method: http.MethodHead, target: "/flow", body: ""},
			{method: http.MethodHead, target: "/flow/child", body: ""},
		}
		for _, request := range requests {
			t.Run(request.method+" "+request.target, func(t *testing.T) {
				response, body := conformanceDo(t, request.method, base+request.target)
				requireResponse(t, response, body, http.StatusOK, "flow", request.body)
			})
		}
	})

	t.Run("mount segment boundary", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Mount(http.MethodGet, "/flow", markerChain("flow", http.StatusOK, "flow"))
		engine.NoRoute(markerChain("no-route", http.StatusNotFound, "no-route"))
		base := conformanceServe(t, engine)

		response, body := conformanceDo(t, http.MethodGet, base+"/flowx")
		requireResponse(t, response, body, http.StatusNotFound, "no-route", "no-route")
	})

	t.Run("mounted method mismatch", func(t *testing.T) {
		engine := conformanceEngine(t, factory)
		engine.Mount(http.MethodGet, "/flow", markerChain("flow", http.StatusOK, "flow"))
		engine.NoMethod(markerChain("no-method", http.StatusMethodNotAllowed, "no-method"))
		base := conformanceServe(t, engine)

		response, body := conformanceDo(t, http.MethodPost, base+"/flow/child")
		requireResponse(t, response, body, http.StatusMethodNotAllowed, "no-method", "no-method")
		requireAllowIncludes(t, response, http.MethodGet, http.MethodHead)
	})
}

// conformanceEngine builds one engine through factory with neutral options.
func conformanceEngine(t *testing.T, factory web.EngineFactory) web.Engine {
	t.Helper()
	engine, err := factory.NewEngine(web.Options{})
	if err != nil {
		t.Fatalf("NewEngine() failed: %v", err)
	}
	return engine
}

// conformanceServe serves engine on a loopback listener and returns its base
// URL. The cleanup shuts the engine down with a bounded context and waits for
// Serve to return: a Shutdown that reported success while Serve kept running
// would leave the other half of the port's shutdown contract unmet.
func conformanceServe(t *testing.T, engine web.Engine) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to bind a loopback listener: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- engine.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := engine.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() failed: %v", err)
		}
		select {
		case err := <-served:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve() returned %v after Shutdown, want nil or http.ErrServerClosed", err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("Serve() did not return after Shutdown")
		}
	})
	return "http://" + listener.Addr().String()
}

// conformanceDo issues one request and returns the response together with its
// body. The body is read here so a HEAD response can be asserted to be empty
// without a second call.
func conformanceDo(t *testing.T, method, url string) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("%s %s: failed to build the request: %v", method, url, err)
	}
	response, err := conformanceClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: request failed: %v", method, url, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("%s %s: failed to read the response body: %v", method, url, err)
	}
	return response, string(body)
}

// markerChain builds a terminal handler that marks the response with marker
// and writes body. The marker is a header because a HEAD response carries no
// body, so the body alone cannot tell which registration ran.
func markerChain(marker string, status int, body string) []web.Handler {
	return []web.Handler{func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Chain", marker)
		c.String(status, "%s", body)
		return nil
	}}
}

// requireResponse asserts the status, the chain marker, and the body of one
// conformance response.
func requireResponse(t *testing.T, response *http.Response, body string, status int, marker, wantBody string) {
	t.Helper()
	if response.StatusCode != status {
		t.Fatalf("%s: status = %d, want %d", requestLabel(response), response.StatusCode, status)
	}
	if got := response.Header.Get("X-Chain"); got != marker {
		t.Fatalf("%s: X-Chain = %q, want %q -- the %q chain must have run", requestLabel(response), got, marker, marker)
	}
	if body != wantBody {
		t.Fatalf("%s: body = %q, want %q", requestLabel(response), body, wantBody)
	}
}

// requireNotRedirect asserts the response was produced by a chain rather than
// by the engine's matcher.
func requireNotRedirect(t *testing.T, response *http.Response) {
	t.Helper()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		t.Fatalf("%s: status = %d, want a chain-rendered response rather than a matcher redirect",
			requestLabel(response), response.StatusCode)
	}
	if location := response.Header.Get("Location"); location != "" {
		t.Fatalf("%s: Location = %q, want no redirect target", requestLabel(response), location)
	}
}

// requireAllowSet asserts that Allow is exactly want, as the set the port
// defines it to be: order is not significant.
func requireAllowSet(t *testing.T, response *http.Response, want ...string) {
	t.Helper()
	got := allowSetOf(response)
	if len(got) != len(want) {
		t.Fatalf("%s: Allow = %q, want the set %v", requestLabel(response), response.Header.Get("Allow"), want)
	}
	for _, method := range want {
		if !got[method] {
			t.Fatalf("%s: Allow = %q, want the set %v", requestLabel(response), response.Header.Get("Allow"), want)
		}
	}
}

// requireAllowIncludes asserts that Allow names at least want. It is for
// responses whose full method set is the engine's to decide.
func requireAllowIncludes(t *testing.T, response *http.Response, want ...string) {
	t.Helper()
	got := allowSetOf(response)
	for _, method := range want {
		if !got[method] {
			t.Fatalf("%s: Allow = %q, want it to include %q", requestLabel(response), response.Header.Get("Allow"), method)
		}
	}
}

// requireNoAllow asserts that an unmatched path carries no Allow header: a 404
// is not a method mismatch.
func requireNoAllow(t *testing.T, response *http.Response) {
	t.Helper()
	if allow := response.Header.Get("Allow"); allow != "" {
		t.Fatalf("%s: Allow = %q, want no Allow header on an unmatched path", requestLabel(response), allow)
	}
}

// allowSetOf reads Allow as a set: methods are comma-separated, surrounding
// whitespace is not significant, and an absent header is the empty set.
func allowSetOf(response *http.Response) map[string]bool {
	set := make(map[string]bool)
	for entry := range strings.SplitSeq(response.Header.Get("Allow"), ",") {
		if method := strings.TrimSpace(entry); method != "" {
			set[method] = true
		}
	}
	return set
}

// requestLabel renders the request a conformance assertion is about.
func requestLabel(response *http.Response) string {
	return response.Request.Method + " " + response.Request.URL.String()
}
