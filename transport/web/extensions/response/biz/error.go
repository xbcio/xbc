package biz

import (
	"fmt"
	"net/http"
)

// Error is an explicitly public, HTTP-facing business failure. It belongs at
// the application/transport boundary; domain packages should continue to
// return their own protocol-neutral errors and map them at that boundary.
//
// Error values are immutable after construction. Cause is retained for
// errors.Is/errors.As and internal diagnostics, but is never copied into the
// public Problem Detail by Plugin.
type Error struct {
	status int
	code   string
	detail string
	cause  error
}

// NewError creates a business-rule failure with HTTP 422 Unprocessable Entity.
// Status and cause are optional dimensions supplied by WithStatus and WithCause
// rather than by one constructor per combination, so a third dimension would
// add one method instead of doubling the constructor set.
func NewError(code, detail string) *Error {
	return &Error{status: http.StatusUnprocessableEntity, code: code, detail: detail}
}

// WithStatus returns a copy published with an explicit HTTP status. Plugin
// accepts only 4xx/5xx statuses; an invalid value is converted to a safe
// internal_server_error response rather than leaking the original.
func (e *Error) WithStatus(status int) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.status = status
	return &clone
}

// WithCause returns a copy retaining cause for errors.Is/errors.As and internal
// diagnostics. Because the receiver is copied rather than mutated, a package
// level Error stays usable as a template: ErrOrderClosed.WithCause(dbErr) never
// writes back into ErrOrderClosed.
func (e *Error) WithCause(cause error) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.cause = cause
	return &clone
}

// WithDetail returns a copy whose detail is the receiver's detail treated as a
// printf format and filled with args. It completes the template pattern that
// WithStatus and WithCause start: a package-level Error carries the wording
// once and each call site supplies only the values that vary.
//
//	var ErrQuotaExhausted = biz.NewError("QUOTA.EXHAUSTED", "Quota of %d is exhausted.")
//
//	return ErrQuotaExhausted.WithDetail(limit)
//
// args are published to the client verbatim, so pass only client-safe values.
// An internal cause belongs in WithCause, which never reaches the response.
func (e *Error) WithDetail(args ...any) *Error {
	if e == nil {
		return nil
	}
	clone := *e
	clone.detail = fmt.Sprintf(clone.detail, args...)
	return &clone
}

// Error implements error. A wrapped cause is included for internal logging;
// Plugin publishes Detail rather than this string, so the cause stays private.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.detail
	if message == "" {
		message = e.code
	}
	if message == "" {
		message = "biz error"
	}
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", message, e.cause)
	}
	return message
}

// Unwrap exposes the internal cause to errors.Is/errors.As.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// Status returns the intended HTTP status.
func (e *Error) Status() int {
	if e == nil {
		return 0
	}
	return e.status
}

// Code returns the stable, machine-readable business code.
func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

// Detail returns the public, client-safe explanation.
func (e *Error) Detail() string {
	if e == nil {
		return ""
	}
	return e.detail
}

// maxStatusCode is the top of the HTTP status code space. RFC 9110 assigns no
// class above 5xx, so a larger value is a programming error rather than a
// status this package could publish.
const maxStatusCode = 599

// publishableStatus reports whether status may leave the boundary as-is. Only
// 4xx and 5xx qualify: a success or redirect status would contradict the
// Problem Detail the failure is rendered into.
func publishableStatus(status int) bool {
	return status >= http.StatusBadRequest && status <= maxStatusCode
}

func validCode(code string) bool {
	if code == "" || len(code) > 128 {
		return false
	}
	for _, ch := range []byte(code) {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-' {
			continue
		}
		return false
	}
	return true
}
