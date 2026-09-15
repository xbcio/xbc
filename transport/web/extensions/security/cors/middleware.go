package cors

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xbcio/xbc/transport/web"
)

type policy struct {
	origins          map[string]struct{}
	anyOrigin        bool
	methods          []string
	methodSet        map[string]struct{}
	anyMethod        bool
	headers          []string
	headerSet        map[string]struct{}
	anyHeader        bool
	exposeHeaders    []string
	allowCredentials bool
	maxAgeSeconds    int64
}

func compilePolicy(cfg Config) (*policy, error) {
	origins, anyOrigin, err := normalizeOrigins(cfg.AllowOrigins)
	if err != nil {
		return nil, err
	}
	if anyOrigin && cfg.AllowCredentials {
		return nil, &ConfigError{Field: "allow_credentials", Message: "cannot be true when allow_origins is wildcard"}
	}
	methods, methodSet, anyMethod, err := normalizeMethods(cfg.AllowMethods)
	if err != nil {
		return nil, err
	}
	headers, headerSet, anyHeader, err := normalizeHeaders("allow_headers", cfg.AllowHeaders, true)
	if err != nil {
		return nil, err
	}
	exposed, _, anyExposedHeader, err := normalizeHeaders("expose_headers", cfg.ExposeHeaders, true)
	if err != nil {
		return nil, err
	}
	if anyExposedHeader && cfg.AllowCredentials {
		return nil, &ConfigError{Field: "expose_headers", Message: "cannot be wildcard when allow_credentials is true"}
	}
	if cfg.MaxAge < 0 {
		return nil, &ConfigError{Field: "max_age", Message: "cannot be negative"}
	}

	return &policy{
		origins:          origins,
		anyOrigin:        anyOrigin,
		methods:          methods,
		methodSet:        methodSet,
		anyMethod:        anyMethod,
		headers:          headers,
		headerSet:        headerSet,
		anyHeader:        anyHeader,
		exposeHeaders:    exposed,
		allowCredentials: cfg.AllowCredentials,
		maxAgeSeconds:    int64(cfg.MaxAge / time.Second),
	}, nil
}

// ConfigError identifies an invalid semantic configuration field.
type ConfigError struct {
	Field   string
	Message string
}

func (e *ConfigError) Error() string { return "cors: " + e.Field + " " + e.Message }

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	p.mu.RLock()
	compiled := p.policy
	p.mu.RUnlock()
	if compiled == nil {
		web.AbortProblem(c, web.NewProblem(http.StatusInternalServerError, "internal_server_error"))
		return nil
	}
	compiled.handle(c)
	return nil
}

func (p *policy) handle(c *web.Ctx) {
	origin := c.GetHeader("Origin")
	if origin == "" {
		c.Next()
		return
	}

	if !p.originAllowed(origin) {
		addVary(c.Writer().Header(), "Origin")
		web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
		return
	}

	requestedMethod := c.GetHeader("Access-Control-Request-Method")
	preflight := c.Request().Method == http.MethodOptions && requestedMethod != ""
	var requestedHeaders []string
	if preflight {
		var ok bool
		requestedHeaders, ok = parseRequestedHeaders(c.GetHeader("Access-Control-Request-Headers"))
		if !ok || !p.preflightAllowed(requestedMethod, requestedHeaders) {
			addPreflightVary(c.Writer().Header())
			web.AbortProblem(c, web.NewProblem(http.StatusForbidden, "forbidden"))
			return
		}
	}

	p.writeOriginHeaders(c, origin)
	if !preflight {
		if len(p.exposeHeaders) != 0 {
			c.SetHeader("Access-Control-Expose-Headers", strings.Join(p.exposeHeaders, ", "))
		}
		c.Next()
		return
	}

	addPreflightVary(c.Writer().Header())
	if p.anyMethod {
		c.SetHeader("Access-Control-Allow-Methods", strings.TrimSpace(requestedMethod))
	} else {
		c.SetHeader("Access-Control-Allow-Methods", strings.Join(p.methods, ", "))
	}
	if p.anyHeader {
		if len(requestedHeaders) != 0 {
			c.SetHeader("Access-Control-Allow-Headers", strings.Join(requestedHeaders, ", "))
		}
	} else if len(p.headers) != 0 {
		c.SetHeader("Access-Control-Allow-Headers", strings.Join(p.headers, ", "))
	}
	if p.maxAgeSeconds > 0 {
		c.SetHeader("Access-Control-Max-Age", strconv.FormatInt(p.maxAgeSeconds, 10))
	}
	// A preflight response has no body, but the response must still be
	// committed here: gin's Status/WriteHeader only records a pending status
	// code, it does not itself flip Written() to true (that happens on the
	// underlying writer's first Write, via WriteHeaderNow). Writing a nil body
	// commits the pending 204 immediately, so the shared "don't double-write
	// the response" guards in biz, recovery, and problem observe Written() ==
	// true through the same ResponseWriter this middleware just wrote to. See
	// TestPreflightAbortIsObservableAsWrittenByOuterMiddleware.
	c.Status(http.StatusNoContent)
	_, _ = c.Writer().Write(nil)
	c.Abort()
}

func (p *policy) originAllowed(origin string) bool {
	if containsControl(origin) {
		return false
	}
	if p.anyOrigin {
		return true
	}
	_, ok := p.origins[origin]
	return ok
}

func (p *policy) preflightAllowed(method string, headers []string) bool {
	method = strings.TrimSpace(method)
	if !isToken(method) {
		return false
	}
	if _, ok := p.methodSet[method]; !p.anyMethod && !ok {
		return false
	}
	if p.anyHeader {
		return true
	}
	for _, header := range headers {
		if _, ok := p.headerSet[strings.ToLower(header)]; !ok {
			return false
		}
	}
	return true
}

func (p *policy) writeOriginHeaders(c *web.Ctx, origin string) {
	if p.anyOrigin {
		c.SetHeader("Access-Control-Allow-Origin", "*")
	} else {
		c.SetHeader("Access-Control-Allow-Origin", origin)
		addVary(c.Writer().Header(), "Origin")
	}
	if p.allowCredentials {
		c.SetHeader("Access-Control-Allow-Credentials", "true")
	}
}

func parseRequestedHeaders(raw string) ([]string, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		header := strings.TrimSpace(part)
		if !isToken(header) {
			return nil, false
		}
		key := strings.ToLower(header)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, canonicalHeaderName(header))
	}
	return result, true
}

func addPreflightVary(header http.Header) {
	addVary(header, "Origin")
	addVary(header, "Access-Control-Request-Method")
	addVary(header, "Access-Control-Request-Headers")
}

func addVary(header http.Header, value string) {
	for _, line := range header.Values("Vary") {
		for _, existing := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(existing), value) || strings.TrimSpace(existing) == "*" {
				return
			}
		}
	}
	header.Add("Vary", value)
}
