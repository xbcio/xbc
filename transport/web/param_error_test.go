package web_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) web.ProblemDetail {
	t.Helper()
	assert.Equal(t, web.ProblemContentType, recorder.Header().Get("Content-Type"))
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	return problem
}

// TestParamErrorMapsBothKnownAndStreamedBodyOverflowTo413 covers the two
// distinct ways an oversized body is detected, which reach 413 through
// different code. A declared Content-Length is rejected by the limiter before
// the handler runs; a streamed body is only discovered while reading, and that
// failure reaches the client solely because ParamError recognizes
// *http.MaxBytesError. Nothing about either path is engine-specific, so both
// are driven through the neutral test engine.
func TestParamErrorMapsBothKnownAndStreamedBodyOverflowTo413(t *testing.T) {
	for _, test := range []struct {
		name          string
		contentLength bool
	}{
		{name: "known content length", contentLength: true},
		{name: "streamed content length", contentLength: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := enginetest.New()
			engine.Use(web.LimitRequestBody(32))
			engine.POST("/requests", func(_ context.Context, c *web.Ctx) error {
				var destination map[string]any
				body, err := io.ReadAll(c.Request().Body)
				if err != nil {
					return web.ParamError(err, &destination)
				}
				return web.ParamError(json.Unmarshal(body, &destination), &destination)
			})

			body := `{"name":"Alice","email":"alice@example.com","address":{"city":"Shanghai"}}`
			request := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			if !test.contentLength {
				request.ContentLength = -1
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			assert.Equal(t, http.StatusRequestEntityTooLarge, response.Code)
			assert.Equal(t, "request_body_too_large", decodeProblem(t, response).Properties["code"])
		})
	}
}

func TestParamErrorTreatsProgrammerErrorsAsInternalFailures(t *testing.T) {
	t.Run("invalid validator target", func(t *testing.T) {
		engine := enginetest.New()
		engine.GET("/requests", func(context.Context, *web.Ctx) error {
			return web.ParamError(&validator.InvalidValidationError{Type: reflect.TypeOf(0)}, nil)
		})
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/requests", nil))

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Equal(t, "internal_server_error", decodeProblem(t, response).Properties["code"])
	})

	assert.NoError(t, web.ParamError(nil, nil))
}

// paramErrorArrayElement is validated twice independently below, standing in
// for two elements of an array request body that each failed validation on
// their own.
type paramErrorArrayElement struct {
	Name  string `json:"name" validate:"required"`
	Email string `json:"email" validate:"required,email"`
}

// TestParamErrorCollectsEveryElementOfAnAggregateFailure pins the neutral
// contract for a binding failure that arrives as a multi-error: every
// element's field errors must reach the client, not just the first one's.
//
// Two failing elements is the minimum that discriminates. With one, an
// implementation that stops at the first match through errors.As alone
// produces exactly the same result, and the assertion would be green either
// way.
func TestParamErrorCollectsEveryElementOfAnAggregateFailure(t *testing.T) {
	validate := validator.New()

	// The first element fails only its "name" field (required); the second
	// fails only its "email" field (email). The two elements are validated
	// independently, then joined with errors.Join -- exactly how an engine
	// adapter is expected to normalize a per-element binding failure.
	firstElement := validate.Struct(paramErrorArrayElement{Email: "alice@example.com"})
	require.Error(t, firstElement, "第一个元素必须因缺少 name 而校验失败")

	secondElement := validate.Struct(paramErrorArrayElement{Name: "Bob", Email: "not-an-email"})
	require.Error(t, secondElement, "第二个元素必须因 email 格式不合法而校验失败")

	var firstAsValidationErrors, secondAsValidationErrors validator.ValidationErrors
	require.True(t, errors.As(firstElement, &firstAsValidationErrors))
	require.True(t, errors.As(secondElement, &secondAsValidationErrors))
	require.Len(t, firstAsValidationErrors, 1, "第一个元素只应因 name 一个字段失败")
	require.Len(t, secondAsValidationErrors, 1, "第二个元素只应因 email 一个字段失败")

	engine := enginetest.New()
	engine.POST("/requests", func(_ context.Context, _ *web.Ctx) error {
		return web.ParamError(
			errors.Join(firstAsValidationErrors, secondAsValidationErrors),
			&[]paramErrorArrayElement{},
		)
	})
	request := httptest.NewRequest(http.MethodPost, "/requests", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)

	assert.Equal(t, http.StatusBadRequest, response.Code)
	problem := decodeProblem(t, response)
	assert.Equal(t, "validation_failed", problem.Properties["code"])

	data, err := json.Marshal(problem.Properties["errors"])
	require.NoError(t, err)
	var fieldErrors []web.FieldError
	require.NoError(t, json.Unmarshal(data, &fieldErrors))

	assert.Len(t, fieldErrors, 2, "聚合失败的每个元素都必须贡献字段错误，不能只取第一个")
	var fields []string
	for _, fieldError := range fieldErrors {
		fields = append(fields, fieldError.Field+":"+fieldError.Code)
	}
	assert.Contains(t, fields, "name:required", "第一个元素的字段错误必须出现")
	assert.Contains(t, fields, "email:email", "第二个元素的字段错误必须出现，不能被第一个元素吞掉")
}

// paramErrorOpaqueAggregate is a multi-error that does not implement
// Unwrap() []error, standing in for an engine's own aggregate type reaching
// ParamError unnormalized (e.g. through the escape hatch documented on
// ParamError).
type paramErrorOpaqueAggregate []error

func (o paramErrorOpaqueAggregate) Error() string { return "opaque aggregate binding failure" }

// TestParamErrorTreatsAnUnnormalizedAggregateAsASafeBadRequest pins the
// bounded escape-hatch fallback: a multi-error type that is not the standard
// library's Unwrap() []error shape cannot be walked for per-field detail, so
// it must still resolve to a safe 400 rather than either a 500 or a
// validation_failed it cannot substantiate.
func TestParamErrorTreatsAnUnnormalizedAggregateAsASafeBadRequest(t *testing.T) {
	opaque := paramErrorOpaqueAggregate{errors.New("first element invalid"), errors.New("second element invalid")}

	engine := enginetest.New()
	engine.POST("/requests", func(_ context.Context, _ *web.Ctx) error {
		return web.ParamError(opaque, &[]paramErrorArrayElement{})
	})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/requests", nil))

	assert.Equal(t, http.StatusBadRequest, response.Code, "未归一化的聚合失败也必须落到安全的 400，不能变成 500")
	assert.Equal(
		t,
		"invalid_request",
		decodeProblem(t, response).Properties["code"],
		"未实现 Unwrap() []error 的聚合不能被误判为逐字段校验失败",
	)
}
