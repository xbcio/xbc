package gin

import (
	"errors"
	"net/http"

	ginlib "github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

	"github.com/xbcio/xbc/transport/web"
)

// requestContext implements web.RequestContext over a live *gin.Context.
//
// Every method forwards. Nothing here derives an answer gin already has: the
// route parameter table, the trusted-proxy parsing behind ClientIP, the
// two-phase status protocol, the binding pipeline, and the handler index that
// Next and Abort move are all engine state, and a second implementation of any
// of them would fork from the one the engine itself uses.
//
// The xbc-side policy that used to sit next to these calls does not move in
// here either. Bind returns gin's own binding error rather than a ParamError,
// because turning a binding failure into a Problem Detail is a decision
// transport/web makes once for every engine.
type requestContext struct {
	c *ginlib.Context
}

// newRequestContext wraps a live *gin.Context. It stays unexported: the only
// callers are this adapter's own registration path and its tests, and an
// exported constructor would put *gin.Context back into a public signature
// that the Engine port exists to remove.
func newRequestContext(c *ginlib.Context) *requestContext {
	return &requestContext{c: c}
}

func (rc *requestContext) Request() *http.Request { return rc.c.Request }

func (rc *requestContext) SetRequest(r *http.Request) { rc.c.Request = r }

// Writer returns gin's response writer through the neutral contract, which it
// already satisfies: gin.ResponseWriter is a strict superset of
// web.ResponseWriter.
func (rc *requestContext) Writer() web.ResponseWriter { return rc.c.Writer }

// SetWriter installs a neutral writer as gin's. The shim is what makes the
// two faces meet: gin writes through its own two-phase ResponseWriter, and w
// speaks net/http semantics where WriteHeader commits.
//
// A writer that is already a gin.ResponseWriter is installed as-is. That is
// how a buffering middleware restores the writer it replaced: wrapping the
// original in a fresh shim would install a second status holder whose own
// default masks the status gin's writer already recorded.
func (rc *requestContext) SetWriter(w web.ResponseWriter) {
	if writer, ok := w.(ginlib.ResponseWriter); ok {
		rc.c.Writer = writer
		return
	}
	rc.c.Writer = newShim(w)
}

func (rc *requestContext) Param(name string) string { return rc.c.Param(name) }

func (rc *requestContext) ClientIP() string { return rc.c.ClientIP() }

func (rc *requestContext) Status(code int) { rc.c.Status(code) }

func (rc *requestContext) Bind(obj any) error { return normalizeBindError(rc.c.ShouldBind(obj)) }

func (rc *requestContext) BindURI(obj any) error { return normalizeBindError(rc.c.ShouldBindUri(obj)) }

// normalizeBindError restates gin's aggregate validation failure in the
// standard library's multi-error shape, so web.ParamError can walk it without
// naming a gin type. gin reports a per-element failure as
// binding.SliceValidationError, which is a []error with no Unwrap; errors.Join
// gives those same elements a shape errors.As and errors.Is already
// understand.
//
// The recursion is not decorative. A slice of slices nests
// SliceValidationError inside itself, and converting only the outer level
// would leave the inner one opaque again.
//
// Every element gin ever appends to a SliceValidationError is the non-nil
// result of a failed per-element validation (see gin's
// defaultValidator.ValidateStruct, binding/default_validator.go): it only
// appends when v.ValidateStruct(...) returns a non-nil error, so a nil
// element is not reachable here. errors.Join skipping nils is therefore never
// exercised by production input; it is not a silent-success hazard for this
// call site.
func normalizeBindError(err error) error {
	var aggregate binding.SliceValidationError
	if !errors.As(err, &aggregate) {
		return err
	}
	normalized := make([]error, 0, len(aggregate))
	for _, item := range aggregate {
		normalized = append(normalized, normalizeBindError(item))
	}
	return errors.Join(normalized...)
}

// JSON renders obj and reports a render failure to its caller.
//
// gin's own Context.JSON returns nothing: a failed render is pushed onto
// Context.Errors and aborts the chain. Forwarding to it keeps both of those
// effects, which the error boundary still relies on, and the newly appended
// entry is what this method reports back. Rendering through render.JSON
// directly would return the error without reimplementing anything, but it
// would also drop gin's bodyAllowedForStatus handling and write a body for a
// 204 or a 304.
func (rc *requestContext) JSON(code int, obj any) error {
	before := len(rc.c.Errors)
	rc.c.JSON(code, obj)
	if len(rc.c.Errors) > before {
		return rc.c.Errors[len(rc.c.Errors)-1].Err
	}
	return nil
}

func (rc *requestContext) Next() { rc.c.Next() }

func (rc *requestContext) Abort() { rc.c.Abort() }

var _ web.RequestContext = (*requestContext)(nil)
