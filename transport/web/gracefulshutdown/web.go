package gracefulshutdown

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// RegisterRoutes contributes a self-protected endpoint when explicitly
// enabled. Its public authentication policy bypasses JWT/Casbin; access is
// still denied here unless the direct peer is allowed or a secret token
// matches.
func (p *Plugin) RegisterRoutes(router *web.Router) {
	cfg := p.endpoint
	if !cfg.enabled {
		return
	}
	router.POST(cfg.path, p.handleShutdown).
		Name("management.shutdown").
		Auth(web.Public())
}

func (p *Plugin) handleShutdown(c *gin.Context) {
	cfg := p.endpoint
	if !cfg.enabled || !authorized(c.Request, cfg) {
		// Deliberately use the same response for absent and wrong credentials.
		c.Header("Cache-Control", "no-store")
		web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
		return
	}

	if p.controller == nil || !p.controller.Request("http operator request") {
		c.Header("Cache-Control", "no-store")
		web.AbortProblem(c, web.NewProblem(http.StatusConflict, "shutdown_already_requested"))
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusAccepted, gin.H{"status": "shutting_down"})
}

func authorized(request *http.Request, cfg endpointConfig) bool {
	if request == nil {
		return false
	}
	loopback := cfg.allowLoopback && directPeerIsLoopback(request.RemoteAddr)
	if !cfg.hasToken {
		return loopback
	}
	candidate := request.Header.Get(cfg.header)
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
