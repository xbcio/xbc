package swagger

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	"github.com/swaggest/swgui/v5emb"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// Key is Swagger's stable Definition and configuration identity.
const Key plugin.Key = "swagger"

type documentSnapshot struct {
	data []byte
}

type handlerSnapshot struct {
	handler http.Handler
}

// Plugin owns the immutable OpenAPI snapshot and documentation routes.
type Plugin struct {
	settings settings
	document atomic.Pointer[documentSnapshot]
	ui       atomic.Pointer[handlerSnapshot]
}

var (
	_ web.RouteContributor     = (*Plugin)(nil)
	_ web.RouteCatalogListener = (*Plugin)(nil)
)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		return newPlugin(cfg)
	},
	plugin.Options[*Plugin]{
		Activation: plugin.WhenConfigured("plugins." + Key.String()),
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *Plugin) web.RouteContributor { return value }),
			plugin.ExportAs[web.RouteCatalogListener](func(value *Plugin) web.RouteCatalogListener { return value }),
		),
	},
)

var bundle = plugin.BundleOf(definition)

// New constructs a directly usable Swagger plugin with production defaults.
func New() *Plugin {
	value, _ := newPlugin(DefaultConfig())
	return value
}

func newPlugin(cfg Config) (*Plugin, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Plugin{settings: normalized}, nil
}

// Definition returns Swagger's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns Swagger's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// RegisterRoutes contributes the JSON and optional embedded UI endpoints.
func (p *Plugin) RegisterRoutes(r *web.Router) {
	cfg := p.settings
	r.GET(cfg.jsonPath, p.serveDocument).
		Name("swagger.openapi").
		Auth(web.Public())
	if !cfg.uiEnabled {
		return
	}

	r.GET(cfg.uiPath, p.serveUI).
		Name("swagger.ui").
		Auth(web.Public())
	r.GET(cfg.uiPath+"/*asset", p.serveUI).
		Name("swagger.assets").
		Auth(web.Public())
}

// RoutesReady builds and atomically publishes a complete document only after
// Web has frozen all application routes.
func (p *Plugin) RoutesReady(routes web.RouteCatalog) error {
	if routes == nil {
		return fmt.Errorf("swagger: route catalog is nil")
	}
	cfg := p.settings
	all := routes.All()
	data, err := buildDocument(cfg, all)
	if err != nil {
		return err
	}

	var uiHandler http.Handler
	if cfg.uiEnabled {
		jsonPath, uiPath := documentationPaths(all, cfg.jsonPath, cfg.uiPath)
		uiHandler = v5emb.New(cfg.title, jsonPath, uiPath+"/")
	}
	if uiHandler != nil {
		p.ui.Store(&handlerSnapshot{handler: uiHandler})
	}
	p.document.Store(&documentSnapshot{data: data})
	return nil
}

// Document returns a defensive copy of the current OpenAPI JSON snapshot.
func (p *Plugin) Document() []byte {
	snapshot := p.document.Load()
	if snapshot == nil {
		return nil
	}
	return append([]byte(nil), snapshot.data...)
}

func documentationPaths(routes []web.RouteInfo, fallbackJSON, fallbackUI string) (jsonPath, uiPath string) {
	for _, route := range routes {
		switch route.Name {
		case "swagger.openapi":
			jsonPath = route.Path
		case "swagger.ui":
			uiPath = route.Path
		}
	}
	if jsonPath == "" {
		jsonPath = fallbackJSON
	}
	if uiPath == "" {
		uiPath = fallbackUI
	}
	return jsonPath, uiPath
}

func (p *Plugin) serveUI(c *gin.Context) {
	snapshot := p.ui.Load()
	if snapshot == nil || snapshot.handler == nil {
		web.AbortProblem(c, web.NewProblem(http.StatusServiceUnavailable, "documentation_not_ready"))
		return
	}
	snapshot.handler.ServeHTTP(c.Writer, c.Request)
}

func (p *Plugin) serveDocument(c *gin.Context) {
	snapshot := p.document.Load()
	if snapshot == nil {
		web.AbortProblem(c, web.NewProblem(http.StatusServiceUnavailable, "documentation_not_ready"))
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", snapshot.data)
}

func marshalDocument(document openAPIDocument) ([]byte, error) {
	data, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("swagger: marshal OpenAPI document: %w", err)
	}
	return data, nil
}
