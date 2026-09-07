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
// Use NewStatusError when another HTTP error status is semantically correct.
func NewError(code, detail string) *Error {
	return NewStatusError(http.StatusUnprocessableEntity, code, detail)
}

// NewStatusError creates a business failure with an explicit HTTP status.
// Plugin accepts only 4xx/5xx statuses and a non-empty stable code; invalid
// values are converted to a safe internal_server_error response.
func NewStatusError(status int, code, detail string) *Error {
	return &Error{status: status, code: code, detail: detail}
}

// WrapError creates an HTTP 422 business failure that retains cause for error
// chain inspection and internal diagnostics.
func WrapError(cause error, code, detail string) *Error {
	return WrapStatusError(cause, http.StatusUnprocessableEntity, code, detail)
}

// WrapStatusError creates an explicit-status business failure that retains
// cause for error chain inspection and internal diagnostics.
func WrapStatusError(cause error, status int, code, detail string) *Error {
	return &Error{status: status, code: code, detail: detail, cause: cause}
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
