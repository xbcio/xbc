package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/enginetest"
)

// TestCapRequestBodyBoundsTheReplayOfAReplayableBody pins the upper half of
// the body cap: a request whose GetBody can produce its body again still has
// that replay bounded when the cap is installed, because a replayed read that
// escaped the maximum would hand out exactly the unbounded body the stage
// exists to deny. The replay goes through the request's own GetBody, the way
// a retrying outbound client reads it, rather than through a wrapper the test
// installed itself.
func TestCapRequestBodyBoundsTheReplayOfAReplayableBody(t *testing.T) {
	for _, test := range []struct {
		name          string
		original      string
		replay        string
		wantStatus    int
		wantOriginal  string
		wantReplay    string
		wantOversized bool
	}{
		{
			name:         "both reads fit the cap",
			original:     `{"name":"Alice"}`,
			replay:       `{"name":"Alice"}`,
			wantStatus:   http.StatusOK,
			wantOriginal: `{"name":"Alice"}`,
			wantReplay:   `{"name":"Alice"}`,
		},
		{
			name:          "the replay exceeds the cap the original fit",
			original:      `{"name":"Alice"}`,
			replay:        `{"name":"Alice","email":"alice@example.com","address":{"city":"Shanghai"}}`,
			wantStatus:    http.StatusRequestEntityTooLarge,
			wantOriginal:  `{"name":"Alice"}`,
			wantOversized: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var (
				originalRead string
				replayRead   string
				replayErr    error
			)
			engine := enginetest.New()
			engine.Use(web.CapRequestBody(32))
			engine.POST("/requests", func(_ context.Context, c *web.Ctx) error {
				original, err := io.ReadAll(c.Request().Body)
				if err != nil {
					return web.ParamError(err, nil)
				}
				originalRead = string(original)
				replay, err := c.Request().GetBody()
				if err != nil {
					return web.ParamError(err, nil)
				}
				defer replay.Close()
				replayed, err := io.ReadAll(replay)
				replayErr = err
				replayRead = string(replayed)
				if err != nil {
					return web.ParamError(err, nil)
				}
				return nil
			})

			request := httptest.NewRequest(http.MethodPost, "/requests", strings.NewReader(test.original))
			request.Header.Set("Content-Type", "application/json")
			// httptest.NewRequest records no replay facility, so the test
			// installs the one this case is about: the middleware stacks ahead
			// of a handler set GetBody the same way, and the cap must reach it.
			replaySource := test.replay
			request.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(replaySource)), nil
			}
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)

			assert.Equal(t, test.wantStatus, response.Code)
			assert.Equal(t, test.wantOriginal, originalRead, "the first read must yield the body as sent")
			if !test.wantOversized {
				require.NoError(t, replayErr)
				assert.Equal(t, test.wantReplay, replayRead)
				return
			}
			var maxBytesError *http.MaxBytesError
			require.True(t, errors.As(replayErr, &maxBytesError),
				"the oversized replay must fail with net/http's own MaxBytesError, got %v", replayErr)
			assert.Equal(t, int64(32), maxBytesError.Limit)
		})
	}
}

// TestCapRequestBodyKeepsABodylessRequestBodyless pins the empty-body path: a
// request that arrived with no content still reads as empty after the cap, and
// reading it produces no overflow -- the cap is a boundary, not a payload.
func TestCapRequestBodyKeepsABodylessRequestBodyless(t *testing.T) {
	engine := enginetest.New()
	engine.Use(web.CapRequestBody(32))
	engine.GET("/requests", func(_ context.Context, c *web.Ctx) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return web.ParamError(err, nil)
		}
		assert.Empty(t, body)
		return nil
	})
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/requests", nil))
	assert.Equal(t, http.StatusOK, response.Code)
}
