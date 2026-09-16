package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/go-playground/validator/v10"
)

// requestError is an internal transport failure understood by web.onerror's
// built-in mappings. Its Error method exposes only a stable code; decoder and
// validator details (which may contain rejected values) never reach clients.
type requestError struct {
	status      int
	code        string
	detail      string
	fieldErrors []FieldError
	cause       error
}

func (e *requestError) Error() string {
	if e == nil || e.code == "" {
		return "request_error"
	}
	return e.code
}

func (e *requestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *requestError) problem() ProblemDetail {
	if e == nil {
		return NewProblem(http.StatusInternalServerError, "internal_server_error")
	}
	problem := NewProblem(e.status, e.code)
	problem.Detail = e.detail
	if len(e.fieldErrors) != 0 {
		problem.Properties["errors"] = append([]FieldError(nil), e.fieldErrors...)
	}
	return problem
}

func newRequestError(status int, code, detail string, cause error) error {
	return &requestError{status: status, code: code, detail: detail, cause: cause}
}

// ParamError adapts an error returned by the engine's binding to Web's
// centralized onerror/Problem Detail contract. It does not select a binder,
// read the request, or run validation; the engine remains the single owner of
// binding. target is the value that was bound, and is used only to translate
// validator field paths to their public json, form, uri, or header names.
//
// An aggregate failure -- produced when each element of an array body is
// validated separately -- is recognized through the standard library's
// multi-error shape, Unwrap() []error. Ctx.Bind and Ctx.BindURI guarantee it:
// an engine whose binder reports aggregates as its own opaque type normalizes
// them in its adapter. An error handed here straight from an engine's native
// binder through the escape hatch may therefore arrive unnormalized. It still
// maps to a safe 400, but as invalid_request rather than a validation_failed
// carrying per-field errors.
func ParamError(err error, target any) error {
	if err == nil {
		return nil
	}
	if invalidBindingUsage(err) {
		return newRequestError(http.StatusInternalServerError, "internal_server_error", "", err)
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return newRequestError(http.StatusRequestEntityTooLarge, "request_body_too_large", "", err)
	}

	if fieldErrors, ok := validationFieldErrors(err, reflect.TypeOf(target)); ok {
		return &requestError{
			status:      http.StatusBadRequest,
			code:        "validation_failed",
			detail:      "Request validation failed.",
			fieldErrors: fieldErrors,
			cause:       err,
		}
	}
	if isJSONDecodeError(err) {
		return newRequestError(
			http.StatusBadRequest,
			"invalid_request_body",
			"Request body could not be decoded.",
			err,
		)
	}
	return newRequestError(
		http.StatusBadRequest,
		"invalid_request",
		"Request data is invalid.",
		err,
	)
}

func invalidBindingUsage(err error) bool {
	var invalidUnmarshal *json.InvalidUnmarshalError
	if errors.As(err, &invalidUnmarshal) {
		return true
	}
	var invalidValidation *validator.InvalidValidationError
	if errors.As(err, &invalidValidation) {
		return true
	}
	if items, ok := asAggregateErrors(err); ok {
		for _, item := range items {
			if invalidBindingUsage(item) {
				return true
			}
		}
	}
	return false
}

func isJSONDecodeError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var syntaxError *json.SyntaxError
	if errors.As(err, &syntaxError) {
		return true
	}
	var typeError *json.UnmarshalTypeError
	return errors.As(err, &typeError)
}

func validationFieldErrors(err error, root reflect.Type) ([]FieldError, bool) {
	var fieldErrors []validator.FieldError
	if !collectFieldErrors(err, &fieldErrors) || len(fieldErrors) == 0 {
		return nil, false
	}
	errors := make([]FieldError, 0, len(fieldErrors))
	for _, fieldError := range fieldErrors {
		errors = append(errors, FieldError{
			Field:   bindingValidationPath(root, fieldError.StructNamespace()),
			Code:    fieldError.Tag(),
			Message: validationMessage(fieldError.Tag()),
		})
	}
	sort.SliceStable(errors, func(i, j int) bool {
		if errors[i].Field == errors[j].Field {
			return errors[i].Code < errors[j].Code
		}
		return errors[i].Field < errors[j].Field
	})
	return errors, true
}

// collectFieldErrors walks err for every validator.FieldError it carries.
//
// The aggregate check runs before the plain validator.ValidationErrors check,
// and that order is load-bearing, not cosmetic. errors.As on a concrete type
// stops at the first match it finds while walking Unwrap() []error, so
// errors.As(errors.Join(v1, v2), &validationErrors) binds only v1 and silently
// drops v2's field errors. Recognizing the aggregate shape first and
// recursing into every element ourselves is what makes every element's field
// errors reach the client instead of only the first one's.
func collectFieldErrors(err error, destination *[]validator.FieldError) bool {
	if err == nil {
		return true
	}
	if items, ok := asAggregateErrors(err); ok {
		for _, item := range items {
			if !collectFieldErrors(item, destination) {
				return false
			}
		}
		return true
	}
	var validationErrors validator.ValidationErrors
	if errors.As(err, &validationErrors) {
		for _, fieldError := range validationErrors {
			*destination = append(*destination, fieldError)
		}
		return true
	}
	return false
}

// asAggregateErrors reports the elements of a multi-error. The shape checked
// here is the standard library's: errors.Join produces it, errors.Is and
// errors.As already traverse it, and any engine adapter can produce it without
// this package naming an engine type.
//
// errors.As alone is not a substitute. It stops at the first match, and the
// whole point of an aggregate binding failure -- one element of an array body
// failed validation, and so did another -- is that every element's field errors
// must reach the client.
func asAggregateErrors(err error) ([]error, bool) {
	var aggregate interface{ Unwrap() []error }
	if errors.As(err, &aggregate) {
		return aggregate.Unwrap(), true
	}
	return nil, false
}

func validationMessage(rule string) string {
	switch rule {
	case "required", "required_if", "required_unless", "required_with", "required_without":
		return "is required"
	case "email":
		return "must be a valid email address"
	case "url", "uri":
		return "must be a valid URL"
	case "uuid", "uuid3", "uuid4", "uuid5":
		return "must be a valid UUID"
	case "min", "gt", "gte":
		return "does not meet the minimum constraint"
	case "max", "lt", "lte":
		return "exceeds the maximum constraint"
	case "oneof":
		return "must be one of the allowed values"
	default:
		return "is invalid"
	}
}

func bindingValidationPath(root reflect.Type, namespace string) string {
	root = bindingRootType(root)
	parts := strings.Split(namespace, ".")
	if root != nil && len(parts) > 0 && parts[0] == root.Name() {
		parts = parts[1:]
	}
	current := root
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		fieldName, indexes := splitValidationSegment(part)
		for current != nil && current.Kind() == reflect.Pointer {
			current = current.Elem()
		}
		name := fieldName
		if current != nil && current.Kind() == reflect.Struct {
			if field, found := current.FieldByName(fieldName); found {
				name = bindingFieldName(field)
				current = field.Type
			}
		}
		result = append(result, name+indexes)
		for range strings.Count(indexes, "[") {
			for current != nil && current.Kind() == reflect.Pointer {
				current = current.Elem()
			}
			if current == nil {
				break
			}
			switch current.Kind() {
			case reflect.Array, reflect.Slice, reflect.Map:
				current = current.Elem()
			}
		}
	}
	if len(result) == 0 {
		return "request"
	}
	return strings.Join(result, ".")
}

func bindingRootType(root reflect.Type) reflect.Type {
	for root != nil {
		switch root.Kind() {
		case reflect.Pointer, reflect.Array, reflect.Slice:
			root = root.Elem()
		default:
			return root
		}
	}
	return nil
}

func splitValidationSegment(segment string) (string, string) {
	index := strings.IndexByte(segment, '[')
	if index < 0 {
		return segment, ""
	}
	var sanitized strings.Builder
	rest := segment[index:]
	for len(rest) > 0 {
		open := strings.IndexByte(rest, '[')
		close := strings.IndexByte(rest, ']')
		if open < 0 || close < open {
			break
		}
		value := rest[open+1 : close]
		numeric := value != ""
		for _, char := range value {
			if char < '0' || char > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			sanitized.WriteString("[")
			sanitized.WriteString(value)
			sanitized.WriteString("]")
		} else {
			sanitized.WriteString("[*]")
		}
		rest = rest[close+1:]
	}
	return segment[:index], sanitized.String()
}

func bindingFieldName(field reflect.StructField) string {
	for _, key := range [...]string{"json", "form", "uri", "header"} {
		name := strings.SplitN(field.Tag.Get(key), ",", 2)[0]
		if name != "" && name != "-" {
			return name
		}
	}
	return field.Name
}
