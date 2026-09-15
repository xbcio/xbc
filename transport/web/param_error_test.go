package web_test

import (
	"context"
	"encoding/json"
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
