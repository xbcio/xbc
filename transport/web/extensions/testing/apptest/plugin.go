package apptest

import (
	"context"
	"net/http"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is the stable plugin identity of the readiness probe.
const Key plugin.Key = "apptest"

// readyPath is the probe route New polls before returning an App. It lives
// outside any application path so a route under test cannot shadow it.
const readyPath = "/__apptest/ready"

// Plugin contributes the harness's readiness probe. It owns no state: the
// probe answers whether the server can serve, not whether the application
// under test is healthy -- that is what the test's own assertions are for.
type Plugin struct{}

var _ web.RouteContributor = (*Plugin)(nil)

var definition = plugin.Define(
	Key,
	func(plugin.BuildContext) (*Plugin, error) { return &Plugin{}, nil },
	plugin.Options[*Plugin]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns the probe's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the probe as a selectable Plugin Bundle. New composes it
// automatically; it is exported so a composition built elsewhere can include
// the same probe.
func Bundle() plugin.Bundle { return bundle }

// RegisterRoutes registers the probe as a public, unmetered route. Both
// declarations are required: without Auth(web.Public()) the deny default
// answers the probe with 401, and without Unmetered a saturated server would
// fail the very request that only asks whether it is alive.
func (*Plugin) RegisterRoutes(router *web.Router) {
	router.GET(readyPath, ready).Name("apptest.ready").Auth(web.Public()).Unmetered()
}

func ready(_ context.Context, c *web.Ctx) error {
	c.JSON(http.StatusOK, map[string]bool{"ready": true})
	return nil
}
