package pprof

import (
	"context"
	"net/http"
	stdpprof "net/http/pprof"
	"strings"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes pprof only when explicitly enabled.
//
// The routes are registered on the management plane: profiling a process is not
// business traffic, and it is the surface most obviously wrong to expose
// wherever business clients connect. With no management listener configured --
// the default -- they are ordinary routes on the serving listener, exactly as
// they were before the plane existed; see web.Router.Management.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	cfg := p.currentSettings()
	if !cfg.enabled {
		return
	}
	handler := p.handler()
	management := router.Management()
	management.GET(cfg.path, handler).Name("management.pprof.index")
	management.GET(cfg.path+"/*profile", handler).Name("management.pprof.profile")
	management.POST(cfg.path+"/*profile", handler).Name("management.pprof.command")
}

func (p *Plugin) handler() web.Handler {
	return func(_ context.Context, c *web.Ctx) error {
		cfg := p.currentSettings()
		c.SetHeader("Cache-Control", "no-store")
		c.SetHeader("X-Content-Type-Options", "nosniff")
		if !cfg.enabled {
			web.AbortProblem(c, web.NewProblem(http.StatusNotFound, "not_found"))
			return nil
		}
		serveProfile(c.Writer(), c.Request(), cfg.path)
		return nil
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
