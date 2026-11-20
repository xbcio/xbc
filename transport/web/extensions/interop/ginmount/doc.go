// Package ginmount mounts a complete, existing Gin engine beneath the XBC Web
// route table as one mount row.
//
// A host with an established Gin application keeps its controllers, its Gin
// middleware, and its handler signatures. XBC declares routes through
// web.Router, which owns no third-party engine, so the two cannot meet on
// their own. This plugin is the bridge: a Registrar contributes one Gin
// subtree, and the plugin mounts that subtree as a single web.Router mount
// row. The host's recovery, access log, request id, session, and security
// middleware are installed on the engine by the registrar itself, so they keep
// running exactly where they ran before.
//
// # Mounting
//
// Every Registrar names a literal prefix such as "/api/v1" and installs its
// routes relative to it. Registrars that name the same prefix share one engine,
// which is how a host that already splits its Gin setup across several
// registration functions keeps a single engine and a single middleware stack.
// The plugin mounts each prefix once, with the prefix stripped before the
// engine sees the request, so the engine's own routing sees the same relative
// paths it always did.
//
// The mounted subtree is declared web.Public() on the XBC side. That is a
// statement about where authentication happens, not an exemption from it: the
// engine carries the host's own authentication middleware, and XBC's
// authentication chain is not what protects a Gin subtree. Everything XBC
// installs outside the mount -- request ids, access logs, body limits, the
// in-flight gate -- still wraps every request into it.
//
// A path inside a mounted subtree that the inner engine does not match is
// answered by Gin's own 404 (or 405 for a matched path with an unmatched
// method), not by XBC's problem-details 404, because once the prefix is
// stripped the engine owns matching. That is the price of handing a whole
// router to another framework; an application that needs XBC's error
// vocabulary on a Gin handler can migrate that one route instead.
//
// # MountPath constraints
//
// MountPath is the literal, non-root prefix the subtree is served beneath. It
// is refused when it is empty, does not start with "/", is the root, ends with
// "/", or carries Gin routing syntax (":" or "*"). A mount owns a whole
// subtree, and each of those shapes either names no subtree or claims one the
// route table cannot describe as a literal row. The prefix is read once, when
// the plugin is built, so what validation approved is what gets mounted.
//
// # Per-route migration
//
// transport/web/engines/ginx.Handler adapts one gin.HandlerFunc to a web.Handler,
// for a route that should live in the XBC route table while its body is still
// Gin-shaped. It is the counterpart of this package: Handler moves one route at
// a time and keeps XBC's route metadata, authentication policy, and error
// boundary around it, while this package moves an entire existing engine and
// leaves matching and error rendering to Gin.
//
// # Configuration
//
//	plugins:
//	  ginmount:
//	    enabled: true
//
// Presenting the section activates the plugin. A Bundle can be composed without
// the section, in which case the plugin contributes nothing; a Registrar must
// still be exported by some enabled plugin for anything to be mounted.
//
// # Usage
//
// Implement Registrar on the plugin that owns a Gin subtree and export it as a
// contract input of this plugin:
//
//	type API struct{ controller *Controller }
//
//	func (*API) MountPath() string { return "/api/v1" }
//
//	func (a *API) RegisterGin(engine *gin.Engine) {
//		engine.Use(recovery.New(), accesslog.New())
//		a.controller.Setup(engine.Group(""))
//	}
//
//	var apiDefinition = plugin.Define(
//		"api",
//		func(plugin.BuildContext) (*API, error) { return &API{}, nil },
//		plugin.Options[*API]{
//			Exports: plugin.Contracts(
//				plugin.ExportAs[ginmount.Registrar](func(a *API) ginmount.Registrar { return a }),
//			),
//		},
//	)
//
// Once the plugin is composed and its section is present, compose the Bundle of
// this package as well; it resolves every exported Registrar and mounts each
// subtree before the route table freezes. A controller list that already
// implements Setup can be adapted with NewControllerRegistrar instead of a
// hand-written Registrar:
//
//	registrar := ginmount.NewControllerRegistrar("/api/v1", groupHandlers, controllers...)
//
// A single handler can also be adopted without an engine:
//
//	router.GET("/legacy/:id", ginengine.Handler(legacyHandler)).Auth(web.Public())
package ginmount
