package csrf_test

import (
	"cmp"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	csrf "github.com/xbcio/xbc/transport/web/extensions/security/csrf"
	"github.com/xbcio/xbc/transport/web/extensions/security/securityheaders"
)

// This file composes the real CSRF and security-headers middleware through the
// engine-neutral request path and orders them with their real web.Order
// values. The full plugin assembly path is not reachable from a package of the
// transport/web module: this module's architecture guard forbids every one of
// its packages, test imports included, from importing plugin/assembly. The
// resolver below therefore applies the two same-phase rules the production
// resolver applies for these two entries -- declared Before/After edges first,
// canonical identity otherwise -- rather than hand-picking an order.

type orderedMiddleware struct {
	key   plugin.Key
	value web.Middleware
}

// runsBefore reports whether from's declared order constraints place it before
// to.
func runsBefore(from, to orderedMiddleware) bool {
	order := from.value.Order()
	for _, ref := range order.Before {
		if ref.Key() == to.key {
			return true
		}
	}
	for _, ref := range order.After {
		if ref.Key() == to.key {
			return false
		}
	}
	toOrder := to.value.Order()
	for _, ref := range toOrder.Before {
		if ref.Key() == from.key {
			return false
		}
	}
	for _, ref := range toOrder.After {
		if ref.Key() == from.key {
			return true
		}
	}
	return false
}

// compareOrder mirrors the production resolver's rules for two entries in one
// phase: a declared edge decides, otherwise canonical identity does.
func compareOrder(left, right orderedMiddleware) int {
	if runsBefore(left, right) {
		return -1
	}
	if runsBefore(right, left) {
		return 1
	}
	if phase := cmp.Compare(int(left.value.Order().Phase), int(right.value.Order().Phase)); phase != 0 {
		return phase
	}
	return strings.Compare(left.key.String(), right.key.String())
}

// TestRefusedUnsafeRequestCarriesSecurityHeaders is the F3 guard: when both
// plugins are composed, a refused unsafe request must already carry the
// security headers, so security-headers has to resolve before CSRF. Without
// the ordering preference neither entry declares an edge and canonical
// identity puts "csrf" first, which is exactly the regression this test
// catches.
func TestRefusedUnsafeRequestCarriesSecurityHeaders(t *testing.T) {
	csrfPlugin, err := csrf.New(csrf.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	entries := []orderedMiddleware{
		{key: csrf.Key, value: csrfPlugin},
		{key: securityheaders.Key, value: securityheaders.New()},
	}
	ordered := slices.Clone(entries)
	slices.SortFunc(ordered, compareOrder)
	if ordered[0].key != securityheaders.Key || ordered[1].key != csrf.Key {
		t.Fatalf("resolved order = [%s %s], want [%s %s]; declared orders: securityheaders=%#v csrf=%#v",
			ordered[0].key, ordered[1].key, securityheaders.Key, csrf.Key,
			entries[1].value.Order(), entries[0].value.Order())
	}

	chain := make([]web.Handler, 0, len(ordered)+1)
	for _, entry := range ordered {
		chain = append(chain, entry.value.Handler())
	}
	downstream := 0
	chain = append(chain, func(_ context.Context, c *web.Ctx) error {
		downstream++
		c.Status(http.StatusOK)
		return nil
	})
	engine := enginetest.New()
	engine.Handle(http.MethodPost, "/resource", chain)

	// Neither Origin nor Referer: CSRF refuses this state-changing request.
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://example.com/resource", nil))
	if response.Code != http.StatusForbidden || downstream != 0 {
		t.Fatalf("status/downstream = %d/%d, want 403/0", response.Code, downstream)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := response.Header().Get(header); got != want {
			t.Fatalf("%s on the CSRF refusal = %q, want %q; CSRF must run after security-headers", header, got, want)
		}
	}
	if got := response.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("CSRF refusal carries no Content-Security-Policy; CSRF must run after security-headers")
	}
}
