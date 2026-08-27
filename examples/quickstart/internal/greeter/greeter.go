// Package greeter is the business plugin of the quickstart example: it
// contributes one HTTP route and nothing else.
//
// It exists to show the shape of an ordinary application plugin, and in
// particular three things that are easy to get wrong when coming from a
// framework where wiring lives in main:
//
//   - The plugin is declared here, in the package that defines it, from
//     init(). main never names it beyond a blank import.
//   - Contributing a route means implementing web.RouteProvider. There is no
//     registration call, no router handed down through constructors, and no
//     ordering the author has to arrange -- web collects every provider
//     itself once all plugins have initialized.
//   - The dependency on web is one-directional and explicit. greeter imports
//     web because it speaks HTTP; nothing in web or in the core knows greeter
//     exists.
package greeter

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/catalog"
	"github.com/xbcio/xbc/web"
)

const key plugin.Key = "greeter"

// init declares the plugin and does nothing else -- no config read, no
// connection, no goroutine. The Definition owns the stable key and static
// cardinality; everything that could fail or take time belongs in a lifecycle
// method, where the framework can order it, report it and unwind it.
func init() {
	catalog.Declare(plugin.Definition{
		Key:       key,
		Factory:   func() plugin.Plugin { return new(Plugin) },
		Instances: plugin.SingleInstance,
	})
}

// Plugin needs no base class or identity method. plugin.Plugin is a marker;
// this type participates by implementing web.RouteProvider. Implementations
// may embed plugin.Base when they want its Context conveniences, but it is
// never required.
type Plugin struct{}

// Compile-time proof that this type still satisfies the capability it is
// declared for. Without it, renaming RegisterRoutes or changing its
// signature would not break the build -- the plugin would simply stop being
// detected as a RouteProvider, and the only symptom would be a route that
// silently stopped existing at runtime.
var (
	_ plugin.Plugin     = (*Plugin)(nil)
	_ web.RouteProvider = (*Plugin)(nil)
)

// RegisterRoutes implements web.RouteProvider. web calls it during its own
// Start, after every plugin has finished Init, and before any traffic is
// accepted -- so a route registered here is guaranteed to exist before the
// first request can arrive.
func (p *Plugin) RegisterRoutes(r *web.Router) {
	r.GET("/hello", func(gc *gin.Context) {
		gc.JSON(http.StatusOK, gin.H{"message": "hello from xbc"})
	})
}
