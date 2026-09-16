// Package greeter is the Quickstart application's business Plugin. It owns a
// canonical Definition and contributes HTTP routes through an explicit Bundle.
package greeter

import (
	"context"
	"time"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/response/biz"
)

// Key is greeter's stable configuration and runtime identity.
const Key plugin.Key = "greeter"

// Plugin contributes the Quickstart's business routes.
type Plugin struct{}

var (
	_ web.RouteContributor = (*Plugin)(nil)
	_ health.Contributor   = (*Plugin)(nil)
)

var definition = plugin.Define(
	Key,
	func(plugin.BuildContext) (*Plugin, error) { return &Plugin{}, nil },
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
			plugin.ExportAs[health.Contributor](func(value *Plugin) health.Contributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns greeter's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns greeter's side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// GreetingRequest is the JSON body accepted by the greeting endpoint.
type GreetingRequest struct {
	Name string `json:"name" binding:"required,min=2,max=80" example:"XBC"`
}

// Greeting is the greeting endpoint's business payload.
type Greeting struct {
	Message string `json:"message" example:"hello from xbc"`
}

// HealthChecks implements health.Contributor. The aggregator prefixes every
// name with this plugin's Identity, so the probe reports "greeter/greeting".
//
// A plugin that owns an external dependency pings it here, with a Timeout
// tighter than the section default when the dependency is on a hot path. The
// greeter has no dependency, so the check only reports that the plugin was
// constructed and started: registering it is what makes the plugin visible in
// /readyz at all.
func (*Plugin) HealthChecks() []health.NamedChecker {
	return []health.NamedChecker{{
		Name:    "greeting",
		Kind:    health.Readiness,
		Timeout: 200 * time.Millisecond,
		Checker: health.CheckFunc(func(context.Context) error { return nil }),
	}}
}

// RegisterRoutes implements web.RouteContributor.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	router.GET("/hello", p.getGreeting).
		Name("greeter.hello").
		Auth(web.Public())

	router.POST("/hello", p.createGreeting).
		Name("greeter.hello.create").
		Auth(web.Public())
}

// getGreeting returns the default greeting.
//
// @Summary Get a greeting
// @Tags greetings
// @Produce json
// @Success 200 {object} biz.Response[Greeting]
// @Failure 500 {object} web.ProblemDetail
// @Router /hello [get]
func (*Plugin) getGreeting(_ context.Context, c *web.Ctx) error {
	return biz.OK(c, Greeting{Message: "hello from xbc"})
}

// createGreeting returns a greeting for the submitted name.
//
// @Summary Create a greeting
// @Tags greetings
// @Accept json
// @Produce json
// @Param request body GreetingRequest true "Greeting request"
// @Success 200 {object} biz.Response[Greeting]
// @Failure 400 {object} web.ProblemDetail
// @Failure 500 {object} web.ProblemDetail
// @Router /hello [post]
func (*Plugin) createGreeting(_ context.Context, c *web.Ctx) error {
	var request GreetingRequest
	if err := c.Bind(&request); err != nil {
		return err
	}
	return biz.OK(c, Greeting{Message: "hello " + request.Name + " from xbc"})
}
