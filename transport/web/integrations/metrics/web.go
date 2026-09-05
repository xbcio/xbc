package metrics

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes the self-protected exposition endpoint only when
// explicitly enabled. Public bypasses authentication plugins; this handler
// still enforces its direct-peer/token policy itself.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	state := p.state.Load()
	if state == nil || !state.config.endpoint.enabled {
		return
	}
	router.GET(state.config.endpoint.path, p.handleMetrics).
		Name("management.metrics").
		Auth(web.Public())
}

func (p *Plugin) handleMetrics(c *gin.Context) {
	state := p.state.Load()
	if state == nil || !state.config.endpoint.enabled {
		web.AbortProblem(c, web.NewProblem(http.StatusNotFound, "not_found"))
		return
	}
	if !authorized(c.Request, state.config.endpoint) {
		c.Header("Cache-Control", "no-store")
		web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
		return
	}
	c.Header("Cache-Control", "no-store")
	promhttp.HandlerFor(state.registry.inner, *state.handler).ServeHTTP(c.Writer, c.Request)
}

func authorized(request *http.Request, cfg endpointConfig) bool {
	if request == nil {
		return false
	}
	loopback := cfg.allowLoopback && directPeerIsLoopback(request.RemoteAddr)
	if !cfg.hasToken {
		return loopback
	}
	values := request.Header.Values(cfg.header)
	candidate := ""
	if len(values) == 1 {
		candidate = values[0]
	}
	candidateDigest := sha256.Sum256([]byte(candidate))
	tokenOK := subtle.ConstantTimeCompare(candidateDigest[:], cfg.tokenDigest[:]) == 1
	return loopback || tokenOK
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
