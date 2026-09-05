package apikey

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

func (p *Plugin) authenticate(c *gin.Context) {
	if route, ok := web.CurrentRoute(c); ok && route.Auth.IsPublic() {
		c.Next()
		return
	}
	if p == nil {
		unauthorized(c, defaultScheme)
		return
	}
	state := p.state
	if state == nil || state.repo == nil {
		unauthorized(c, defaultScheme)
		return
	}
	presented, appID, ok := extractCredential(c, state.config)
	if !ok || len([]byte(presented)) < state.config.minKeyBytes {
		unauthorized(c, state.config.bearerScheme)
		return
	}
	if state.config.requireAppID && appID == "" {
		unauthorized(c, state.config.bearerScheme)
		return
	}

	digest := HashKey(presented)
	credential, found, err := state.repo.Lookup(c.Request.Context(), appID, digest)
	// Repository diagnostics are intentionally collapsed into one response so
	// storage failures cannot become a credential-enumeration oracle.
	if err != nil || !found || strings.TrimSpace(credential.Subject) == "" {
		unauthorized(c, state.config.bearerScheme)
		return
	}
	attributes := cloneAttributes(credential.Attributes)
	if attributes == nil {
		attributes = make(map[string]any, 2)
	}
	if credential.ID != "" {
		attributes["credential_id"] = credential.ID
	}
	if credential.AppID != "" {
		attributes["app_id"] = credential.AppID
	}
	if !web.SetPrincipal(c, web.Principal{
		Subject:    credential.Subject,
		AuthMethod: "apikey",
		Attributes: attributes,
	}) {
		unauthorized(c, state.config.bearerScheme)
		return
	}
	c.Next()
}

func extractCredential(c *gin.Context, cfg normalizedConfig) (secret, appID string, ok bool) {
	headerKey, keyPresent, keyValid := singleHeader(c, cfg.header)
	authorization, authorizationPresent, authorizationValid := singleHeader(c, "Authorization")
	appID, _, appIDValid := singleHeader(c, cfg.appIDHeader)
	if !keyValid || !authorizationValid || !appIDValid || keyPresent && authorizationPresent {
		return "", "", false
	}
	if keyPresent {
		return headerKey, appID, true
	}
	if !cfg.allowBearer || !authorizationPresent {
		return "", "", false
	}
	fields := strings.Fields(authorization)
	if len(fields) != 2 || !strings.EqualFold(fields[0], cfg.bearerScheme) || fields[1] == "" {
		return "", "", false
	}
	return fields[1], appID, true
}

// singleHeader rejects duplicate and blank-present header values instead of
// letting net/http's first-value behavior choose one credential ambiguously.
func singleHeader(c *gin.Context, name string) (value string, present, valid bool) {
	if c == nil || c.Request == nil {
		return "", false, false
	}
	values := c.Request.Header.Values(name)
	if len(values) == 0 {
		return "", false, true
	}
	if len(values) != 1 {
		return "", true, false
	}
	raw := values[0]
	value = strings.TrimSpace(raw)
	if value == "" || value != raw {
		return "", true, false
	}
	return value, true, true
}

func unauthorized(c *gin.Context, scheme string) {
	if scheme != "" {
		c.Header("WWW-Authenticate", scheme)
	}
	web.AbortProblem(c, web.NewProblem(http.StatusUnauthorized, "unauthorized"))
}
