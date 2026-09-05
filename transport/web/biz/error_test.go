package biz

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/requestid"
)

func TestBizErrorDefaultsAndWrapsCause(t *testing.T) {
	cause := errors.New("account balance row 42")
	err := WrapError(cause, "PAYMENT.INSUFFICIENT_FUNDS", "Insufficient funds.")

	assert.Equal(t, http.StatusUnprocessableEntity, err.Status())
	assert.Equal(t, "PAYMENT.INSUFFICIENT_FUNDS", err.Code())
	assert.Equal(t, "Insufficient funds.", err.Detail())
	assert.ErrorIs(t, err, cause)
	assert.Contains(t, err.Error(), "account balance row 42", "internal error chains retain the cause for diagnostics")

	plain := NewError("ORDER.CLOSED", "The order is closed.")
	assert.Equal(t, http.StatusUnprocessableEntity, plain.Status())
	assert.Equal(t, "The order is closed.", plain.Error())
}

func TestPluginMapsWrappedBizErrorAndPropagatesRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	requestIDs := requestid.New()
	business := New()
	engine := gin.New()
	engine.Use(requestIDs.Handler())
	engine.Use(web.OnError())
	engine.Use(business.Handler())
	engine.GET("/orders/:id", web.Handle(func(c *gin.Context) error {
		return fmt.Errorf("application boundary: %w", WrapStatusError(
			errors.New("private database detail"),
			http.StatusConflict,
			"ORDER.ALREADY_PAID",
			"The order has already been paid.",
		))
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/orders/42", nil))

	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Contains(t, response.Header().Get("Content-Type"), "application/problem+json")
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	assert.Equal(t, "ORDER.ALREADY_PAID", problem.Properties["code"])
	assert.Equal(t, "The order has already been paid.", problem.Detail)
	assert.NotEmpty(t, problem.Properties["requestId"])
	assert.Equal(t, response.Header().Get("X-Request-ID"), problem.Properties["requestId"])
	assert.NotContains(t, response.Body.String(), "private database detail")
	assert.NotContains(t, response.Body.String(), "application boundary")
}

func TestPluginOnErrorKeepsWebSafeFallbackForUnknownErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(web.OnError())
	engine.Use(New().Handler())
	engine.GET("/orders", web.Handle(func(*gin.Context) error {
		return errors.New("postgres://admin:private-password@database/orders")
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/orders", nil))

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	problem := decodeBizProblem(t, response)
	assert.Equal(t, "internal_server_error", problem.Properties["code"])
	assert.Empty(t, problem.Detail)
	assert.NotContains(t, response.Body.String(), "private-password")
}

func TestPluginDeclinesUnrecognizedErrors(t *testing.T) {
	problem, ok := New().mapError(nil, errors.New("unknown"))
	assert.False(t, ok)
	assert.Equal(t, web.ProblemDetail{}, problem)
}

func TestPluginFailsClosedForInvalidPublicContract(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "success status", err: NewStatusError(http.StatusOK, "FALSE.SUCCESS", "private")},
		{name: "redirect status", err: NewStatusError(http.StatusFound, "FALSE.REDIRECT", "private")},
		{name: "out of range status", err: NewStatusError(600, "FALSE.STATUS", "private")},
		{name: "empty code", err: NewStatusError(http.StatusConflict, "", "private")},
		{name: "spaced code", err: NewStatusError(http.StatusConflict, "ORDER INVALID", "private")},
		{name: "control code", err: NewStatusError(http.StatusConflict, "ORDER\nINVALID", "private")},
		{name: "punctuated code", err: NewStatusError(http.StatusConflict, "ORDER/INVALID", "private")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problem, ok := New().mapError(nil, tt.err)
			require.True(t, ok)
			assert.Equal(t, http.StatusInternalServerError, problem.Status)
			assert.Equal(t, "internal_server_error", problem.Properties["code"])
			assert.Empty(t, problem.Detail)
		})
	}
}

func decodeBizProblem(t *testing.T, response *httptest.ResponseRecorder) web.ProblemDetail {
	t.Helper()
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	return problem
}
