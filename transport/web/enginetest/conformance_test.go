package enginetest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/xbcio/xbc/transport/web"
)

// TestConformance runs the shared Engine contract suite against this engine.
// The suite is what keeps this package's documented conformance claim honest:
// the same subtests a production adapter runs are run here.
func TestConformance(t *testing.T) {
	RunConformance(t, Factory{})
}

// TestUncleanPathReachesNoRouteInsteadOfRedirect pins the request rule this
// engine has to enforce itself. ServeMux cleans the request path and answers
// the difference with a redirect of its own, straight from the matcher and
// ahead of every chain -- exactly the adapter-answers-the-request bypass
// web.Engine forbids. Engine.ServeHTTP recognizes the unclean path first and
// hands it to the unmatched chains, so the response is the framework's own
// 404 rather than a 3xx.
func TestUncleanPathReachesNoRouteInsteadOfRedirect(t *testing.T) {
	engine := New()
	engine.GET("/b", func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusOK, "route")
		return nil
	})
	engine.NoRoute([]web.Handler{func(_ context.Context, c *web.Ctx) error {
		c.String(http.StatusNotFound, "no-route")
		return nil
	}})

	for _, target := range []string{"/a/../b", "//b"} {
		t.Run(target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))

			// Result() is the response as committed, not the recorder's live
			// header map, so a header set after the status line cannot be
			// mistaken for one the client received.
			result := recorder.Result()
			defer func() { _ = result.Body.Close() }()

			assert.Equal(t, http.StatusNotFound, result.StatusCode,
				"an unclean path must reach NoRoute instead of being redirected to its cleaned form")
			assert.Equal(t, "no-route", recorder.Body.String(), "the NoRoute chain must answer the request")
			assert.Empty(t, result.Header.Get("Location"), "a matcher redirect must not answer an unclean path")
			assert.Empty(t, result.Header.Get("Allow"),
				"an unclean path is not a method mismatch, so the clean form's methods must not be reported")
		})
	}
}
