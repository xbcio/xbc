package gin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
)

// TestMountCoversThePrefixAndItsSubtree pins the dialect translation this
// adapter performs for web.Engine.Mount. The port asks for coverage of a whole
// subtree on a segment boundary, while gin registers one path per node, so the
// adapter has to register the bare prefix and a catch-all together; either
// registration alone leaves a request the port promises to cover answered by
// the matcher instead of the mounted chain.
//
// The matched chain must see the request exactly as it arrived: the port
// defines covering, not the stripping, which belongs to whatever wrapped the
// handler. The /mx case is what keeps "covers the subtree" from degrading into
// "matches a string prefix", and the POST case pins that a mount answers only
// the methods it was mounted for -- gin's own 405 with Allow, the same answer
// an unmounted path gets.
func TestMountCoversThePrefixAndItsSubtree(t *testing.T) {
	restoreProcessGlobals(t)

	built, err := Factory{}.NewEngine(web.Options{})
	require.NoError(t, err, "NewEngine() must not return an error")
	adapter, ok := built.(*engine)
	require.True(t, ok, "NewEngine must return this package's *engine")

	var seen []string
	handler := func(_ context.Context, c *web.Ctx) error {
		seen = append(seen, c.Request().URL.Path)
		c.Status(http.StatusNoContent)
		return nil
	}
	adapter.Mount(http.MethodGet, "/m", []web.Handler{handler})

	cases := []struct {
		name     string
		method   string
		target   string
		wantCode int
		wantRan  bool
	}{
		{name: "the prefix itself", method: http.MethodGet, target: "/m", wantCode: http.StatusNoContent, wantRan: true},
		{name: "the subtree root", method: http.MethodGet, target: "/m/", wantCode: http.StatusNoContent, wantRan: true},
		{name: "deeper in the subtree", method: http.MethodGet, target: "/m/x/y", wantCode: http.StatusNoContent, wantRan: true},
		{name: "a sibling sharing the prefix's characters", method: http.MethodGet, target: "/mx", wantCode: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(seen)
			recorder := httptest.NewRecorder()
			adapter.e.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.target, nil))

			assert.Equal(t, tc.wantCode, recorder.Code)
			if !tc.wantRan {
				assert.Len(t, seen, before, "a path outside the subtree must not reach the mounted chain")
				return
			}
			require.Len(t, seen, before+1, "the mounted chain must run exactly once")
			assert.Equal(t, tc.target, seen[before], "the chain must see the request path unchanged -- stripping belongs to the caller")
		})
	}

	t.Run("an unmounted method", func(t *testing.T) {
		before := len(seen)
		recorder := httptest.NewRecorder()
		adapter.e.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/m/x", nil))

		assert.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
		assert.Len(t, seen, before, "an unmounted method must not reach the mounted chain")
		assert.ElementsMatch(t, []string{http.MethodGet},
			strings.Split(recorder.Result().Header.Get("Allow"), ", "),
			"the 405 must list the methods the subtree is really mounted for")
	})
}
