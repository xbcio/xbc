package pprof

import (
	"net/http"
	stdpprof "net/http/pprof"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes pprof only when explicitly enabled. Access control
// is delegated to the application's authentication policy.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	cfg := p.currentSettings()
	if !cfg.enabled {
		return
	}
	handler := p.handler()
	router.GET(cfg.path, handler).Name("management.pprof.index")
	router.GET(cfg.path+"/*profile", handler).Name("management.pprof.profile")
	router.POST(cfg.path+"/*profile", handler).Name("management.pprof.command")
}

func (p *Plugin) handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := p.currentSettings()
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		if !cfg.enabled {
			web.AbortProblem(c, web.NewProblem(http.StatusNotFound, "not_found"))
			return
		}
		serveProfile(c.Writer, c.Request, cfg.path)
	}
}

func serveProfile(writer http.ResponseWriter, request *http.Request, basePath string) {
	name := strings.TrimPrefix(request.URL.Path, basePath)
	name = strings.Trim(name, "/")
	switch name {
	case "":
		stdpprof.Index(writer, request)
	case "cmdline":
		stdpprof.Cmdline(writer, request)
	case "profile":
		stdpprof.Profile(writer, request)
	case "symbol":
		stdpprof.Symbol(writer, request)
	case "trace":
		stdpprof.Trace(writer, request)
	default:
		stdpprof.Handler(name).ServeHTTP(writer, request)
	}
}
