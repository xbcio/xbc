// Package version is the production example's public Plugin. It owns one
// canonical Definition, reads its build metadata from plugins.version, and
// contributes exactly one route that needs no authentication -- which is what
// makes it this service's public half.
//
// It accepts no work of its own and holds nothing that needs releasing, so it
// implements neither Stop nor Drain: its one handler reads an immutable value
// and answers, and the Web plugin's own Stop is what finishes a request already
// in flight.
package version

import (
	"context"
	"net/http"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is version's stable configuration and runtime identity. Its section is
// plugins.version, and because the Definition declares no Activation policy it
// is active as soon as it is composed -- unless the merged configuration sets
// plugins.version.enabled: false.
const Key plugin.Key = "version"

// Config is bound from plugins.version. It carries build metadata only; the
// route below publishes these values deliberately, so nothing secret belongs
// here. A container image sets Version from its build argument, which is why
// the value is configuration rather than a compiled constant.
type Config struct {
	Name    string `yaml:"name"    default:"xbc-production-example"`
	Version string `yaml:"version" default:"dev"`
}

// Info is the payload of the public version endpoint.
type Info struct {
	Name    string `json:"name"    example:"xbc-production-example"`
	Version string `json:"version" example:"1.4.0"`
}

// Plugin serves the service's own build metadata.
type Plugin struct {
	config Config
}

var _ web.RouteContributor = (*Plugin)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: func() Config {
			return Config{Name: "xbc-production-example", Version: "dev"}
		},
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return &Plugin{config: cfg}, nil
	},
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns version's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns version's side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

// RegisterRoutes contributes the public metadata route.
//
// Auth(Public()) is a tier-2 declaration and the weakest reason a route can be
// reachable without credentials: a web.security policy rule (tier 1) still
// overrides it, which is where a deployment that wants every route
// authenticated would say so.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	router.GET("/version", p.get).
		Name("version.get").
		Auth(web.Public())
}

func (p *Plugin) get(_ context.Context, c *web.Ctx) error {
	c.JSON(http.StatusOK, Info{Name: p.config.Name, Version: p.config.Version})
	return nil
}
