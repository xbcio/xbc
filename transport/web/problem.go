package web

import (
	"encoding/json"
	"net/http"
)

const problemContentType = "application/problem+json; charset=utf-8"

var standardProblemMembers = map[string]struct{}{
	"type":     {},
	"title":    {},
	"status":   {},
	"detail":   {},
	"instance": {},
}

// ProblemDetail is the RFC 9457 response model. Properties contains extension
// members and is flattened into the top-level JSON object, matching RFC 9457
// and Spring's ProblemDetail rather than inventing a nested properties object.
// Standard members always win if Properties contains a conflicting key.
type ProblemDetail struct {
	Type       string         `json:"type"`
	Title      string         `json:"title"`
	Status     int            `json:"status"`
	Detail     string         `json:"detail,omitempty"`
	Instance   string         `json:"instance,omitempty"`
	Properties map[string]any `json:"-"`
}

// FieldError is the stable, safe representation of one invalid request field.
// Code identifies the failed validation constraint; Message never includes the
// rejected value.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// NewProblem creates a valid ProblemDetail and adds XBC's conventional
// machine-readable code extension when code is non-empty.
func NewProblem(status int, code string) ProblemDetail {
	problem := normalizeProblem(ProblemDetail{Status: status}, nil)
	problem.Properties = make(map[string]any, 1)
	if code != "" {
		problem.Properties["code"] = code
	}
	return problem
}

// MarshalJSON flattens RFC 9457 extension members while preventing them from
// replacing the five standard members.
func (p ProblemDetail) MarshalJSON() ([]byte, error) {
	p = normalizeProblem(p, nil)
	members := make(map[string]any, len(p.Properties)+5)
	for name, value := range p.Properties {
		if _, standard := standardProblemMembers[name]; !standard {
			members[name] = value
		}
	}
	members["type"] = p.Type
	members["title"] = p.Title
	members["status"] = p.Status
	if p.Detail != "" {
		members["detail"] = p.Detail
	}
	if p.Instance != "" {
		members["instance"] = p.Instance
	}
	return json.Marshal(members)
}

// UnmarshalJSON collects non-standard members in Properties so a decoded
// ProblemDetail retains the same extension data it would expose on the wire.
func (p *ProblemDetail) UnmarshalJSON(data []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}

	var decoded ProblemDetail
	for name, destination := range map[string]any{
		"type":     &decoded.Type,
		"title":    &decoded.Title,
		"status":   &decoded.Status,
		"detail":   &decoded.Detail,
		"instance": &decoded.Instance,
	} {
		if raw, exists := members[name]; exists {
			if err := json.Unmarshal(raw, destination); err != nil {
				return err
			}
			delete(members, name)
		}
	}
	if len(members) != 0 {
		decoded.Properties = make(map[string]any, len(members))
		for name, raw := range members {
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			decoded.Properties[name] = value
		}
	}
	*p = decoded
	return nil
}

// WriteProblem writes p without aborting the remaining handler chain.
// Most middleware should call AbortProblem instead.
func WriteProblem(c *Ctx, p ProblemDetail) {
	if c == nil || c.Writer() == nil || c.Writer().Written() {
		return
	}
	p = normalizeProblem(p, c)
	payload, err := json.Marshal(p)
	if err != nil {
		p = normalizeProblem(NewProblem(http.StatusInternalServerError, "internal_server_error"), c)
		payload, _ = json.Marshal(p)
	}

	header := c.Writer().Header()
	for _, name := range []string{
		"Content-Encoding", "Content-Length", "Content-Range", "Trailer",
		"Transfer-Encoding", "ETag", "Last-Modified",
	} {
		header.Del(name)
	}
	header.Set("Content-Type", problemContentType)
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	c.Writer().WriteHeader(p.Status)
	_, _ = c.Writer().Write(payload)
}

// AbortProblem stops the handler chain and writes an RFC 9457 response.
func AbortProblem(c *Ctx, p ProblemDetail) {
	if c == nil {
		return
	}
	c.Abort()
	WriteProblem(c, p)
}

func normalizeProblem(p ProblemDetail, c *Ctx) ProblemDetail {
	if p.Status < 100 || p.Status > 599 {
		p.Status = http.StatusInternalServerError
	}
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
		if p.Title == "" {
			p.Title = "HTTP Error"
		}
	}
	if p.Instance == "" && c != nil && c.Request() != nil && c.Request().URL != nil {
		p.Instance = c.Request().URL.EscapedPath()
		if p.Instance == "" {
			p.Instance = c.Request().URL.Path
		}
	}
	return p
}
