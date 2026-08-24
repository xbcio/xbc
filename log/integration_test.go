package log

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Hard-constraint automated gate: the log package must not depend on the
// framework internals, not even by one line.
func TestLogPackageHasNoFrameworkDependency(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command unavailable, skipping dependency direction check")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/xbcio/xbc/log").Output()
	require.NoError(t, err)

	for _, line := range strings.Split(string(out), "\n") {
		p := strings.TrimSpace(line)
		if p == "" || p == "github.com/xbcio/xbc/log" {
			continue
		}
		assert.False(t, strings.HasPrefix(p, "github.com/xbcio/xbc"),
			"log 包必须零框架依赖，但依赖了 %s", p)
	}
}

// Two services are strung into the same trace via headers.
func TestEndToEndTwoServiceTracePropagation(t *testing.T) {
	logs := installObserver(t)

	// Service A: no upstream -> new trace -> log one line -> inject outbound header
	ctxA := Extract(context.Background(), propagation.HeaderCarrier(http.Header{}), "POST /orders")
	TInfo(ctxA, "创建订单", "order_id", 1001)

	outbound := http.Header{}
	Inject(ctxA, propagation.HeaderCarrier(outbound))

	// Service B: restore the trace from the inbound header -> log one line
	ctxB := Extract(context.Background(), propagation.HeaderCarrier(outbound), "POST /payments")
	TInfo(ctxB, "发起支付", "amount", 99.5)

	require.Len(t, logs.All(), 2)
	a, b := logs.All()[0].ContextMap(), logs.All()[1].ContextMap()

	assert.Equal(t, a["trace_id"], b["trace_id"], "两个服务的日志用同一个 trace_id 串起来")
	assert.Equal(t, a["request_id"], b["request_id"])
	assert.NotEqual(t, a["span_id"], b["span_id"])
	assert.Equal(t, "POST /orders", a["span_name"])
	assert.Equal(t, "POST /payments", b["span_name"])
}

// The same set of KV pairs, two sinks, two renderings -- masking must take
// effect in both.
func TestEndToEndMaskingCoversBothRenderings(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")        // extension implies -> console
	cfg.File.ErrorPath = filepath.Join(dir, "err.jsonl") // extension implies -> json
	cfg.MaskFields = []string{"salary"}
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Error("入职", "password", "hunter2", "salary", 50000, "name", "alice")
	require.NoError(t, Close())

	human, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Contains(t, string(human), "password=***")
	assert.Contains(t, string(human), "salary=***", "配置追加的字段同样生效")
	assert.Contains(t, string(human), "name=alice")
	assert.NotContains(t, string(human), "hunter2")
	assert.NotContains(t, string(human), "50000")

	machine, err := os.ReadFile(cfg.File.ErrorPath)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(machine), &m))
	assert.Equal(t, maskPlaceholder, m["password"])
	assert.Equal(t, maskPlaceholder, m["salary"])
	assert.Equal(t, "alice", m["name"])
}

// The escape hatch cannot bypass masking -- maskCore is part of the logger's
// composition.
func TestEndToEndEscapeHatchIsAlsoMasked(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	SetLogger(newZapLogger(zap.New(newMaskCore(core, newMasker(nil)))))
	t.Cleanup(func() { SetLogger(Nop()) })

	z, ok := Zap(context.Background())
	require.True(t, ok)
	z.Info("直接用 zap", zap.String("token", "abc.def"), zap.String("user", "bob"))

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, maskPlaceholder, m["token"])
	assert.Equal(t, "bob", m["user"])
}

// The complete shape of one request, from entry to a nested span.
func TestEndToEndRequestLifecycle(t *testing.T) {
	logs := installObserver(t)

	inbound := http.Header{}
	inbound.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	ctx := Extract(context.Background(), propagation.HeaderCarrier(inbound), "GET /orders/:id")
	TInfo(ctx, "请求进入")

	dbCtx, dbDone := Span(ctx, "db.query")
	TInfo(dbCtx, "查询订单", "order_id", 1001)
	dbDone()

	TInfo(ctx, "请求完成", "status", 200)

	entries := logs.All()
	require.Len(t, entries, 4) // entry, query, span end, completion

	const wantTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	for i, e := range entries {
		assert.Equal(t, wantTrace, e.ContextMap()["trace_id"], "第 %d 条", i)
	}

	reqSpan := entries[0].ContextMap()["span_id"]
	dbSpan := entries[1].ContextMap()["span_id"]
	assert.NotEqual(t, reqSpan, dbSpan, "子 span 有独立的 span_id")
	assert.Equal(t, reqSpan, entries[3].ContextMap()["span_id"], "回到父 span")
	assert.Equal(t, "span 结束", entries[2].Message)
}

// Re-Init must not leak the previous round's file handles, nor keep writing
// logs into the old file.
func TestReInitClosesPreviousSinks(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.jsonl")
	second := filepath.Join(dir, "second.jsonl")

	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = first
	require.NoError(t, Init(cfg))
	L().Info("第一轮")

	cfg.File.Path = second
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })
	L().Info("第二轮")
	require.NoError(t, Close())

	a, err := os.ReadFile(first)
	require.NoError(t, err)
	assert.Contains(t, string(a), "第一轮")
	assert.NotContains(t, string(a), "第二轮", "换配置后不该继续写旧文件")

	b, err := os.ReadFile(second)
	require.NoError(t, err)
	assert.Contains(t, string(b), "第二轮")
}
