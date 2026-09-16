package biz

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
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

// PaginatedData is the stable payload produced by Paginated. It carries one
// page of items plus the total across all pages; it is not a page number.
//
// The wire name is "list", matching GFA's established pagination contract. The
// Go field carries the same name so both ends of a discussion refer to one
// identifier.
type PaginatedData[T any] struct {
	List  []T   `json:"list"`
	Total int64 `json:"total"`
}

// NewResponse builds a success envelope carrying the invariants every business
// response shares. RequestID stays empty: only a live request can supply one,
// so Write fills it from the transport context.
func NewResponse[T any](data T) Response[T] {
	return Response[T]{Success: true, Code: SuccessCode, Data: data}
}

// Paginated builds a pagination payload for any success writer, so the page
// shape and the HTTP status stay independent: OK(c, Paginated(items, total))
// and Created(c, Paginated(items, total)) are both expressible.
//
// A nil item slice is normalized to an empty JSON array so clients do not need
// separate null handling. A negative total is a caller bug with no safe
// rendering, so it is clamped to zero rather than published.
func Paginated[T any](items []T, total int64) PaginatedData[T] {
	if items == nil {
		items = make([]T, 0)
	}
	if total < 0 {
		total = 0
	}
	return PaginatedData[T]{List: items, Total: total}
}

// Write serializes a successful 2xx response before committing headers. This
// allows web.Handle to route encoding failures through the centralized error
// boundary without emitting a partial success response. Statuses 204 and 205
// are rejected because an envelope body would violate their HTTP semantics.
func Write[T any](c *web.Ctx, status int, data T) error {
	if c == nil || c.Writer() == nil {
		return fmt.Errorf("biz: response context is unavailable")
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices || status == http.StatusNoContent || status == http.StatusResetContent {
		return fmt.Errorf("biz: response status %d cannot carry a success envelope", status)
	}
	if c.Writer().Written() {
		return fmt.Errorf("biz: response is already committed")
	}

	// From already falls back to the standard request context, so reading it
	// once covers both halves of Gin's split context. Do not add a second read
	// against c.Request().Context(): that set is a strict subset of this one.
	response := NewResponse(data)
	if requestID, ok := requestid.From(c); ok {
		response.RequestID = requestID
	}
	// The envelope is encoded with the standard library. Sharing the engine's
	// pluggable codec -- and with it the sonic, go_json and jsoniter build tags
	// -- is no longer available here: this package speaks the neutral Web
	// contract and has no engine to borrow a codec from. An application that
	// builds with one of those tags still gets it for the engine's own
	// responses; only this envelope changes, and it is a small flat struct for
	// which the standard encoder is not the cost that would justify reopening
	// an engine dependency.
	payload, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("biz: encode success response: %w", err)
	}

	c.SetHeader("Content-Type", jsonContentType)
	c.Status(status)
	if _, err := c.Writer().Write(payload); err != nil {
		return fmt.Errorf("biz: write success response: %w", err)
	}
	return nil
}

// OK writes a 200 success envelope.
func OK[T any](c *web.Ctx, data T) error {
	return Write(c, http.StatusOK, data)
}

// Created writes a 201 success envelope.
func Created[T any](c *web.Ctx, data T) error {
	return Write(c, http.StatusCreated, data)
}

// Accepted writes a 202 success envelope.
func Accepted[T any](c *web.Ctx, data T) error {
	return Write(c, http.StatusAccepted, data)
}
