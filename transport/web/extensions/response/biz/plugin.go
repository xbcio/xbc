package biz

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
)

// Key is the stable configuration and middleware identity of the optional
// business response contract plugin.
const Key plugin.Key = "biz"

// Plugin contributes the optional success contract and a biz.onerror
// middleware that maps Error values into safe RFC 9457 Problem Details.
type Plugin struct{}

var _ web.Middleware = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{Defaults: DefaultConfig},
	func(plugin.BuildContext, Config) (*Plugin, error) {
		return New(), nil
	},
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable business response plugin.
func New() *Plugin { return &Plugin{} }

// Handler contributes the business error converter as a focused OnError
// boundary. Web's outer boundary remains the final safe fallback for unknown
// errors and invalid mapper output.
func (p *Plugin) Handler() gin.HandlerFunc {
	return web.OnError(web.ErrorMapperFunc(p.mapError))
}

// Order places the business error mapper in Web's error phase.
func (*Plugin) Order() web.Order { return web.Order{Phase: web.PhaseError} }

// mapError recognizes wrapped Error values. Invalid public contracts fail
// closed as a fixed 500 response.
func (p *Plugin) mapError(c *gin.Context, err error) (web.ProblemDetail, bool) {
	var businessError *Error
	if !errors.As(err, &businessError) || businessError == nil {
		return web.ProblemDetail{}, false
	}
	if businessError.Status() < http.StatusBadRequest || businessError.Status() > 599 || !validCode(businessError.Code()) {
		return web.NewProblem(http.StatusInternalServerError, "internal_server_error"), true
	}

	problem := web.NewProblem(businessError.Status(), businessError.Code())
	problem.Detail = businessError.Detail()
	if requestID, ok := requestid.FromGin(c); ok {
		problem.Properties["requestId"] = requestID
	}
	return problem, true
}

// Definition returns biz's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns biz's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
