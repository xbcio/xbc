package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/transport/web"
	"github.com/xbcio/xbc/transport/web/extensions/observability/requestid"
)

func TestBizErrorDefaultsAndWrapsCause(t *testing.T) {
	cause := errors.New("account balance row 42")
	err := NewError("PAYMENT.INSUFFICIENT_FUNDS", "Insufficient funds.").WithCause(cause)

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
	engine.Use(web.Handle(web.OnError()))
	engine.Use(business.Handler())
	engine.GET("/orders/:id", web.Handle(func(context.Context, *web.Ctx) error {
		return fmt.Errorf("application boundary: %w", NewError(
			"ORDER.ALREADY_PAID",
			"The order has already been paid.",
		).WithStatus(http.StatusConflict).WithCause(errors.New("private database detail")))
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
	engine.Use(web.Handle(web.OnError()))
	engine.Use(New().Handler())
	engine.GET("/orders", web.Handle(func(context.Context, *web.Ctx) error {
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

// mapErrorCtx builds the *web.Ctx the error resolver would hand an ErrorMapper.
// The framework always passes a live Ctx, so these direct-call tests must too;
// a bare gin.Context with no request is enough, and exercises the
// no-request-ID branch of mapError.
func mapErrorCtx() *web.Ctx {
	gc, _ := gin.CreateTestContext(httptest.NewRecorder())
	return web.NewCtx(gc)
}

func TestPluginDeclinesUnrecognizedErrors(t *testing.T) {
	problem, ok := New().mapError(mapErrorCtx(), errors.New("unknown"))
	assert.False(t, ok)
	assert.Equal(t, web.ProblemDetail{}, problem)
}

func TestPluginFailsClosedForInvalidPublicContract(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "success status", err: NewError("FALSE.SUCCESS", "private").WithStatus(http.StatusOK)},
		{name: "redirect status", err: NewError("FALSE.REDIRECT", "private").WithStatus(http.StatusFound)},
		{name: "out of range status", err: NewError("FALSE.STATUS", "private").WithStatus(600)},
		{name: "empty code", err: NewError("", "private").WithStatus(http.StatusConflict)},
		{name: "spaced code", err: NewError("ORDER INVALID", "private").WithStatus(http.StatusConflict)},
		{name: "control code", err: NewError("ORDER\nINVALID", "private").WithStatus(http.StatusConflict)},
		{name: "punctuated code", err: NewError("ORDER/INVALID", "private").WithStatus(http.StatusConflict)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problem, ok := New().mapError(mapErrorCtx(), tt.err)
			require.True(t, ok)
			assert.Equal(t, http.StatusInternalServerError, problem.Status)
			assert.Equal(t, "internal_server_error", problem.Properties["code"])
			assert.Empty(t, problem.Detail)
		})
	}
}

func TestWithStatusAndWithCauseCopyInsteadOfMutating(t *testing.T) {
	template := NewError("ORDER.CLOSED", "The order is closed.")
	cause := errors.New("row 42 locked")

	derived := template.WithStatus(http.StatusConflict).WithCause(cause)

	// 派生值带上了两个维度。
	assert.Equal(t, http.StatusConflict, derived.Status())
	assert.ErrorIs(t, derived, cause)
	assert.Equal(t, "ORDER.CLOSED", derived.Code())
	assert.Equal(t, "The order is closed.", derived.Detail())

	// 模板不被写回，因此可以作为包级变量复用。
	assert.Equal(t, http.StatusUnprocessableEntity, template.Status())
	assert.NoError(t, template.Unwrap())
	assert.NotContains(t, template.Error(), "row 42 locked")

	// 同一模板可以派生出互不干扰的多个值。
	other := template.WithCause(errors.New("row 43 locked"))
	assert.NotErrorIs(t, other, cause)
	assert.Equal(t, http.StatusUnprocessableEntity, other.Status())
}

func TestWithModifiersTolerateNilReceiver(t *testing.T) {
	var absent *Error
	assert.Nil(t, absent.WithStatus(http.StatusConflict))
	assert.Nil(t, absent.WithCause(errors.New("ignored")))
}

func TestWithDetailFillsTemplateWithoutMutatingIt(t *testing.T) {
	template := NewError("QUOTA.EXHAUSTED", "Quota of %d is exhausted.")

	derived := template.WithDetail(50)

	assert.Equal(t, "Quota of 50 is exhausted.", derived.Detail())
	assert.Equal(t, "QUOTA.EXHAUSTED", derived.Code())
	assert.Equal(t, http.StatusUnprocessableEntity, derived.Status())

	// 模板不被写回，同一个包级变量可以反复派生出互不干扰的值。
	assert.Equal(t, "Quota of %d is exhausted.", template.Detail())
	assert.Equal(t, "Quota of 100 is exhausted.", template.WithDetail(100).Detail())

	// 与另外两个维度可以任意组合，且顺序无关。
	combined := template.WithDetail(7).WithStatus(http.StatusConflict)
	assert.Equal(t, "Quota of 7 is exhausted.", combined.Detail())
	assert.Equal(t, http.StatusConflict, combined.Status())
}

func TestPluginLogsCauseOfNon5xxBusinessFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &captureLogger{}
	business := &Plugin{logger: logger}
	engine := gin.New()
	engine.Use(web.Handle(web.OnError()))
	engine.Use(business.Handler())
	engine.GET("/orders", web.Handle(func(context.Context, *web.Ctx) error {
		return NewError("ORDER.ALREADY_PAID", "The order has already been paid.").
			WithStatus(http.StatusConflict).
			WithCause(errors.New("row 42 locked by txn 7"))
	}))

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/orders", nil))

	// 409 不会走 Web 的 5xx 日志路径，cause 必须由 biz 自己记下来，
	// 否则它既不进响应体也不进日志，等于凭空消失。
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.NotContains(t, response.Body.String(), "row 42 locked by txn 7")

	entries := logger.errorEntries()
	require.Len(t, entries, 1)
	assert.Equal(t, "business failure", entries[0].msg)
	assert.Contains(t, fmt.Sprint(entries[0].fields...), "row 42 locked by txn 7")
	assert.Contains(t, fmt.Sprint(entries[0].fields...), "ORDER.ALREADY_PAID")
}

func TestPluginDoesNotLogBusinessFailureWithoutCause(t *testing.T) {
	logger := &captureLogger{}
	business := &Plugin{logger: logger}

	problem, ok := business.mapError(mapErrorCtx(), NewError("ORDER.CLOSED", "The order is closed."))

	require.True(t, ok)
	assert.Equal(t, http.StatusUnprocessableEntity, problem.Status)
	assert.Empty(t, logger.errorEntries(), "没有 cause 就没有可诊断的内容，不该产生日志噪声")
}

type captureLogger struct {
	mu      sync.Mutex
	entries []captureEntry
}

type captureEntry struct {
	level  string
	msg    string
	fields []any
}

func (l *captureLogger) add(level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, captureEntry{level: level, msg: msg, fields: append([]any(nil), fields...)})
}

func (l *captureLogger) errorEntries() []captureEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var errorEntries []captureEntry
	for _, entry := range l.entries {
		if entry.level == "error" {
			errorEntries = append(errorEntries, entry)
		}
	}
	return errorEntries
}

func (l *captureLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) corelog.Logger  { return l }
func (*captureLogger) Enabled(corelog.Level) bool    { return true }

func decodeBizProblem(t *testing.T, response *httptest.ResponseRecorder) web.ProblemDetail {
	t.Helper()
	var problem web.ProblemDetail
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &problem))
	return problem
}
