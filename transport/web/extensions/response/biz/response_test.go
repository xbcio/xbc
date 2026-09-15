package biz

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
)

type responseItem struct {
	ID string `json:"id"`
}

func TestSuccessWritersUseEnvelopeAndValidatedRequestID(t *testing.T) {
	tests := []struct {
		name       string
		write      func(context.Context, *web.Ctx) error
		wantStatus int
	}{
		{name: "ok", write: func(_ context.Context, c *web.Ctx) error { return OK(c, responseItem{ID: "42"}) }, wantStatus: http.StatusOK},
		{name: "created", write: func(_ context.Context, c *web.Ctx) error { return Created(c, responseItem{ID: "42"}) }, wantStatus: http.StatusCreated},
		{name: "accepted", write: func(_ context.Context, c *web.Ctx) error { return Accepted(c, responseItem{ID: "42"}) }, wantStatus: http.StatusAccepted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestIDs := requestid.New()
			engine := enginetest.New()
			engine.Use(requestIDs.Handler())
			engine.GET("/response", tt.write)

			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/response", nil))

			assert.Equal(t, tt.wantStatus, response.Code)
			assert.Equal(t, "application/json; charset=utf-8", response.Header().Get("Content-Type"))
			var envelope Response[responseItem]
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
			assert.True(t, envelope.Success)
			assert.Equal(t, SuccessCode, envelope.Code)
			assert.Empty(t, envelope.Message)
			assert.Equal(t, "42", envelope.Data.ID)
			assert.NotEmpty(t, envelope.RequestID)
			assert.Equal(t, response.Header().Get("X-Request-ID"), envelope.RequestID)
		})
	}
}

func TestPaginatedNormalizesNilItemsToEmptyArray(t *testing.T) {
	engine := enginetest.New()
	engine.GET("/items", func(_ context.Context, c *web.Ctx) error {
		return OK(c, Paginated[responseItem](nil, 0))
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/items", nil))

	assert.Equal(t, http.StatusOK, response.Code)
	var envelope Response[PaginatedData[responseItem]]
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
	assert.NotNil(t, envelope.Data.List)
	assert.Empty(t, envelope.Data.List)
	assert.EqualValues(t, 0, envelope.Data.Total)
	assert.Contains(t, response.Body.String(), `"list":[]`)
}

func TestEncodingFailureIsMappedBeforeSuccessIsCommitted(t *testing.T) {
	engine := enginetest.New()
	engine.GET("/failure", func(_ context.Context, c *web.Ctx) error {
		return OK(c, make(chan int))
	})

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/failure", nil))

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Contains(t, response.Header().Get("Content-Type"), "application/problem+json")
	assert.NotContains(t, response.Body.String(), "unsupported type")
	assert.NotContains(t, response.Body.String(), `"success":true`)
}

func TestWriteRejectsStatusesThatCannotCarrySuccessEnvelope(t *testing.T) {
	for _, status := range []int{0, http.StatusContinue, http.StatusNoContent, http.StatusResetContent, http.StatusMultipleChoices, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c := enginetest.NewCtx(httptest.NewRecorder(), nil)
			err := Write(c, status, responseItem{})
			require.Error(t, err)
			assert.False(t, c.Writer().Written())
		})
	}
}

func TestWriteRejectsCommittedResponse(t *testing.T) {
	c := enginetest.NewCtx(httptest.NewRecorder(), nil)
	// 写出一个字节才算提交；仅记录状态码不算，Write 之后 Written 必须为真。
	_, err := c.Writer().Write([]byte("committed"))
	require.NoError(t, err)
	require.True(t, c.Writer().Written())
	require.Error(t, OK(c, responseItem{}))
}

func TestPaginatedClampsNegativeTotal(t *testing.T) {
	// total 为负是调用方的 bug，没有可发布的渲染形式，钳到 0 而不是发出去。
	page := Paginated([]responseItem{}, -1)
	assert.EqualValues(t, 0, page.Total)
	assert.NotNil(t, page.List)
}
