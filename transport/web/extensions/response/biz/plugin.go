package biz

import (
	"encoding/json"
	"errors"
	"net/http"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
)

// Key is the stable configuration and middleware identity of the optional
// business response contract plugin.
const Key plugin.Key = "biz"

// Plugin contributes the optional success contract, a biz.onerror middleware
// that maps Error values into safe RFC 9457 Problem Details, and a
// web.ErrorRenderer that turns failures into the response envelope when
// Config.FailureEnvelope is on.
type Plugin struct {
	logger          corelog.Logger
	failureEnvelope bool
}

var (
	_ web.Middleware    = (*Plugin)(nil)
	_ web.ErrorRenderer = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{Defaults: DefaultConfig},
	func(context plugin.BuildContext, cfg Config) (*Plugin, error) {
		return &Plugin{logger: context.Log(), failureEnvelope: cfg.FailureEnvelope}, nil
	},
	plugin.Options[*Plugin]{
		// The renderer is exported whether or not the envelope is on: a disabled
		// Plugin declines every failure, so the Server's default is untouched.
		Exports: plugin.Contracts(
			plugin.ExportAs[web.Middleware](func(value *Plugin) web.Middleware { return value }),
			plugin.ExportAs[web.ErrorRenderer](func(value *Plugin) web.ErrorRenderer { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable business response plugin with default
// configuration. It logs through a no-op logger; the assembled plugin receives
// the application logger instead.
func New() *Plugin { return &Plugin{} }

// NewWithConfig constructs a directly usable business response plugin with an
// explicit configuration, the way an assembled plugin receives plugins.biz.
func NewWithConfig(cfg Config) *Plugin { return &Plugin{failureEnvelope: cfg.FailureEnvelope} }

func (p *Plugin) log() corelog.Logger {
	if p == nil || p.logger == nil {
		return corelog.Nop()
	}
	return p.logger
}

// Handler contributes the business error converter as a focused OnError
// boundary. Web's outer boundary remains the final safe fallback for unknown
// errors and invalid mapper output.
func (p *Plugin) Handler() web.Handler {
	return web.OnError(web.ErrorMapperFunc(p.mapError))
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
		if requestID, ok := requestid.From(c); ok {
			problem.Properties["requestId"] = requestID
		}
	}
	return problem, true
}

// RenderError implements web.ErrorRenderer. With Config.FailureEnvelope off it
// declines every failure, so the Server keeps its RFC 9457 default.
//
// With it on, a failure that carries a valid business code is written as the
// success envelope's shape with success=false: message is the problem's detail
// (its title when there is none), data is {"errors": [...]} for a validation
// failure and null otherwise, and requestId comes from the same source as the
// success path. Nothing else in the problem's extension members is copied. A
// failure without a valid code is declined rather than published under an
// invented one, so a custom mapper's code-less problem still reaches the client
// as a Problem Detail. The HTTP status is the framework's and is not touched.
func (p *Plugin) RenderError(c *web.Ctx, problem web.ProblemDetail) ([]byte, bool) {
	if p == nil || !p.failureEnvelope {
		return nil, false
	}
	code, ok := problem.Properties["code"].(string)
	if !ok || !validCode(code) {
		return nil, false
	}

	message := problem.Detail
	if message == "" {
		message = problem.Title
	}
	var data any
	if fieldErrors, present := problem.Properties["errors"]; present {
		data = map[string]any{"errors": fieldErrors}
	}
	response := Response[any]{Code: code, Message: message, Data: data}
	if requestID, found := requestid.From(c); found {
		response.RequestID = requestID
	}
	payload, err := json.Marshal(response)
	if err != nil {
		return nil, false
	}
	return payload, true
}

// Definition returns biz's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns biz's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }
