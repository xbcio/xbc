package biz

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
)

// Key is the stable configuration and middleware identity of the optional
// business response contract plugin.
const Key plugin.Key = "biz"

// Plugin contributes the optional success contract and a biz.onerror
// middleware that maps Error values into safe RFC 9457 Problem Details.
type Plugin struct {
	logger corelog.Logger
}

var _ web.Middleware = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{Defaults: DefaultConfig},
	func(context plugin.BuildContext, _ Config) (*Plugin, error) {
		return &Plugin{logger: context.Log()}, nil
	},
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable business response plugin. It logs through a
// no-op logger; the assembled plugin receives the application logger instead.
func New() *Plugin { return &Plugin{} }

func (p *Plugin) log() corelog.Logger {
	if p == nil || p.logger == nil {
		return corelog.Nop()
	}
	return p.logger
}

// Handler contributes the business error converter as a focused OnError
// boundary. Web's outer boundary remains the final safe fallback for unknown
// errors and invalid mapper output.
func (p *Plugin) Handler() gin.HandlerFunc {
	return web.Handle(web.OnError(web.ErrorMapperFunc(p.mapError)))
}

// Order places the business error mapper in Web's error phase.
func (*Plugin) Order() web.Order { return web.Order{Phase: web.PhaseError} }

// mapError recognizes wrapped Error values. Invalid public contracts fail
// closed as a fixed 500 response.
func (p *Plugin) mapError(c *web.Ctx, err error) (web.ProblemDetail, bool) {
	var businessError *Error
	if !errors.As(err, &businessError) || businessError == nil {
		return web.ProblemDetail{}, false
	}
	if !publishableStatus(businessError.Status()) || !validCode(businessError.Code()) {
		return web.NewProblem(http.StatusInternalServerError, "internal_server_error"), true
	}

	// Web's resolver logs only 5xx, so a 4xx business failure would discard its
	// cause at both ends: it is deliberately kept out of the response, and
	// nothing else would record it. Attaching a cause is the caller's explicit
	// signal that the failure is worth diagnosing, so honor it here.
	if cause := businessError.Unwrap(); cause != nil {
		p.log().Error("business failure",
			"code", businessError.Code(),
			"status", businessError.Status(),
			"error", cause,
		)
	}

	problem := web.NewProblem(businessError.Status(), businessError.Code())
	problem.Detail = businessError.Detail()
	if c != nil {
		if requestID, ok := requestid.FromGin(c.Gin()); ok {
			problem.Properties["requestId"] = requestID
		}
	}
	return problem, true
}

// Definition returns biz's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns biz's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
