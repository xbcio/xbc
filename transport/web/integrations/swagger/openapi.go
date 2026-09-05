package swagger

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/xbcio/xbc/transport/web"
)

var pathParameter = regexp.MustCompile(`(?:^|/)([:*])([^/]+)`)

type openAPIDocument struct {
	OpenAPI    string              `json:"openapi"`
	Info       openAPIInfo         `json:"info"`
	Servers    []openAPIServer     `json:"servers,omitempty"`
	Paths      map[string]pathItem `json:"paths"`
	Components *openAPIComponents  `json:"components,omitempty"`
}

type openAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

type openAPIServer struct {
	URL string `json:"url"`
}

type openAPIComponents struct {
	Schemas         map[string]schema         `json:"schemas"`
	SecuritySchemes map[string]securityScheme `json:"securitySchemes,omitempty"`
}

type securityScheme struct {
	Type   string `json:"type"`
	Scheme string `json:"scheme"`
	Format string `json:"bearerFormat,omitempty"`
}

type pathItem map[string]operation

type operation struct {
	OperationID string                `json:"operationId"`
	Summary     string                `json:"summary,omitempty"`
	Tags        []string              `json:"tags,omitempty"`
	Parameters  []parameter           `json:"parameters,omitempty"`
	Responses   map[string]response   `json:"responses"`
	Security    []map[string][]string `json:"security,omitempty"`
	Public      bool                  `json:"x-xbc-public"`
	Permission  string                `json:"x-xbc-permission,omitempty"`
	Idempotent  bool                  `json:"x-xbc-idempotent"`
}

type parameter struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Schema   schema `json:"schema"`
}

type schema struct {
	Ref         string            `json:"$ref,omitempty"`
	Type        string            `json:"type,omitempty"`
	Format      string            `json:"format,omitempty"`
	Description string            `json:"description,omitempty"`
	Minimum     *int              `json:"minimum,omitempty"`
	Maximum     *int              `json:"maximum,omitempty"`
	Properties  map[string]schema `json:"properties,omitempty"`
	Required    []string          `json:"required,omitempty"`
	Items       *schema           `json:"items,omitempty"`
}

type response struct {
	Description string               `json:"description"`
	Content     map[string]mediaType `json:"content,omitempty"`
}

type mediaType struct {
	Schema schema `json:"schema"`
}

func buildDocument(cfg settings, routes []web.RouteInfo) ([]byte, error) {
	document := openAPIDocument{
		OpenAPI: "3.1.0",
		Info: openAPIInfo{
			Title:       cfg.title,
			Version:     cfg.version,
			Description: cfg.description,
		},
		Paths: make(map[string]pathItem),
		Components: &openAPIComponents{
			Schemas: problemSchemas(),
		},
	}
	for _, server := range cfg.servers {
		document.Servers = append(document.Servers, openAPIServer{URL: server})
	}
	if cfg.bearerAuth {
		document.Components.SecuritySchemes = map[string]securityScheme{
			"bearerAuth": {Type: "http", Scheme: "bearer", Format: "JWT"},
		}
	}

	sorted := append([]web.RouteInfo(nil), routes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Path == sorted[j].Path {
			return sorted[i].Method < sorted[j].Method
		}
		return sorted[i].Path < sorted[j].Path
	})

	operationIDs := make(map[string]web.RouteInfo, len(sorted))
	for _, route := range sorted {
		method := strings.ToLower(strings.TrimSpace(route.Method))
		if !supportedMethod(method) {
			return nil, fmt.Errorf("swagger: route %s %s uses unsupported OpenAPI method", route.Method, route.Path)
		}
		openPath, params, err := convertPath(route.Path)
		if err != nil {
			return nil, fmt.Errorf("swagger: route %s %s: %w", route.Method, route.Path, err)
		}
		operationID := strings.TrimSpace(route.Name)
		if operationID == "" {
			operationID = generatedOperationID(method, openPath)
		}
		if previous, exists := operationIDs[operationID]; exists {
			return nil, fmt.Errorf("swagger: duplicate operationId %q for %s %s and %s %s", operationID, previous.Method, previous.Path, route.Method, route.Path)
		}
		operationIDs[operationID] = route
		public := route.Auth.IsPublic()

		op := operation{
			OperationID: operationID,
			Summary:     route.Name,
			Tags:        routeTags(openPath),
			Parameters:  params,
			Responses: map[string]response{
				"200": {Description: "Successful response"},
				"default": {
					Description: "Problem Details error response",
					Content: map[string]mediaType{
						"application/problem+json": {
							Schema: schema{Ref: "#/components/schemas/ProblemDetail"},
						},
					},
				},
			},
			Public:     public,
			Permission: route.Perm,
			Idempotent: route.Idempotent,
		}
		if cfg.bearerAuth && !public {
			op.Security = []map[string][]string{{"bearerAuth": {}}}
		}
		item := document.Paths[openPath]
		if item == nil {
			item = make(pathItem)
			document.Paths[openPath] = item
		}
		item[method] = op
	}

	return marshalDocument(document)
}

func problemSchemas() map[string]schema {
	minimumStatus, maximumStatus := 100, 599
	return map[string]schema{
		"ProblemDetail": {
			Type:        "object",
			Description: "RFC 9457 Problem Details with XBC machine-readable extensions.",
			Required:    []string{"type", "title", "status"},
			Properties: map[string]schema{
				"type": {
					Type:        "string",
					Format:      "uri-reference",
					Description: "URI reference identifying the problem type.",
				},
				"title": {
					Type:        "string",
					Description: "Short human-readable summary of the problem type.",
				},
				"status": {
					Type:        "integer",
					Description: "HTTP status code generated for this occurrence.",
					Minimum:     &minimumStatus,
					Maximum:     &maximumStatus,
				},
				"detail": {
					Type:        "string",
					Description: "Human-readable explanation specific to this occurrence.",
				},
				"instance": {
					Type:        "string",
					Format:      "uri-reference",
					Description: "URI reference identifying this occurrence without its query string.",
				},
				"code": {
					Type:        "string",
					Description: "Stable XBC application error code.",
				},
				"errors": {
					Type:  "array",
					Items: &schema{Ref: "#/components/schemas/FieldError"},
				},
			},
		},
		"FieldError": {
			Type:     "object",
			Required: []string{"field", "message"},
			Properties: map[string]schema{
				"field":   {Type: "string", Description: "JSON field path."},
				"code":    {Type: "string", Description: "Stable validation constraint code."},
				"message": {Type: "string", Description: "Safe validation message that omits the rejected value."},
			},
		},
	}
}

func supportedMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete,
		http.MethodOptions, http.MethodHead, http.MethodPatch, http.MethodTrace:
		return true
	default:
		return false
	}
}

func convertPath(routePath string) (string, []parameter, error) {
	if routePath == "" || !strings.HasPrefix(routePath, "/") {
		return "", nil, fmt.Errorf("path must be absolute")
	}
	seen := make(map[string]struct{})
	params := make([]parameter, 0)
	invalid := ""
	converted := pathParameter.ReplaceAllStringFunc(routePath, func(match string) string {
		slash := ""
		part := match
		if strings.HasPrefix(part, "/") {
			slash = "/"
			part = strings.TrimPrefix(part, "/")
		}
		name := part[1:]
		if name == "" {
			invalid = match
			return match
		}
		if _, duplicate := seen[name]; duplicate {
			invalid = name
			return match
		}
		seen[name] = struct{}{}
		params = append(params, parameter{Name: name, In: "path", Required: true, Schema: schema{Type: "string"}})
		return slash + "{" + name + "}"
	})
	if invalid != "" {
		return "", nil, fmt.Errorf("invalid or duplicate path parameter %q", invalid)
	}
	return converted, params, nil
}

func generatedOperationID(method, routePath string) string {
	var b strings.Builder
	b.WriteString(method)
	for _, r := range routePath {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func routeTags(routePath string) []string {
	for _, segment := range strings.Split(strings.Trim(routePath, "/"), "/") {
		if segment != "" && !strings.HasPrefix(segment, "{") {
			return []string{segment}
		}
	}
	return nil
}
