package pprof

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	stdpprof "net/http/pprof"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes pprof only when explicitly enabled. The routes
// use an explicit public authentication policy because this plugin performs
// its own stricter direct-peer/token
// authorization before invoking any runtime profile handler.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	cfg := p.currentSettings()
	if !cfg.enabled {
		return
	}
	handler := p.handler()
	router.GET(cfg.path, handler).Name("management.pprof.index").Auth(web.Public())
	router.GET(cfg.path+"/*profile", handler).Name("management.pprof.profile").Auth(web.Public())
	router.POST(cfg.path+"/*profile", handler).Name("management.pprof.command").Auth(web.Public())
}

func (p *Plugin) handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := p.currentSettings()
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		if !cfg.enabled || !authorized(c.Request, cfg) {
			web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
			return
		}
		serveProfile(c.Writer, c.Request, cfg.path)
	}
}

func authorized(request *http.Request, cfg settings) bool {
	if request == nil {
		return false
	}
	if cfg.allowLoopback && directPeerIsLoopback(request.RemoteAddr) {
		return true
	}
	if !cfg.hasToken {
		return false
	}
	candidate := sha256.Sum256([]byte(request.Header.Get(cfg.header)))
	return subtle.ConstantTimeCompare(candidate[:], cfg.tokenDigest[:]) == 1
}

func directPeerIsLoopback(remoteAddr string) bool {
	remoteAddr = strings.TrimSpace(remoteAddr)
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
