package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bindingAddress struct {
	City string `json:"city" binding:"required"`
}

type bindingRequest struct {
	Name    string         `json:"name" binding:"required,min=3"`
	Email   string         `json:"email" binding:"required,email"`
	Address bindingAddress `json:"address" binding:"required"`
}

func serveJSONBindingRequest(body string, maximum int64, destination any) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	if maximum > 0 {
		engine.Use(Handle(limitRequestBody(maximum)))
	}
	// These tests exercise ParamError's mapping of raw binding failures, so they
	// call gin's binding directly rather than Ctx.Bind, which would wrap the
	// error in ParamError before the test ever sees it.
	engine.POST("/requests", Handle(func(_ context.Context, c *Ctx) error {
		if err := c.Gin().ShouldBindJSON(destination); err != nil {
			return ParamError(err, destination)
		}
		c.Status(http.StatusNoContent)
		return nil
	}))
	request := httptest.NewRequest(http.MethodPost, "/requests?secret=query", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func decodeProblem(t *testing.T, recorder *httptest.ResponseRecorder) ProblemDetail {
	t.Helper()
	assert.Equal(t, problemContentType, recorder.Header().Get("Content-Type"))
	var problem ProblemDetail
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	return problem
}

func decodeFieldErrors(t *testing.T, problem ProblemDetail) []FieldError {
	t.Helper()
	data, err := json.Marshal(problem.Properties["errors"])
	require.NoError(t, err)
	var fieldErrors []FieldError
	require.NoError(t, json.Unmarshal(data, &fieldErrors))
	return fieldErrors
}

func TestParamErrorAdaptsGinJSONBinding(t *testing.T) {
	var destination bindingRequest
	response := serveJSONBindingRequest(
		`{"name":"Alice","email":"alice@example.com","address":{"city":"Shanghai"}}`,
		1024,
		&destination,
	)

	assert.Equal(t, http.StatusNoContent, response.Code)
	assert.Equal(t, "Alice", destination.Name)
	assert.Equal(t, "Shanghai", destination.Address.City)
}

func TestParamErrorMapsGinJSONDecodeFailures(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty body", body: ``},
		{name: "malformed body", body: `{"name":"private-secret"`},
		{name: "wrong field type", body: `{"name":42}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var destination bindingRequest
			response := serveJSONBindingRequest(test.body, 4096, &destination)

			assert.Equal(t, http.StatusBadRequest, response.Code)
			problem := decodeProblem(t, response)
			assert.Equal(t, "invalid_request_body", problem.Properties["code"])
			assert.Equal(t, "/requests", problem.Instance)
			assert.NotContains(t, response.Body.String(), "private-secret")
			assert.NotContains(t, response.Body.String(), "query")
		})
	}
}

func TestParamErrorReturnsFieldErrorsWithoutRejectedValues(t *testing.T) {
	var destination bindingRequest
	response := serveJSONBindingRequest(
		`{"name":"xy","email":"private-secret","address":{}}`,
		4096,
		&destination,
	)

	assert.Equal(t, http.StatusBadRequest, response.Code)
	problem := decodeProblem(t, response)
	assert.Equal(t, "validation_failed", problem.Properties["code"])
	assert.Equal(t, []FieldError{
		{Field: "address.city", Code: "required", Message: "is required"},
		{Field: "email", Code: "email", Message: "must be a valid email address"},
		{Field: "name", Code: "min", Message: "does not meet the minimum constraint"},
	}, decodeFieldErrors(t, problem))
	assert.NotContains(t, response.Body.String(), "private-secret")
}

func TestParamErrorAdaptsGinQueryBinding(t *testing.T) {
	type queryRequest struct {
		Page int    `form:"page" binding:"gte=1"`
		Sort string `form:"sort" binding:"oneof=name created"`
	}

	serve := func(rawQuery string) *httptest.ResponseRecorder {
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		engine.GET("/requests", Handle(func(_ context.Context, c *Ctx) error {
			var request queryRequest
			if err := c.Gin().ShouldBindQuery(&request); err != nil {
				return ParamError(err, &request)
			}
			c.Status(http.StatusNoContent)
			return nil
		}))
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/requests?"+rawQuery, nil)
		engine.ServeHTTP(response, request)
		return response
	}

	t.Run("validator errors use form field names", func(t *testing.T) {
		response := serve("page=0&sort=private-secret")

		assert.Equal(t, http.StatusBadRequest, response.Code)
		problem := decodeProblem(t, response)
		assert.Equal(t, "validation_failed", problem.Properties["code"])
		assert.Equal(t, []FieldError{
			{Field: "page", Code: "gte", Message: "does not meet the minimum constraint"},
			{Field: "sort", Code: "oneof", Message: "must be one of the allowed values"},
		}, decodeFieldErrors(t, problem))
		assert.NotContains(t, response.Body.String(), "private-secret")
	})

	t.Run("conversion errors are safe invalid requests", func(t *testing.T) {
		response := serve("page=private-secret&sort=name")

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Equal(t, "invalid_request", decodeProblem(t, response).Properties["code"])
		assert.NotContains(t, response.Body.String(), "private-secret")
	})
}

func TestParamErrorMapsBothKnownAndStreamedBodyOverflowTo413(t *testing.T) {
	for _, test := range []struct {
		name          string
		contentLength bool
	}{
		{name: "known content length", contentLength: true},
		{name: "streamed content length", contentLength: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			engine := gin.New()
			engine.Use(Handle(limitRequestBody(32)))
			engine.POST("/requests", Handle(func(_ context.Context, c *Ctx) error {
				var destination bindingRequest
				if err := c.Gin().ShouldBindJSON(&destination); err != nil {
					return ParamError(err, &destination)
				}
				c.Status(http.StatusNoContent)
				return nil
			}))
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
	t.Run("invalid JSON destination", func(t *testing.T) {
		response := serveJSONBindingRequest(`{}`, 1024, nil)

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Equal(t, "internal_server_error", decodeProblem(t, response).Properties["code"])
	})

	t.Run("invalid validator target", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		engine := gin.New()
		engine.GET("/requests", Handle(func(context.Context, *Ctx) error {
			return ParamError(&validator.InvalidValidationError{Type: reflect.TypeOf(0)}, nil)
		}))
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/requests", nil))

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Equal(t, "internal_server_error", decodeProblem(t, response).Properties["code"])
	})

	assert.NoError(t, ParamError(nil, nil))
}
