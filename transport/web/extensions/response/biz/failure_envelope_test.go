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

func enabledPlugin() *Plugin { return NewWithConfig(Config{FailureEnvelope: true}) }

func codedProblem(status int, code string) web.ProblemDetail {
	return web.NewProblem(status, code)
}

func renderEnvelope(t *testing.T, plugin *Plugin, c *web.Ctx, problem web.ProblemDetail) map[string]any {
	t.Helper()
	body, ok := plugin.RenderError(c, problem)
	require.True(t, ok, "the failure must be rendered")
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(body, &envelope), "body: %s", body)
	return envelope
}

func TestFailureEnvelopeIsOffByDefault(t *testing.T) {
	assert.False(t, DefaultConfig().FailureEnvelope)
	assert.False(t, New().failureEnvelope)
	assert.True(t, enabledPlugin().failureEnvelope)
}

func TestRenderErrorDeclinesEverythingWhenTheEnvelopeIsOff(t *testing.T) {
	problem := codedProblem(http.StatusUnprocessableEntity, "ORDER.CLOSED")
	for name, plugin := range map[string]*Plugin{
		"default":  New(),
		"explicit": NewWithConfig(Config{FailureEnvelope: false}),
		"nil":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			body, ok := plugin.RenderError(nil, problem)
			assert.False(t, ok)
			assert.Nil(t, body)
		})
	}
}

func TestRenderErrorDeclinesAProblemWithoutAValidBusinessCode(t *testing.T) {
	plugin := enabledPlugin()
	withProperty := func(value any) web.ProblemDetail {
		problem := codedProblem(http.StatusConflict, "")
		problem.Properties["code"] = value
		return problem
	}
	for name, problem := range map[string]web.ProblemDetail{
		"missing":          codedProblem(http.StatusConflict, ""),
		"nil properties":   {Status: http.StatusConflict},
		"empty":            withProperty(""),
		"not a string":     withProperty(42),
		"illegal rune":     withProperty("bad code!"),
		"too long":         withProperty(string(make([]byte, 129))),
		"unicode":          withProperty("订单.已关闭"),
		"path-like":        withProperty("a/b"),
		"whitespace":       withProperty(" ORDER.CLOSED"),
		"newline injected": withProperty("ORDER\nCLOSED"),
	} {
		t.Run(name, func(t *testing.T) {
			body, ok := plugin.RenderError(nil, problem)
			assert.False(t, ok, "a code-less or invalid-code problem must stay a Problem Detail")
			assert.Nil(t, body)
		})
	}
}

func TestRenderErrorMapsTheProblemOntoTheEnvelope(t *testing.T) {
	plugin := enabledPlugin()

	t.Run("detail becomes the message", func(t *testing.T) {
		problem := codedProblem(http.StatusUnprocessableEntity, "ORDER.CLOSED")
		problem.Title, problem.Detail = "Unprocessable Entity", "The order is closed."

		envelope := renderEnvelope(t, plugin, nil, problem)

		assert.Equal(t, map[string]any{
			"success": false, "code": "ORDER.CLOSED", "message": "The order is closed.", "data": nil,
		}, envelope)
	})

	t.Run("title is the message when there is no detail", func(t *testing.T) {
		problem := codedProblem(http.StatusNotFound, "not_found")
		problem.Title = "Not Found"

		assert.Equal(t, "Not Found", renderEnvelope(t, plugin, nil, problem)["message"])
	})

	t.Run("errors become data.errors verbatim", func(t *testing.T) {
		problem := codedProblem(http.StatusBadRequest, "validation_failed")
		problem.Detail = "Request validation failed."
		problem.Properties["errors"] = []web.FieldError{
			{Field: "email", Code: "email", Message: "must be an email"},
			{Field: "name", Code: "required", Message: "is required"},
		}

		envelope := renderEnvelope(t, plugin, nil, problem)

		assert.Equal(t, map[string]any{"errors": []any{
			map[string]any{"field": "email", "code": "email", "message": "must be an email"},
			map[string]any{"field": "name", "code": "required", "message": "is required"},
		}}, envelope["data"])
	})

	t.Run("data is null without errors", func(t *testing.T) {
		envelope := renderEnvelope(t, plugin, nil, codedProblem(http.StatusConflict, "ORDER.CLOSED"))

		assert.Contains(t, envelope, "data")
		assert.Nil(t, envelope["data"])
	})

	t.Run("other extension members are not copied", func(t *testing.T) {
		problem := codedProblem(http.StatusConflict, "ORDER.CLOSED")
		problem.Detail = "x"
		problem.Properties["requestId"] = "from-the-problem"
		problem.Properties["secret"] = "internal"
		problem.Properties["retryAfter"] = 30
		problem.Type, problem.Instance = "https://example.test/type", "/orders/1"

		envelope := renderEnvelope(t, plugin, nil, problem)

		assert.Equal(t, map[string]any{
			"success": false, "code": "ORDER.CLOSED", "message": "x", "data": nil,
		}, envelope, "only the whitelisted members may reach the client")
	})
}

// TestRenderErrorEchoesTheRequestIDFromTheSameSourceAsSuccess drives RenderError
// through a real request so the id comes from requestid.From, the source
// Write uses, rather than from anything the problem carries.
func TestRenderErrorEchoesTheRequestIDFromTheSameSourceAsSuccess(t *testing.T) {
	plugin := enabledPlugin()
	for _, test := range []struct {
		name       string
		middleware bool
	}{{"with the middleware", true}, {"without the middleware", false}} {
		t.Run(test.name, func(t *testing.T) {
			engine := enginetest.New()
			if test.middleware {
				engine.Use(requestid.New().Handler())
			}
			engine.GET("/fail", func(_ context.Context, c *web.Ctx) error {
				body, ok := plugin.RenderError(c, codedProblem(http.StatusConflict, "ORDER.CLOSED"))
				require.True(t, ok)
				c.Status(http.StatusConflict)
				_, err := c.Writer().Write(body)
				return err
			})

			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/fail", nil))

			var envelope map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &envelope))
			if !test.middleware {
				assert.NotContains(t, envelope, "requestId", "no id means no field, not an empty one")
				return
			}
			assert.NotEmpty(t, response.Header().Get("X-Request-ID"))
			assert.Equal(t, response.Header().Get("X-Request-ID"), envelope["requestId"])
		})
	}
}

// TestMappedBusinessErrorRendersAsTheEnvelope joins the two halves biz owns:
// mapError produces the problem and RenderError turns it into the envelope, so
// a drift in either one's idea of the code or the detail shows up here.
func TestMappedBusinessErrorRendersAsTheEnvelope(t *testing.T) {
	plugin := enabledPlugin()
	problem, ok := plugin.mapError(nil, NewError("ORDER.ALREADY_PAID", "The order is already paid.").WithStatus(http.StatusConflict))
	require.True(t, ok)

	envelope := renderEnvelope(t, plugin, nil, problem)

	assert.Equal(t, "ORDER.ALREADY_PAID", envelope["code"])
	assert.Equal(t, "The order is already paid.", envelope["message"])
	assert.Equal(t, false, envelope["success"])
}
