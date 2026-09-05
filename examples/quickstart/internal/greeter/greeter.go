// Package greeter is the Quickstart application's business Plugin. It owns a
// canonical Definition and contributes HTTP routes through an explicit Bundle.
package greeter

import (
	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/biz"
)

// Key is greeter's stable configuration and runtime identity.
const Key plugin.Key = "greeter"

// Plugin contributes the Quickstart's business routes.
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

// Definition returns greeter's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns greeter's side-effect-free explicit composition Bundle.
func Bundle() plugin.Bundle { return bundle }

type greetingRequest struct {
	Name string `json:"name" binding:"required,min=2,max=80"`
}

// RegisterRoutes implements web.RouteContributor.
func (*Plugin) RegisterRoutes(router *web.Router) {
	router.GET("/hello", web.Handle(func(gc *gin.Context) error {
		return biz.OK(gc, gin.H{"message": "hello from xbc"})
	})).Name("greeter.hello").Auth(web.Public())

	router.POST("/hello", web.Handle(func(gc *gin.Context) error {
		var request greetingRequest
		if err := gc.ShouldBindJSON(&request); err != nil {
			return web.ParamError(err, &request)
		}
		return biz.OK(gc, gin.H{"message": "hello " + request.Name + " from xbc"})
	})).Name("greeter.hello.create").Auth(web.Public())
}
