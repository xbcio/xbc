package requestid

import (
	"context"
	"net/http"

	"github.com/xbcio/xbc/transport/web"
)

type contextKey struct{}

const ginContextKey = "xbc/transport/web/extensions/observability/requestid.id"

// FromContext returns the validated request ID propagated through the standard
// request context.
func FromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	value, ok := ctx.Value(contextKey{}).(string)
	return value, ok && value != ""
}

// FromRequest returns the request ID from r.Context().
func FromRequest(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	return FromContext(r.Context())
}

// From returns the request ID stored on both Ctx's request-scoped values and
// the standard request context. The latter fallback also works with copied
// requests.
func From(c *web.Ctx) (string, bool) {
	if c == nil {
		return "", false
	}
	if value, ok := c.Get(ginContextKey); ok {
		id, valid := value.(string)
		if valid && id != "" {
			return id, true
		}
	}
	return FromRequest(c.Request())
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}
