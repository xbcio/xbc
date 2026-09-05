package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web/requestid"
)

const (
	// SuccessCode is the stable machine code used by successful responses.
	SuccessCode     = "0"
	jsonContentType = "application/json; charset=utf-8"
)

// Response is the opt-in business success envelope inspired by GFA. HTTP
// failures deliberately do not use this type; they remain RFC 9457 Problem
// Details with semantic status codes.
type Response[T any] struct {
	Success   bool   `json:"success"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Data      T      `json:"data"`
	RequestID string `json:"requestId,omitempty"`
}

// Page is the stable payload used by Paginated.
type Page[T any] struct {
	Items []T   `json:"items"`
	Total int64 `json:"total"`
}

// NewResponse builds a success envelope and propagates a validated request ID
// when the requestid plugin placed one in ctx.
func NewResponse[T any](ctx context.Context, data T) Response[T] {
	response := Response[T]{Success: true, Code: SuccessCode, Data: data}
	if requestID, ok := requestid.FromContext(ctx); ok {
		response.RequestID = requestID
	}
	return response
}

// NewPage builds a pagination payload. A nil item slice is normalized to an
// empty JSON array so clients do not need separate null handling.
func NewPage[T any](items []T, total int64) Page[T] {
	if items == nil {
		items = make([]T, 0)
	}
	return Page[T]{Items: items, Total: total}
}

// Write serializes a successful 2xx response before committing headers. This
// allows web.Handle to route encoding failures through the centralized error
// boundary without emitting a partial success response. Statuses 204 and 205
// are rejected because an envelope body would violate their HTTP semantics.
func Write[T any](c *gin.Context, status int, data T) error {
	if c == nil || c.Writer == nil {
		return fmt.Errorf("biz: response context is unavailable")
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || status == http.StatusNoContent || status == http.StatusResetContent {
		return fmt.Errorf("biz: response status %d cannot carry a success envelope", status)
	}
	if c.Writer.Written() {
		return fmt.Errorf("biz: response is already committed")
	}

	ctx := context.Background()
	if c.Request != nil {
		ctx = c.Request.Context()
	}
	response := NewResponse(ctx, data)
	if requestID, ok := requestid.FromGin(c); ok {
		response.RequestID = requestID
	}
	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("biz: encode success response: %w", err)
	}

	c.Header("Content-Type", jsonContentType)
	c.Status(status)
	if _, err := c.Writer.Write(payload); err != nil {
		return fmt.Errorf("biz: write success response: %w", err)
	}
	return nil
}

// OK writes a 200 success envelope.
func OK[T any](c *gin.Context, data T) error {
	return Write(c, http.StatusOK, data)
}

// Created writes a 201 success envelope.
func Created[T any](c *gin.Context, data T) error {
	return Write(c, http.StatusCreated, data)
}

// Accepted writes a 202 success envelope.
func Accepted[T any](c *gin.Context, data T) error {
	return Write(c, http.StatusAccepted, data)
}

// Paginated writes a 200 success envelope containing Page. total must be
// non-negative; an invalid value is treated as an internal programming error.
func Paginated[T any](c *gin.Context, items []T, total int64) error {
	if total < 0 {
		return fmt.Errorf("biz: pagination total cannot be negative")
	}
	return OK(c, NewPage(items, total))
}
