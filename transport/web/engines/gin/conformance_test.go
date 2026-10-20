package gin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// TestConformance runs the shared Engine contract suite against this adapter.
// It is what keeps the adapter and the neutral test engine from drifting:
// implied HEAD, method classification, Allow as a set, mount coverage, and the
// no-matcher-redirect rules are exercised over real connections.
func TestConformance(t *testing.T) {
	restoreProcessGlobals(t)
	enginetest.RunConformance(t, Factory{})
}

// TestImpliedHEADMaterializationIsIdempotent pins the two halves of the
// deferral: HEAD stays unregistered until Serve materializes it, and a second
// materialization is a no-op. The second half matters because gin cannot
// replace a registered handler, so a second registration of the same HEAD
// pattern panics -- the exact failure an idempotent materialization must avoid.
func TestImpliedHEADMaterializationIsIdempotent(t *testing.T) {
	restoreProcessGlobals(t)

	built, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err, "NewEngine() must not return an error")
	adapter, ok := built.(*engine)
	require.True(t, ok, "NewEngine must return this package's *engine")

	adapter.Handle(http.MethodGet, "/once", []web.Handler{func(_ context.Context, c *web.Ctx) error {
		c.SetHeader("X-Chain", "get")
		c.String(http.StatusOK, "get")
		return nil
	}})

	// Before materialization the HEAD half is still pending: gin has no HEAD
	// route yet, so the direct engine answers the request as a method mismatch.
	// This is the state Serve always leaves behind it, never a state a request
	// observes through the port.
	pending := httptest.NewRecorder()
	adapter.e.ServeHTTP(pending, httptest.NewRequest(http.MethodHead, "/once", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, pending.Code,
		"the implied HEAD must not be registered before Serve materializes it")

	adapter.materializeImpliedHEAD()
	adapter.materializeImpliedHEAD()

	served := httptest.NewRecorder()
	adapter.e.ServeHTTP(served, httptest.NewRequest(http.MethodHead, "/once", nil))
	assert.Equal(t, http.StatusOK, served.Code, "the materialized HEAD registration must serve the GET chain")
	assert.Equal(t, "get", served.Result().Header.Get("X-Chain"),
		"the HEAD registration must run the chain registered for GET")
}

// TestExplicitHEADOverridesImplied pins both registration orders the deferral
// exists for. Route.Any registers GET before HEAD, while an explicit Match or
// a hand-written composition may do the reverse; an eager implied-HEAD
// registration would panic on the second call in the first order, and a
// materialization that forgot to check ownership would panic in either. After
// materialization the explicit registration must own the pattern, and GET must
// keep serving its own chain.
func TestExplicitHEADOverridesImplied(t *testing.T) {
	restoreProcessGlobals(t)

	marker := func(name string) []web.Handler {
		return []web.Handler{func(_ context.Context, c *web.Ctx) error {
			c.SetHeader("X-Chain", name)
			c.String(http.StatusOK, "%s", name)
			return nil
		}}
	}

	for _, order := range []struct {
		name      string
		headFirst bool
	}{
		{name: "GET then explicit HEAD"},
		{name: "explicit HEAD then GET", headFirst: true},
	} {
		t.Run(order.name, func(t *testing.T) {
			built, err := Factory{}.NewEngine(web.Options{})
			require.NoError(t, err, "NewEngine() must not return an error")
			adapter, ok := built.(*engine)
			require.True(t, ok, "NewEngine must return this package's *engine")

			require.NotPanics(t, func() {
				if order.headFirst {
					adapter.Handle(http.MethodHead, "/report", marker("head"))
					adapter.Handle(http.MethodGet, "/report", marker("get"))
					return
				}
				adapter.Handle(http.MethodGet, "/report", marker("get"))
				adapter.Handle(http.MethodHead, "/report", marker("head"))
			}, "registering both GET and HEAD for one pattern must not panic in either order")

			require.NotPanics(t, adapter.materializeImpliedHEAD,
				"materialization must skip a pattern an explicit HEAD registration already owns")

			head := httptest.NewRecorder()
			adapter.e.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/report", nil))
			assert.Equal(t, http.StatusOK, head.Code)
			assert.Equal(t, "head", head.Result().Header.Get("X-Chain"),
				"the explicit HEAD registration must replace the implied one")

			get := httptest.NewRecorder()
			adapter.e.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/report", nil))
			assert.Equal(t, http.StatusOK, get.Code)
			assert.Equal(t, "get", get.Result().Header.Get("X-Chain"),
				"the GET registration must keep serving GET")
		})
	}
}
