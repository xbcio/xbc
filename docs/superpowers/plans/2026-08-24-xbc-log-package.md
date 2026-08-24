# xbc `log` 子包实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 `github.com/xbcio/xbc/log` —— 一个零框架依赖、可脱离 xbc 单独使用的结构化日志包：SLF4J 式门面接口 + zap 默认 binding + console/json 双渲染 + 强制脱敏 + OTel 兼容的链路模型。

**Architecture:** 门面（`Logger` 接口，KV 变参）与 binding（zap 实现）分离，业务调用点不 import zap；可选能力接口（`ZapProvider` / `CallerSkipper`）让第三方 binding 按需实现，不实现也能工作。KV 是数据，console 与 json 是同一份数据的两种渲染，每个 sink 各自决定格式。脱敏包在 `zapcore.Core` 层，**每个叶子 sink 各包一层，位于 Tee 之内**（包在 Tee 之外会让 `Check` 挂上自己、绕掉 Tee 的 per-sink 级别过滤；采样器才包在 Tee 之外），覆盖全部 sink 与 `log.Zap()` 逃生舱口。链路模型内嵌 `trace.SpanContext`，与 OTel SDK 天然互操作。

**Tech Stack:** Go 1.25+ / zap v1.28.0 / lumberjack.v2 v2.2.1 / OpenTelemetry API v1.45.0 / oklog/ulid v2.1.2 / mattn/go-isatty v0.0.24 / testify v1.12.1

**Spec:** [docs/superpowers/specs/2026-08-23-xbc-plugin-framework-design.md](../specs/2026-08-23-xbc-plugin-framework-design.md) —— 本计划实现其 §8（日志与链路追踪）、§9（零依赖子包中的 `log`）、§13 第 0 步。

---

## Global Constraints

以下约束对**每一个** task 隐式生效，不再逐条重复：

- **module**：`github.com/xbcio/xbc`，单仓单 module。
- **go 指令**：`go 1.25.0`。这是依赖里最高的下限（`go.opentelemetry.io/otel/trace@v1.45.0` 要求 1.25.0），不是随意抬高。
- **依赖版本锁死**：zap `v1.28.0`、lumberjack.v2 `v2.2.1`、otel `v1.45.0`、otel/trace `v1.45.0`、ulid/v2 `v2.1.2`、go-isatty `v0.0.24`、testify `v1.12.1`。不引入计划外的第三方依赖。
- **零框架依赖（硬约束）**：`log/` 下任何文件**一行都不能 import `github.com/xbcio/xbc` 根包或任何 `xbc/internal/*`**。依赖方向永远是内核 → log，绝不反向。Task 10 有一条自动化测试守这条线。
- **内置脱敏黑名单不可关闭**：`log.mask_fields` 只能**追加**，不能移除内置项。任何允许移除的 API 都是安全缺陷。
- **测试包名**：全部用 `package log`（内部测试），因为要断言未导出的 `masker`、`consoleEncoder`、`nowFunc` 等。
- **注释与文档语言**：**代码注释一律英文**（用户 2026-08-24 指令，覆盖原「注释中文」规则）；
  README、错误信息、测试断言 message、测试数据字符串一律中文；标识符、tag、配置 key 保持英文。
  判断边界：**引号里的中文不动**，注释（`//` 与 `/* */` 之后）才转英文。
- **提交粒度**：每个 task 的每个 "Commit" 步骤都真的提交一次，不攒批。

---

## File Structure

| 文件 | 职责 | Task |
|---|---|---|
| `go.mod` / `go.sum` | module 声明与依赖锁 | 1 |
| `log/logger.go` | `Logger` 门面接口、`Level`、`ZapProvider`、`CallerSkipper`、`Nop()` | 1 |
| `log/trace.go` | `Trace` 数据模型、`Fork`、ctx 存取；后半段的 `Span` / `SpanOption` | 2, 8 |
| `log/config.go` | `Config` 全族、默认值、后缀推导 format、枚举校验 | 3 |
| `log/mask.go` | `masker` + `maskCore`：Core 层强制脱敏 | 4 |
| `log/console.go` | `consoleEncoder`：对齐、着色、TTY 探测、KV 渲染 | 5 |
| `log/rotate.go` | `dailyRotator`：给 lumberjack 补日期滚动 | 6 |
| `log/zap.go` | 默认 binding：`Init` / `SetLogger` / `L` / `Ctx` / `Sync` / `Zap` / KV→Field | 7 |
| `log/sugar.go` | `TInfo` 等 8 个包级语法糖 | 9 |
| `log/propagate.go` | `Extract` / `Inject`：W3C traceparent 跨服务传播 | 10 |
| `log/README.md` | 用法、配置表、换后端、从 gfa 迁移的陷阱 | 10 |

拆分依据：门面（`logger.go`）与 binding（`zap.go`）必须分开，否则"零 zap 依赖"只是口号；`mask.go` 与 `console.go` 各自是独立可测的 zapcore 组件；`trace.go` 是纯数据 + 一层薄封装，不该被塞进 binding。

---

### Task 1: module 初始化 + `Logger` 门面接口 + `Level`

**Files:**
- Create: `go.mod`
- Create: `log/logger.go`
- Test: `log/logger_test.go`

**Interfaces:**
- Consumes: 无（首个 task）
- Produces:
  - `type Level int8`，常量 `DebugLevel = -1`、`InfoLevel = 0`、`WarnLevel = 1`、`ErrorLevel = 2`（数值与 `zapcore.Level` 对齐，可直接 `zapcore.Level(lv)` 转换）
  - `func (l Level) String() string` → `"DEBUG"` / `"INFO"` / `"WARN"` / `"ERROR"`
  - `func ParseLevel(s string) (Level, error)`
  - `type Logger interface { Debug/Info/Warn/Error(msg string, kv ...any); With(kv ...any) Logger; Enabled(lv Level) bool }`
  - `type ZapProvider interface{ Zap() *zap.Logger }`
  - `type CallerSkipper interface{ WithCallerSkip(n int) Logger }`
  - `func Nop() Logger`

- [ ] **Step 1: 初始化 module 并拉依赖**

```bash
cd /Users/10097292/Desktop/caffe/xbcio/xbc
go mod init github.com/xbcio/xbc
go get go.uber.org/zap@v1.28.0
go get go.opentelemetry.io/otel@v1.45.0
go get go.opentelemetry.io/otel/trace@v1.45.0
go get github.com/oklog/ulid/v2@v2.1.2
go get gopkg.in/natefinch/lumberjack.v2@v2.2.1
go get github.com/mattn/go-isatty@v0.0.24
go get github.com/stretchr/testify@v1.12.1
```

确认 `go.mod` 首行是 `module github.com/xbcio/xbc`，`go` 指令是 `go 1.25.0`（若 `go mod init` 写成了更高的补丁版本，手工改成 `1.25.0`）。

- [ ] **Step 2: 写失败测试**

创建 `log/logger_test.go`：

```go
package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]Level{
		"debug": DebugLevel, "DEBUG": DebugLevel, " Debug ": DebugLevel,
		"info": InfoLevel, "INFO": InfoLevel,
		"warn": WarnLevel, "warning": WarnLevel, "WARN": WarnLevel,
		"error": ErrorLevel, "ERROR": ErrorLevel,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		require.NoError(t, err, "输入 %q", in)
		assert.Equal(t, want, got, "输入 %q", in)
	}

	_, err := ParseLevel("verbose")
	assert.Error(t, err, "未知级别必须报错，不能静默降级到 info")
}

func TestLevelString(t *testing.T) {
	assert.Equal(t, "DEBUG", DebugLevel.String())
	assert.Equal(t, "INFO", InfoLevel.String())
	assert.Equal(t, "WARN", WarnLevel.String())
	assert.Equal(t, "ERROR", ErrorLevel.String())
}

// Level's numeric values must align with zapcore's so bindings can convert
// between them directly. This is an implicit contract; pin it down with a test.
func TestLevelNumericallyMatchesZapcore(t *testing.T) {
	assert.EqualValues(t, zapcore.DebugLevel, DebugLevel)
	assert.EqualValues(t, zapcore.InfoLevel, InfoLevel)
	assert.EqualValues(t, zapcore.WarnLevel, WarnLevel)
	assert.EqualValues(t, zapcore.ErrorLevel, ErrorLevel)
}

func TestNopLoggerSatisfiesFacadeAndNeverPanics(t *testing.T) {
	var l Logger = Nop()
	assert.NotPanics(t, func() {
		l.Debug("d", "k", 1)
		l.Info("i")
		l.Warn("w", "k")
		l.Error("e", "k", nil)
		l.With("a", 1).Info("chained")
	})
	assert.False(t, l.Enabled(ErrorLevel), "Nop 对所有级别都不启用")
}
```

- [ ] **Step 3: 跑测试确认失败**

```bash
go test ./log/ -run 'TestParseLevel|TestLevel|TestNop' -v
```

Expected: 编译失败，`undefined: Level` / `undefined: Nop`。

- [ ] **Step 4: 写实现**

创建 `log/logger.go`：

```go
// Package log is xbc's logging facade.
//
// It follows the SLF4J approach: business code and plugins depend only on
// this package's Logger interface. The concrete backend is assembled by
// Init (zap by default) or swapped via SetLogger. This package has zero
// framework dependencies and can be used standalone, outside of xbc.
package log

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Level is the log level.
// The numeric values are deliberately aligned with zapcore.Level (Debug=-1),
// so bindings can convert between them directly.
type Level int8

const (
	DebugLevel Level = iota - 1
	InfoLevel
	WarnLevel
	ErrorLevel
)

func (l Level) String() string {
	switch l {
	case DebugLevel:
		return "DEBUG"
	case InfoLevel:
		return "INFO"
	case WarnLevel:
		return "WARN"
	case ErrorLevel:
		return "ERROR"
	default:
		return fmt.Sprintf("LEVEL(%d)", int8(l))
	}
}

// ParseLevel parses a level name. Unknown levels return an error instead of
// silently downgrading, so a typo in config doesn't quietly drop logs in production.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return DebugLevel, nil
	case "info", "":
		return InfoLevel, nil
	case "warn", "warning":
		return WarnLevel, nil
	case "error":
		return ErrorLevel, nil
	default:
		return InfoLevel, fmt.Errorf("log: 未知日志级别 %q，可选 debug/info/warn/error", s)
	}
}

// Logger is the facade interface. The variadic args are a KV sequence: key1, val1, key2, val2, ...
// A key must be a string; a non-string key or a dangling trailing argument is
// filed under "!BADKEY" -- it never panics and is never silently dropped.
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)

	// With derives a child Logger with fixed fields attached.
	With(kv ...any) Logger

	// Enabled reports whether this level would actually be emitted, so callers can
	// short-circuit expensive field construction.
	Enabled(lv Level) bool
}

// ZapProvider is an optional capability interface. A backend built on zap should
// implement it so callers can use log.Zap(ctx) to get a strongly-typed entry point
// for zap-specific operations. When switching to a non-zap backend, simply don't
// implement it -- log.Zap will return ok=false.
type ZapProvider interface {
	Zap() *zap.Logger
}

// CallerSkipper is an optional capability interface. The package-level sugar
// functions (TInfo etc.) add one extra stack frame compared to the facade
// methods; this interface lets them point the caller back at the business code
// instead of into the log package internals. Third-party bindings work fine
// without implementing it, at the cost of the caller pointing into sugar.go.
type CallerSkipper interface {
	WithCallerSkip(n int) Logger
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}
func (nopLogger) With(...any) Logger   { return nopLogger{} }
func (nopLogger) Enabled(Level) bool   { return false }

// Nop returns a Logger that discards everything.
// Used in tests, and as the fallback for L() before Init -- logging before
// initialization must not panic.
func Nop() Logger { return nopLogger{} }
```

- [ ] **Step 5: 跑测试确认通过**

```bash
go test ./log/ -v
```

Expected: 4 个测试全 PASS。

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum log/logger.go log/logger_test.go
git commit -m "feat(log): 门面接口与 Level，数值与 zapcore 对齐"
```

---

### Task 2: `Trace` 链路数据模型

**Files:**
- Create: `log/trace.go`
- Test: `log/trace_test.go`

**Interfaces:**
- Consumes: 无（纯数据模型，不依赖 Task 1 的产物）
- Produces:
  - `type Trace struct { trace.SpanContext; ParentSpanID trace.SpanID; SpanName string; RequestID string }`
  - `func NewTrace(name string) Trace`
  - `func TraceFrom(ctx context.Context) Trace`（取不到返回零值，不 panic）
  - `func WithTrace(ctx context.Context, t Trace) context.Context`
  - `func (t Trace) Fork(name string) Trace`
  - `func (t Trace) Valid() bool`
  - `func newSpanID() trace.SpanID`（包内可见，Task 7、10 会用）

**设计要点：`RequestID` 与 `TraceID` 是同一个 128 bit 值的两种编码。** ULID 是 16 字节，W3C trace_id 也是 16 字节。`RequestID` 是它的 Crockford Base32（26 字符、带时间戳前缀、人可读），`TraceID` 是它的 hex（32 字符、W3C 标准）。不是两个独立 ID，可无损互转 —— Task 10 的 `Extract` 就靠这一点从上游 trace_id 反推 request_id。

- [ ] **Step 1: 写失败测试**

创建 `log/trace_test.go`：

```go
package log

import (
	"context"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

func TestNewTraceIsValidAndSampled(t *testing.T) {
	tr := NewTrace("http.request")
	assert.True(t, tr.Valid())
	assert.True(t, tr.TraceID().IsValid())
	assert.True(t, tr.SpanID().IsValid())
	assert.True(t, tr.IsSampled())
	assert.Equal(t, "http.request", tr.SpanName)
	assert.False(t, tr.ParentSpanID.IsValid(), "根 span 没有 parent")
}

// RequestID and TraceID must be the same value in two encodings; Extract's derivation relies on this.
func TestRequestIDAndTraceIDAreSameValue(t *testing.T) {
	tr := NewTrace("root")

	u, err := ulid.Parse(tr.RequestID)
	require.NoError(t, err, "RequestID 必须是合法 ULID")
	assert.Equal(t, trace.TraceID(u), tr.TraceID())
	assert.Equal(t, tr.RequestID, ulid.ULID(tr.TraceID()).String(), "反推必须无损")
}

func TestNewTraceIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewTrace("x").TraceID().String()
		_, dup := seen[id]
		require.False(t, dup, "第 %d 次生成了重复 trace_id", i)
		seen[id] = struct{}{}
	}
}

func TestForkInheritsTraceIDAndChainsParent(t *testing.T) {
	root := NewTrace("http.request")
	child := root.Fork("db.query")

	assert.Equal(t, root.TraceID(), child.TraceID(), "trace_id 必须继承")
	assert.NotEqual(t, root.SpanID(), child.SpanID(), "span_id 必须是新的")
	assert.Equal(t, root.SpanID(), child.ParentSpanID, "parent 必须指向 fork 的源")
	assert.Equal(t, "db.query", child.SpanName)
	assert.Equal(t, root.RequestID, child.RequestID, "request_id 贯穿整条链路")

	grand := child.Fork("redis.get")
	assert.Equal(t, root.TraceID(), grand.TraceID())
	assert.Equal(t, child.SpanID(), grand.ParentSpanID)
}

func TestForkDoesNotMutateParent(t *testing.T) {
	root := NewTrace("root")
	rootSpan := root.SpanID()
	_ = root.Fork("child")
	assert.Equal(t, rootSpan, root.SpanID(), "Fork 是值语义，不能改到调用者")
}

func TestTraceContextRoundTrip(t *testing.T) {
	want := NewTrace("svc")
	ctx := WithTrace(context.Background(), want)
	got := TraceFrom(ctx)

	assert.Equal(t, want.TraceID(), got.TraceID())
	assert.Equal(t, want.SpanID(), got.SpanID())
	assert.Equal(t, want.RequestID, got.RequestID)
	assert.Equal(t, want.SpanName, got.SpanName)
}

func TestTraceFromMissingReturnsZeroValue(t *testing.T) {
	assert.False(t, TraceFrom(context.Background()).Valid())

	//lint:ignore SA1012 explicitly verifying that a nil ctx does not panic
	assert.NotPanics(t, func() { TraceFrom(nil) }) //nolint:staticcheck
	assert.False(t, TraceFrom(nil).Valid()) //nolint:staticcheck
}

func TestNewSpanIDIsUnique(t *testing.T) {
	a, b := newSpanID(), newSpanID()
	assert.True(t, a.IsValid())
	assert.NotEqual(t, a, b)
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'Trace|Fork|SpanID' -v
```

Expected: 编译失败，`undefined: NewTrace`。

- [ ] **Step 3: 写实现**

创建 `log/trace.go`：

```go
package log

import (
	"context"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel/trace"
)

// Trace is the link/trace context of one call.
//
// It embeds OTel's trace.SpanContext, so it can be fed straight into the OTel
// SDK, and can equally be populated from a value parsed out of a W3C
// traceparent header -- no conversion layer needed.
//
// TraceID and RequestID are two encodings of the same 128-bit value:
// the former is the 32-hex-digit form required by W3C, the latter is ULID's
// 26-character Crockford Base32 form. ULID's first 6 bytes are a millisecond
// timestamp, so request_id is naturally time-ordered and easy to compare by eye.
type Trace struct {
	trace.SpanContext

	// ParentSpanID is the upstream span. Zero value for a root span.
	ParentSpanID trace.SpanID

	// SpanName is the current span's name, e.g. "GET /orders/:id", "db.query".
	SpanName string

	// RequestID is TraceID's ULID encoding, unchanged throughout the whole chain.
	RequestID string
}

type traceKey struct{}

// NewTrace starts a new trace.
func NewTrace(name string) Trace {
	id := ulid.Make()
	return Trace{
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    trace.TraceID(id),
			SpanID:     newSpanID(),
			TraceFlags: trace.FlagsSampled,
		}),
		SpanName:  name,
		RequestID: id.String(),
	}
}

// newSpanID generates an 8-byte span_id.
// Takes ULID's [8:16] -- ULID's layout is a 6-byte timestamp plus 10 bytes of
// random entropy, and this slice falls entirely within the random region,
// which is good enough and saves us a direct dependency on crypto/rand.
func newSpanID() trace.SpanID {
	u := ulid.Make()
	return trace.SpanID(u[8:])
}

// Valid reports whether this trace is valid. A zero-value Trace returns false.
func (t Trace) Valid() bool { return t.TraceID().IsValid() }

// Fork derives a child span: trace_id and request_id stay the same, span_id
// is regenerated, and parent points at the current span. Value semantics -- does not mutate the caller.
func (t Trace) Fork(name string) Trace {
	child := t
	child.ParentSpanID = t.SpanID()
	child.SpanContext = t.SpanContext.WithSpanID(newSpanID())
	child.SpanName = name
	return child
}

// WithTrace stores the trace into ctx.
func WithTrace(ctx context.Context, t Trace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom reads the trace from ctx. Returns a zero-value Trace (Valid() == false) if absent, never panics.
func TraceFrom(ctx context.Context) Trace {
	if ctx == nil {
		return Trace{}
	}
	t, _ := ctx.Value(traceKey{}).(Trace)
	return t
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -v
```

Expected: 全部 PASS，包括 1000 次唯一性循环。

- [ ] **Step 5: Commit**

```bash
git add log/trace.go log/trace_test.go
git commit -m "feat(log): Trace 链路模型，内嵌 OTel SpanContext"
```

---

### Task 3: `Config` 与格式推导

**Files:**
- Create: `log/config.go`
- Test: `log/config_test.go`

**Interfaces:**
- Consumes: `ParseLevel`（Task 1）
- Produces:
  - `type Config struct { Level, Stacktrace string; Caller bool; Console ConsoleConfig; File FileConfig; Sampling SamplingConfig; MaskFields []string }`
  - `type ConsoleConfig struct { Enabled bool; Format, Color string }`
  - `type FileConfig struct { Enabled bool; Path, Format, Rotate string; MaxSize, MaxAge, MaxBackups int; Compress bool; ErrorPath string; errorFormat string }`
  - `type SamplingConfig struct { Initial, Thereafter int }`
  - 常量 `FormatConsole = "console"`、`FormatJSON = "json"`、`ColorAuto/ColorAlways/ColorNever`、`RotateDaily = "daily"`、`RotateSize = "size"`
  - `func DefaultConfig() Config`
  - `func (c *Config) Normalize() error` —— 幂等：连调两次结果相同

**为什么 log 包自带默认值与校验：** 内核的配置插件会解析 `default:` tag，但 log 包零框架依赖，不能反过来指望内核。`DefaultConfig()` 是这个包自己的真相源，内核只负责把 YAML 反序列化进来再调 `Normalize()`。

- [ ] **Step 1: 写失败测试**

创建 `log/config_test.go`：

```go
package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigNormalizes(t *testing.T) {
	c := DefaultConfig()
	require.NoError(t, c.Normalize())

	assert.Equal(t, "info", c.Level)
	assert.True(t, c.Caller)
	assert.Equal(t, "error", c.Stacktrace)
	assert.True(t, c.Console.Enabled)
	assert.Equal(t, FormatConsole, c.Console.Format)
	assert.Equal(t, ColorAuto, c.Console.Color)
	assert.False(t, c.File.Enabled, "文件输出默认关闭")
	assert.Equal(t, 100, c.Sampling.Initial)
	assert.Equal(t, 100, c.Sampling.Thereafter)
}

func TestNormalizeIsIdempotent(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = "logs/app.jsonl"

	require.NoError(t, c.Normalize())
	first := c
	require.NoError(t, c.Normalize())
	assert.Equal(t, first, c, "Normalize 必须幂等")
}

// The file extension is the format declaration. Use .jsonl instead of .json --
// the latter makes `jq .` error out on a multi-line file.
func TestFileFormatInferredFromExtension(t *testing.T) {
	cases := map[string]string{
		"logs/app.log":        FormatConsole,
		"logs/app.txt":        FormatConsole,
		"logs/app":            FormatConsole,
		"logs/app.jsonl":      FormatJSON,
		"logs/app.log.jsonl":  FormatJSON,
		"logs/app.ndjson":     FormatJSON,
		"logs/app.json":       FormatJSON,
		"LOGS/APP.JSONL":      FormatJSON,
	}
	for path, want := range cases {
		c := DefaultConfig()
		c.File.Enabled = true
		c.File.Path = path
		c.File.Format = ""
		require.NoError(t, c.Normalize(), path)
		assert.Equal(t, want, c.File.Format, "路径 %q", path)
	}
}

func TestExplicitFormatOverridesInference(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = "logs/app.jsonl"
	c.File.Format = FormatConsole
	require.NoError(t, c.Normalize())
	assert.Equal(t, FormatConsole, c.File.Format, "显式配置压过后缀推导")
}

// error_path has its own extension, and its format is derived independently.
func TestErrorPathFormatInferredIndependently(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = "logs/app.log"
	c.File.ErrorPath = "logs/error.jsonl"
	require.NoError(t, c.Normalize())
	assert.Equal(t, FormatConsole, c.File.Format)
	assert.Equal(t, FormatJSON, c.File.errorFormat)
}

func TestNormalizeRejectsBadEnums(t *testing.T) {
	bad := []struct {
		name  string
		mutate func(*Config)
	}{
		{"level", func(c *Config) { c.Level = "verbose" }},
		{"stacktrace", func(c *Config) { c.Stacktrace = "always" }},
		{"console.format", func(c *Config) { c.Console.Format = "logfmt" }},
		{"console.color", func(c *Config) { c.Console.Color = "maybe" }},
		{"file.format", func(c *Config) { c.File.Enabled = true; c.File.Format = "xml" }},
		{"file.rotate", func(c *Config) { c.File.Enabled = true; c.File.Rotate = "hourly" }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultConfig()
			tc.mutate(&c)
			assert.Error(t, c.Normalize())
		})
	}
}

func TestFileEnabledWithoutPathGetsDefault(t *testing.T) {
	c := DefaultConfig()
	c.File.Enabled = true
	c.File.Path = ""
	require.NoError(t, c.Normalize())
	assert.Equal(t, "logs/app.log", c.File.Path)
}

func TestNoSinkEnabledIsAllowed(t *testing.T) {
	c := DefaultConfig()
	c.Console.Enabled = false
	c.File.Enabled = false
	assert.NoError(t, c.Normalize(), "全关等价于 Nop，是合法配置（测试环境常用）")
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'Config|Format|Normalize|Sink' -v
```

Expected: 编译失败，`undefined: DefaultConfig`。

- [ ] **Step 3: 写实现**

创建 `log/config.go`：

```go
package log

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Rendering formats.
const (
	FormatConsole = "console" // aligned + colored, for humans
	FormatJSON    = "json"    // one JSON object per line, for machine indexing
)

// console coloring modes.
const (
	ColorAuto   = "auto"
	ColorAlways = "always"
	ColorNever  = "never"
)

// File rotation strategies.
const (
	RotateDaily = "daily" // rotate once per calendar day (implemented by this package; lumberjack itself only rotates by size)
	RotateSize  = "size"  // rotate only by max_size
)

// Config is the logging configuration.
//
// Format is per-sink rather than global -- "console to the terminal, json to
// a file" is the most common combination, and a single global format can't express it.
type Config struct {
	Level      string `yaml:"level"      json:"level"`
	Caller     bool   `yaml:"caller"     json:"caller"`
	Stacktrace string `yaml:"stacktrace" json:"stacktrace"`

	Console ConsoleConfig `yaml:"console" json:"console"`
	File    FileConfig    `yaml:"file"    json:"file"`

	Sampling SamplingConfig `yaml:"sampling" json:"sampling"`

	// MaskFields is appended to the built-in mask blacklist. Additive only -- built-in entries cannot be removed.
	MaskFields []string `yaml:"mask_fields" json:"mask_fields"`
}

type ConsoleConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Format  string `yaml:"format"  json:"format"`
	Color   string `yaml:"color"   json:"color"`
}

type FileConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Path    string `yaml:"path"    json:"path"`

	// Format left empty is inferred from Path's extension: .jsonl/.json/.ndjson → json, everything else → console.
	Format string `yaml:"format" json:"format"`

	Rotate     string `yaml:"rotate"      json:"rotate"`
	MaxSize    int    `yaml:"max_size"    json:"max_size"`    // MB
	MaxAge     int    `yaml:"max_age"     json:"max_age"`     // days
	MaxBackups int    `yaml:"max_backups" json:"max_backups"` // count
	Compress   bool   `yaml:"compress"    json:"compress"`

	// ErrorPath, when non-empty, opens an extra sink that only receives error level and above.
	ErrorPath string `yaml:"error_path" json:"error_path"`

	// errorFormat is derived by Normalize from ErrorPath's extension; not exposed externally.
	errorFormat string
}

type SamplingConfig struct {
	Initial    int `yaml:"initial"    json:"initial"`
	Thereafter int `yaml:"thereafter" json:"thereafter"`
}

// DefaultConfig is this package's source of truth for defaults.
// The kernel's config plugin should deserialize YAML into this struct and then call Normalize.
func DefaultConfig() Config {
	return Config{
		Level:      "info",
		Caller:     true,
		Stacktrace: "error",
		Console: ConsoleConfig{
			Enabled: true,
			Format:  FormatConsole,
			Color:   ColorAuto,
		},
		File: FileConfig{
			Enabled:    false,
			Path:       "logs/app.log",
			Rotate:     RotateDaily,
			MaxSize:    100,
			MaxAge:     30,
			MaxBackups: 30,
			Compress:   true,
		},
		Sampling: SamplingConfig{Initial: 100, Thereafter: 100},
	}
}

// Normalize fills in defaults, infers formats, and validates enums. Idempotent.
func (c *Config) Normalize() error {
	if c.Level == "" {
		c.Level = "info"
	}
	if _, err := ParseLevel(c.Level); err != nil {
		return fmt.Errorf("log.level: %w", err)
	}
	if c.Stacktrace == "" {
		c.Stacktrace = "error"
	}
	if _, err := ParseLevel(c.Stacktrace); err != nil {
		return fmt.Errorf("log.stacktrace: %w", err)
	}

	if c.Console.Format == "" {
		c.Console.Format = FormatConsole
	}
	if err := checkFormat("log.console.format", c.Console.Format); err != nil {
		return err
	}
	if c.Console.Color == "" {
		c.Console.Color = ColorAuto
	}
	switch c.Console.Color {
	case ColorAuto, ColorAlways, ColorNever:
	default:
		return fmt.Errorf("log.console.color: 未知取值 %q，可选 auto/always/never", c.Console.Color)
	}

	if c.File.Enabled {
		if c.File.Path == "" {
			c.File.Path = "logs/app.log"
		}
		if c.File.Format == "" {
			c.File.Format = formatFromPath(c.File.Path)
		}
		if err := checkFormat("log.file.format", c.File.Format); err != nil {
			return err
		}
		if c.File.Rotate == "" {
			c.File.Rotate = RotateDaily
		}
		switch c.File.Rotate {
		case RotateDaily, RotateSize:
		default:
			return fmt.Errorf("log.file.rotate: 未知取值 %q，可选 daily/size", c.File.Rotate)
		}
		if c.File.MaxSize <= 0 {
			c.File.MaxSize = 100
		}
		if c.File.MaxAge < 0 {
			c.File.MaxAge = 0
		}
		if c.File.MaxBackups < 0 {
			c.File.MaxBackups = 0
		}
		if c.File.ErrorPath != "" {
			c.File.errorFormat = formatFromPath(c.File.ErrorPath)
		} else {
			c.File.errorFormat = ""
		}
	}

	if c.Sampling.Initial < 0 {
		c.Sampling.Initial = 0
	}
	if c.Sampling.Thereafter < 0 {
		c.Sampling.Thereafter = 0
	}
	return nil
}

func checkFormat(field, v string) error {
	switch v {
	case FormatConsole, FormatJSON:
		return nil
	default:
		return fmt.Errorf("%s: 未知取值 %q，可选 console/json", field, v)
	}
}

// formatFromPath infers the rendering format from the file extension.
// The file extension is the format declaration: writing .jsonl means it's meant
// for machines to read, writing .log means it's meant for humans to read.
func formatFromPath(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jsonl", ".json", ".ndjson":
		return FormatJSON
	default:
		return FormatConsole
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -v
```

Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add log/config.go log/config_test.go
git commit -m "feat(log): 配置结构与文件后缀推导格式"
```

---

### Task 4: Core 层强制脱敏

**Files:**
- Create: `log/mask.go`
- Test: `log/mask_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `func newMasker(extra []string) *masker`
  - `func (m *masker) hit(key string) bool`
  - `func (m *masker) apply(fs []zapcore.Field) []zapcore.Field`
  - `func newMaskCore(c zapcore.Core, m *masker) zapcore.Core`
  - `const maskPlaceholder = "***"`

**`[SEC-INFO]` 为什么做在 Core 层而不是 Encoder 层：** spec §8.8 说"做在 encoder 层"，实现上落到 `zapcore.Core` —— 安全语义完全相同（调用点之外的统一拦截，调用方无法绕过），但可靠性高一个量级。zap 的字段有两条路径：`logger.With(kv)` 走 `Core.With([]Field)`，`logger.Info(msg, kv)` 走 `Core.Write(entry, []Field)`。包一层 Core 覆盖这两个方法就全拦住了，一共 5 个方法；包 Encoder 则要覆盖 `zapcore.ObjectEncoder` 的 20 多个 `Add*` 方法，**漏一个就是一条明文密码进日志**。

**装配位置（Task 7 会用到）：** 每个叶子 sink 各包一层 maskCore，maskCore 在 `zapcore.NewTee(...)` **之内**，采样器在 Tee **之外**。不能反过来把 maskCore 包在 Tee 之外 —— `maskCore.Check` 会把自己挂进 CheckedEntry，Tee 的 per-sink 级别过滤（`zapcore/tee.go:74-79`）便再也不会执行，`error_path` 会收到全量日志。因为 maskCore 是 `*zap.Logger` 的组成部分，`log.Zap()` 逃生舱口拿到的 logger 同样被覆盖。

- [ ] **Step 1: 写失败测试**

创建 `log/mask_test.go`：

```go
package log

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func maskedLogger(extra []string) (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(newMaskCore(core, newMasker(extra))), logs
}

func TestMaskAppliesToWriteFields(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("login", zap.String("password", "hunter2"), zap.String("user", "alice"))

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, maskPlaceholder, m["password"])
	assert.Equal(t, "alice", m["user"], "非敏感字段不受影响")
}

// This test proves the value of the Core-layer approach: fields derived via With are also caught.
func TestMaskAppliesToWithFields(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.With(zap.String("access_token", "abc.def.ghi")).Info("call upstream")

	require.Len(t, logs.All(), 1)
	assert.Equal(t, maskPlaceholder, logs.All()[0].ContextMap()["access_token"])
}

// Regression test: if you forget to override Check when wrapping a zapcore.Core,
// the CheckedEntry gets the inner Core attached, and Write bypasses masking entirely.
func TestMaskCoreCheckRoutesThroughWrapper(t *testing.T) {
	l, logs := maskedLogger(nil)
	ce := l.Check(zapcore.InfoLevel, "manual check")
	require.NotNil(t, ce)
	ce.Write(zap.String("secret", "s3cr3t"))

	require.Len(t, logs.All(), 1)
	assert.Equal(t, maskPlaceholder, logs.All()[0].ContextMap()["secret"])
}

func TestMaskNormalizesKeyStyle(t *testing.T) {
	m := newMasker(nil)
	for _, k := range []string{
		"accessToken", "ACCESS_TOKEN", "access-token", "Access.Token", "access token",
	} {
		assert.True(t, m.hit(k), "应命中：%q", k)
	}
}

func TestMaskMatchesExactlyNotByPrefix(t *testing.T) {
	m := newMasker(nil)
	assert.True(t, m.hit("phone"))
	assert.False(t, m.hit("phone_masked"), "已脱敏的字段不该被二次替换")
	assert.False(t, m.hit("mobile_type"), "前缀相同但语义无关的字段不该误伤")
	assert.False(t, m.hit("token_count"), "token_count 是数量不是凭据")
}

func TestBuiltinBlacklistCannotBeRemoved(t *testing.T) {
	// There is no API to "configure away" a built-in entry; this verifies that appending doesn't affect the built-ins.
	m := newMasker([]string{"salary", "  ", ""})
	assert.True(t, m.hit("salary"), "配置项生效")
	assert.True(t, m.hit("password"), "内置黑名单始终生效")
	assert.True(t, m.hit("private_key"))
	assert.True(t, m.hit("id_card"))
	assert.True(t, m.hit("bank_card"))
}

func TestMaskCoversOrgMandatedBlacklist(t *testing.T) {
	m := newMasker(nil)
	// The absolute blacklist mandated by the org's security policy -- not one of these can be missing
	for _, k := range []string{
		"password", "token", "ulp-token", "access_token", "refresh_token",
		"AK", "SK", "private_key", "db_url", "bank_card", "id_card", "phone",
	} {
		assert.True(t, m.hit(k), "组织规范要求脱敏：%q", k)
	}
}

func TestMaskDoesNotAllocateWhenNothingHits(t *testing.T) {
	m := newMasker(nil)
	in := []zapcore.Field{zap.String("user", "alice"), zap.Int("age", 30)}
	out := m.apply(in)
	assert.Equal(t, &in[0], &out[0], "无命中时应原样返回同一个切片，不复制")
}

func TestMaskDoesNotMutateInput(t *testing.T) {
	m := newMasker(nil)
	in := []zapcore.Field{zap.String("password", "hunter2")}
	out := m.apply(in)
	assert.Equal(t, "hunter2", in[0].String, "输入切片不能被改写")
	assert.Equal(t, maskPlaceholder, out[0].String)
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'Mask|Blacklist' -v
```

Expected: 编译失败，`undefined: newMasker`。

- [ ] **Step 3: 写实现**

创建 `log/mask.go`：

```go
package log

import (
	"strings"

	"go.uber.org/zap/zapcore"
)

// maskPlaceholder is the value a sensitive field is replaced with.
const maskPlaceholder = "***"

// builtinMaskFields is the built-in masking blacklist, always in effect and not
// removable via configuration. Based on the org's security policy's "log absolute
// blacklist", plus common variants.
//
// There are thousands of call sites; expecting every one of them to remember to
// mask is unrealistic. This is the only interception point.
var builtinMaskFields = []string{
	// credentials
	"password", "passwd", "pwd", "old_password", "new_password",
	// tokens
	"token", "ulp-token", "access_token", "refresh_token", "id_token",
	"authorization", "cookie", "set-cookie", "session_id", "jwt",
	// secrets
	"secret", "client_secret", "private_key", "api_key",
	"ak", "sk", "access_key", "access_key_id", "secret_key", "secret_access_key",
	// connection strings
	"db_url", "dsn", "database_url", "conn_str",
	// personal information
	"id_card", "bank_card", "credit_card", "card_no", "cvv", "phone", "mobile",
}

// masker determines whether a field name needs masking.
type masker struct{ keys map[string]struct{} }

// newMasker builds from the built-in blacklist plus extra. extra can only add entries, never remove built-in ones.
func newMasker(extra []string) *masker {
	m := &masker{keys: make(map[string]struct{}, len(builtinMaskFields)+len(extra))}
	for _, k := range builtinMaskFields {
		m.keys[normalizeMaskKey(k)] = struct{}{}
	}
	for _, k := range extra {
		if nk := normalizeMaskKey(k); nk != "" {
			m.keys[nk] = struct{}{}
		}
	}
	return m
}

// normalizeMaskKey normalizes a field name: lowercases it and strips separators.
// This way accessToken / access_token / access-token / ACCESS_TOKEN all hit the same rule.
//
// Matching after normalization is exact, not prefix-based -- phone hits,
// phone_masked doesn't, so a field that's already been masked doesn't get
// masked a second time.
func normalizeMaskKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	for _, r := range k {
		switch r {
		case '_', '-', '.', ' ':
			// separators are all discarded
		default:
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m *masker) hit(key string) bool {
	_, ok := m.keys[normalizeMaskKey(key)]
	return ok
}

// apply returns the masked field slice. When nothing hits, returns the input as-is with no allocation.
// When something hits, makes a copy before modifying it -- never writes over the caller's slice.
func (m *masker) apply(fs []zapcore.Field) []zapcore.Field {
	var out []zapcore.Field
	for i := range fs {
		if !m.hit(fs[i].Key) {
			continue
		}
		if out == nil {
			out = make([]zapcore.Field, len(fs))
			copy(out, fs)
		}
		out[i] = zapcore.Field{
			Key:    fs[i].Key,
			Type:   zapcore.StringType,
			String: maskPlaceholder,
		}
	}
	if out == nil {
		return fs
	}
	return out
}

// maskCore intercepts sensitive fields at the Core layer.
//
// Assembly: each leaf sink gets its own maskCore, INSIDE zapcore.NewTee(...);
// the sampler wraps OUTSIDE the Tee. Never reverse this -- see Task 7 and the
// shipped doc comment in log/mask.go for the full argument.
//
// Since maskCore is part of *zap.Logger, the log.Zap() escape hatch is covered too.
//
// The inner core MUST live in an unexported field, NOT be embedded as
// zapcore.Core: embedding produces the exported implicit field name Core, and
// reflect's CanInterface() returns true for it, so anyone holding this core can
// pull out the unmasked inner core and write through it directly.
type maskCore struct {
	inner zapcore.Core
	m     *masker
}

func newMaskCore(c zapcore.Core, m *masker) zapcore.Core {
	return &maskCore{inner: c, m: m}
}

// Enabled and Sync must be implemented explicitly: with no embedding there is
// nothing to inherit, and that is the point -- when zapcore.Core gains a new
// method later, embedding would silently inherit the inner implementation (a
// new bypass path), whereas an explicit implementation makes it a compile error.
func (c *maskCore) Enabled(l zapcore.Level) bool { return c.inner.Enabled(l) }

func (c *maskCore) Sync() error { return c.inner.Sync() }

func (c *maskCore) With(fs []zapcore.Field) zapcore.Core {
	return &maskCore{inner: c.inner.With(c.m.apply(fs)), m: c.m}
}

// Check must be overridden. The base Check attaches the inner Core to the
// CheckedEntry, and the subsequent Write hits the inner Core directly, bypassing masking entirely.
func (c *maskCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *maskCore) Write(ent zapcore.Entry, fs []zapcore.Field) error {
	return c.inner.Write(ent, c.m.apply(fs))
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -v
```

Expected: 全部 PASS。特别确认 `TestMaskCoreCheckRoutesThroughWrapper` 和 `TestMaskAppliesToWithFields` 通过。

- [ ] **Step 5: 手工验证 Check 陷阱确实存在**

临时把 `mask.go` 里的 `Check` 方法整个注释掉，重跑：

```bash
go test ./log/ -run TestMaskCoreCheckRoutesThroughWrapper -v
```

Expected: FAIL，输出 `s3cr3t` 而不是 `***`。确认失败后**把 Check 方法改回来**再跑一遍确认 PASS。这一步是为了让实现者亲眼看到这个陷阱，不要在后续重构中把它删掉。

- [ ] **Step 6: Commit**

```bash
git add log/mask.go log/mask_test.go
git commit -m "feat(log): Core 层强制脱敏，内置黑名单不可关闭"
```

---

### Task 5: console encoder

**Files:**
- Create: `log/console.go`
- Test: `log/console_test.go`

**Interfaces:**
- Consumes: 无（只依赖 zapcore 与 go-isatty）
- Produces:
  - `func newConsoleEncoder(color bool) zapcore.Encoder`
  - `func wantColor(mode string, w io.Writer) bool`
  - `const ansiReset/ansiDim/ansiRed/ansiGreen/ansiYellow/ansiCyan`

**输出格式（spec §8.7）：**

```
10:23:45.123 INFO  01926f7e         order/service.go:42  校验通过  order_id=1001 amount=99
10:23:45.201 ERROR 01926f7e        payment/client.go:33  支付失败  err="connection refused"
```

| 段 | 宽度 | 规则 |
|---|---|---|
| 时间 | 12，固定 | `15:04:05.000`，不打日期（日期在文件名里） |
| level | 5，左对齐 | 按级着色 |
| trace | 8，固定 | trace_id 前 8 位；无 trace 时 8 个空格 |
| caller | 24，右对齐 | 超长从**左侧**截断加 `…` |
| msg | 变长 | 后跟两个空格再接 KV |
| KV | 变长 | `key=value`，含空格的 value 加引号 |

**实现路径（已验证）：** 嵌入 `*zapcore.MapObjectEncoder` 白拿 `ObjectEncoder` 的 20 多个 `Add*` 方法，自己只补 `Clone()` 与 `EncodeEntry()` 两个方法即满足 `zapcore.Encoder`。

**两个刻意决策：**
1. **字段按 key 字母序输出。** `MapObjectEncoder` 是 map，本就无序；排序既保证输出确定性（测试可断言），又让同名字段每行落在相似位置，扫读更快。
2. **console 下剔除 `trace_id` / `span_id` / `request_id` 三个 KV。** `trace_id` 已在固定列显示，另两个是 128/64 bit 的 ID，挤在人读的行里毫无价值。`span_name` 保留（有信息量）。json sink 全留 —— 那是给机器检索的。

- [ ] **Step 1: 写失败测试**

创建 `log/console_test.go`：

```go
package log

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func encodeOne(t *testing.T, color bool, ent zapcore.Entry, with []zapcore.Field, fs []zapcore.Field) string {
	t.Helper()
	enc := newConsoleEncoder(color)
	for _, f := range with {
		f.AddTo(enc)
	}
	buf, err := enc.EncodeEntry(ent, fs)
	require.NoError(t, err)
	return buf.String()
}

func sampleEntry() zapcore.Entry {
	return zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    time.Date(2026, 8, 24, 10, 23, 45, 123_000_000, time.Local),
		Message: "校验通过",
		Caller: zapcore.EntryCaller{
			Defined: true,
			File:    "/home/u/proj/order/service.go",
			Line:    42,
		},
	}
}

func TestConsoleLayout(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("trace_id", "01926f7e1a2b3c4d5e6f708192a3b4c5"),
		zap.Int("order_id", 1001),
		zap.Int("amount", 99),
	})

	assert.True(t, strings.HasSuffix(out, "\n"), "必须以换行结尾")
	line := strings.TrimSuffix(out, "\n")

	assert.True(t, strings.HasPrefix(line, "10:23:45.123 "), "时间段：%q", line)
	assert.Contains(t, line, "INFO  ", "level 左对齐补到 5 宽")
	assert.Contains(t, line, "01926f7e", "trace 列取 trace_id 前 8 位")
	assert.NotContains(t, line, "01926f7e1a2b", "console 不打完整 trace_id")
	assert.Contains(t, line, "order/service.go:42")
	assert.Contains(t, line, "校验通过")
	assert.Contains(t, line, "amount=99")
	assert.Contains(t, line, "order_id=1001")
	assert.NotContains(t, line, "trace_id=", "trace_id 已占固定列，不再进 KV 区")
}

func TestConsoleFieldsSortedByKey(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.Int("zebra", 1), zap.Int("apple", 2), zap.Int("mango", 3),
	})
	assert.Less(t, strings.Index(out, "apple="), strings.Index(out, "mango="))
	assert.Less(t, strings.Index(out, "mango="), strings.Index(out, "zebra="))
}

func TestConsoleWithFieldsComeBeforeCallFields(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(),
		[]zapcore.Field{zap.String("service", "order")},
		[]zapcore.Field{zap.Int("amount", 99)},
	)
	assert.Less(t, strings.Index(out, "service=order"), strings.Index(out, "amount=99"),
		"With 的上下文字段排在本次调用的字段之前")
}

func TestConsoleCallerTruncatedFromLeft(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/a/very/deeply/nested/package/path/that/is/long/handler.go"
	ent.Caller.Line = 1234

	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "…", "超长 caller 从左侧截断")
	assert.Contains(t, out, "handler.go:1234", "行号一侧必须完整保留")
}

func TestConsoleShortCallerRightAligned(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/x/a/b.go"
	ent.Caller.Line = 7

	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "   a/b.go:7", "短 caller 右对齐补空格")
}

func TestConsoleNoCallerWhenUndefined(t *testing.T) {
	ent := sampleEntry()
	ent.Caller = zapcore.EntryCaller{}
	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "校验通过")
	assert.NotContains(t, out, "undefined")
}

func TestConsoleQuotesValuesWithSpaces(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.Error(errors.New("connection refused")),
		zap.String("plain", "ok"),
		zap.String("empty", ""),
	})
	assert.Contains(t, out, `error="connection refused"`)
	assert.Contains(t, out, "plain=ok", "不含空格的值不加引号")
	assert.Contains(t, out, `empty=""`)
}

// Chinese characters must not be escaped to \uXXXX -- this is exactly why strconv.Quote can't be used.
func TestConsoleKeepsCJKLiteral(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("reason", "余额 不足"),
	})
	assert.Contains(t, out, `reason="余额 不足"`)
	assert.NotContains(t, out, `\u`)
}

func TestConsoleEscapesQuotesAndNewlines(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("raw", "he said \"hi\"\nbye"),
	})
	assert.Contains(t, out, `raw="he said \"hi\"\nbye"`)
	assert.Equal(t, 1, strings.Count(out, "\n"), "值里的换行必须转义，一条日志只占一行")
}

func TestConsoleColoring(t *testing.T) {
	out := encodeOne(t, true, sampleEntry(), nil, []zapcore.Field{
		zap.Error(errors.New("boom")),
	})
	assert.Contains(t, out, ansiGreen+"INFO  "+ansiReset, "INFO 用绿色")
	assert.Contains(t, out, ansiRed, "err 字段的值用红色")
	assert.Contains(t, out, ansiCyan, "key 用青色")
	assert.Contains(t, out, ansiDim, "时间/trace/caller 用暗灰")
}

func TestConsoleLevelColors(t *testing.T) {
	cases := map[zapcore.Level]string{
		zapcore.DebugLevel:  ansiCyan,
		zapcore.InfoLevel:   ansiGreen,
		zapcore.WarnLevel:   ansiYellow,
		zapcore.ErrorLevel:  ansiRed,
		zapcore.DPanicLevel: ansiRed,
	}
	for lv, want := range cases {
		ent := sampleEntry()
		ent.Level = lv
		out := encodeOne(t, true, ent, nil, nil)
		assert.Contains(t, out, want+padRight(lv.CapitalString(), widthLevel)+ansiReset, "级别 %s", lv)
	}
}

// The level column for every level must occupy exactly widthLevel columns -- DPANIC is 6
// characters, the longest of all levels; widthLevel less than that would shift the rest of that row right.
func TestConsoleLevelColumnWidthIsUniform(t *testing.T) {
	levels := []zapcore.Level{
		zapcore.DebugLevel, zapcore.InfoLevel, zapcore.WarnLevel,
		zapcore.ErrorLevel, zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel,
	}
	for _, lv := range levels {
		ent := sampleEntry()
		ent.Level = lv
		out := encodeOne(t, false, ent, nil, nil)
		// The time segment is fixed at 12 + 1 space wide; what follows for widthLevel columns is the level segment
		seg := out[13 : 13+widthLevel]
		assert.Equal(t, lv.CapitalString(), strings.TrimRight(seg, " "), "级别 %s 的文本", lv)
		assert.Equal(t, widthLevel, len(seg), "级别 %s 的列宽", lv)
		assert.Equal(t, byte(' '), out[13+widthLevel], "级别 %s 后必须紧跟分隔空格", lv)
	}
}

// Coloring codes have zero width: colored and uncolored output must be byte-for-byte identical after stripping ANSI.
func TestConsoleColorDoesNotBreakAlignment(t *testing.T) {
	ent := sampleEntry()
	fs := []zapcore.Field{zap.Int("n", 1)}
	plain := encodeOne(t, false, ent, nil, fs)
	colored := stripANSI(encodeOne(t, true, ent, nil, fs))
	assert.Equal(t, plain, colored)
}

// Fields after zap.Namespace get collected by MapObjectEncoder into a nested map, leaving only the namespace name at the top level.
// console must flatten it into a dotted full path instead of printing Go's map[k:v] literal.
func TestConsoleFlattensNamespace(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("svc", "order"),
		zap.Namespace("db"),
		zap.String("host", "10.0.0.1"),
		zap.Int("port", 5432),
	})
	assert.Contains(t, out, "db.host=10.0.0.1")
	assert.Contains(t, out, "db.port=5432")
	assert.Contains(t, out, "svc=order")
	assert.NotContains(t, out, "map[", "不能落 Go 的 map 字面量")
}

// After flattening, consoleHiddenFields judges by full path: the top-level trace_id is
// still dropped as before; a same-named field inside a namespace was placed there explicitly by the caller, so it's kept.
func TestConsoleHiddenFieldsUseFullPath(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("trace_id", "0192abcd0192abcd"),
		zap.Namespace("upstream"),
		zap.String("trace_id", "ffffffffffffffff"),
	})
	assert.NotContains(t, out, "trace_id=0192abcd0192abcd", "顶层 trace_id 已占固定列，不进 KV 区")
	assert.Contains(t, out, "upstream.trace_id=ffffffffffffffff")
}

// A self-referencing map must not make the flattening recursion get stuck.
func TestConsoleNestedDepthIsBounded(t *testing.T) {
	m := map[string]any{"k": "v"}
	m["self"] = m
	done := make(chan string, 1)
	go func() { done <- encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{zap.Any("m", m)}) }()
	select {
	case out := <-done:
		assert.Contains(t, out, "m.k=v")
	case <-time.After(5 * time.Second):
		t.Fatal("展平递归没有停下来")
	}
}

// ByteString / Binary are both []byte in MapObjectEncoder;
// without special-casing this it would print [104 105] instead of hi.
func TestConsoleRendersByteStringAsText(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.ByteString("body", []byte("hi")),
	})
	assert.Contains(t, out, "body=hi")
	assert.NotContains(t, out, "[104 105]")
}

// A caller path containing Chinese characters must not get sliced into a U+FFFD, and must not be misaligned by counting bytes.
func TestConsoleCallerHandlesMultibyte(t *testing.T) {
	ent := sampleEntry()
	ent.Caller = zapcore.EntryCaller{
		Defined: true,
		File:    "/src/中文目录名很长很长很长/service/order.go",
		Line:    42,
	}
	out := encodeOne(t, false, ent, nil, nil)
	assert.True(t, utf8.ValidString(out), "输出必须是合法 UTF-8")
	assert.NotContains(t, out, "�", "不能切出替换字符")
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && s[j] != 'm' {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func TestConsoleCloneIsolatesFields(t *testing.T) {
	base := newConsoleEncoder(false)
	zap.String("shared", "yes").AddTo(base)

	clone := base.Clone()
	zap.String("only_in_clone", "1").AddTo(clone)

	baseOut, err := base.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)
	cloneOut, err := clone.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)

	assert.Contains(t, baseOut.String(), "shared=yes")
	assert.NotContains(t, baseOut.String(), "only_in_clone", "Clone 之后写子实例不能污染父实例")
	assert.Contains(t, cloneOut.String(), "shared=yes")
	assert.Contains(t, cloneOut.String(), "only_in_clone=1")
}

func TestConsoleStacktraceAppended(t *testing.T) {
	ent := sampleEntry()
	ent.Level = zapcore.ErrorLevel
	ent.Stack = "goroutine 1 [running]:\nmain.main()"
	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "goroutine 1 [running]:")
	assert.True(t, strings.HasSuffix(out, "\n"))
}

func TestWantColor(t *testing.T) {
	var buf bytes.Buffer // not a *os.File, cannot possibly be a TTY

	assert.True(t, wantColor(ColorAlways, &buf), "always 无条件开")
	assert.False(t, wantColor(ColorNever, &buf), "never 无条件关")
	assert.False(t, wantColor(ColorAuto, &buf), "auto 下非 TTY 关")

	t.Setenv("NO_COLOR", "1")
	assert.False(t, wantColor(ColorAuto, &buf))
	assert.True(t, wantColor(ColorAlways, &buf), "显式 always 压过 NO_COLOR")
}

func TestWantColorRespectsDumbTerm(t *testing.T) {
	var buf bytes.Buffer
	t.Setenv("TERM", "dumb")
	assert.False(t, wantColor(ColorAuto, &buf))
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'Console|WantColor' -v
```

Expected: 编译失败，`undefined: newConsoleEncoder`。

- [ ] **Step 3: 写实现**

创建 `log/console.go`：

```go
package log

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// ANSI color codes. Zero width, does not affect alignment.
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[90m" // bright black = dim gray
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// Fixed width of each segment.
//
// widthLevel is 6, not 5: the longest level text in zapcore is DPANIC (6 characters,
// measured `zapcore.DPanicLevel.CapitalString()` == "DPANIC"). Using 5 would make
// padRight return the string unchanged when len(s) >= w, shifting the remaining four
// segments of the DPanic row right by one column.
const (
	widthLevel  = 6
	widthTrace  = 8
	widthCaller = 24
)

// Fields that don't go into the KV area under console: trace_id already occupies a
// fixed column, and the other two are 128/64-bit IDs with no value crammed into a
// row meant for humans to read.
// json sink does not do this stripping -- that's for machine indexing.
var consoleHiddenFields = map[string]struct{}{
	"trace_id":   {},
	"span_id":    {},
	"request_id": {},
}

var consolePool = buffer.NewPool()

// consoleEncoder renders one line for human reading.
//
// Embeds *zapcore.MapObjectEncoder to get ObjectEncoder's Add* methods for free;
// it only needs to supply Clone and EncodeEntry to satisfy zapcore.Encoder.
type consoleEncoder struct {
	*zapcore.MapObjectEncoder
	color bool
}

func newConsoleEncoder(color bool) zapcore.Encoder {
	return &consoleEncoder{MapObjectEncoder: zapcore.NewMapObjectEncoder(), color: color}
}

func (e *consoleEncoder) Clone() zapcore.Encoder {
	c := &consoleEncoder{MapObjectEncoder: zapcore.NewMapObjectEncoder(), color: e.color}
	for k, v := range e.Fields {
		c.Fields[k] = v
	}
	return c
}

func (e *consoleEncoder) EncodeEntry(ent zapcore.Entry, fs []zapcore.Field) (*buffer.Buffer, error) {
	// This call's own fields get collected separately, so they can be grouped
	// and output apart from the With context fields.
	call := zapcore.NewMapObjectEncoder()
	for _, f := range fs {
		f.AddTo(call)
	}

	b := consolePool.Get()

	// ① time: fixed 12 wide, no date printed -- the date is in the file name
	e.paint(b, ansiDim, ent.Time.Format("15:04:05.000"))
	b.AppendByte(' ')

	// ② level: 5 wide, left-aligned, colored by level
	e.paint(b, levelColor(ent.Level), padRight(ent.Level.CapitalString(), widthLevel))
	b.AppendByte(' ')

	// ③ trace: 8 wide fixed, first 8 characters of trace_id
	e.paint(b, ansiDim, padRight(shortTrace(e.Fields, call.Fields), widthTrace))
	b.AppendByte(' ')

	// ④ caller: 24 wide, right-aligned, truncated from the left when too long
	e.paint(b, ansiDim, padCallerLeft(callerText(ent.Caller), widthCaller))
	b.AppendByte(' ')

	// ⑤ msg, followed by two spaces then the KVs
	b.AppendString(ent.Message)
	b.AppendString("  ")

	// ⑥ KV: the With context fields first, then this call's fields, each sorted alphabetically by key
	first := true
	e.writeFields(b, e.Fields, &first)
	e.writeFields(b, call.Fields, &first)

	if ent.Stack != "" {
		b.AppendByte('\n')
		b.AppendString(ent.Stack)
	}
	b.AppendByte('\n')
	return b, nil
}

func (e *consoleEncoder) writeFields(b *buffer.Buffer, m map[string]any, first *bool) {
	e.writeFieldsPrefixed(b, m, "", first, 0)
}

// maxConsoleDepth is the depth limit for flattening nesting. A self-referencing map
// would make the recursion never stop, and a human-readable line doesn't need more
// than eight levels of structure anyway. Once the limit is hit, fall back to stringify to finish it off in one shot.
const maxConsoleDepth = 8

// writeFieldsPrefixed recursively flattens nested maps, joining levels into a
// full dotted-path key.
//
// Why flattening is needed: fields after zap.Namespace("db") don't get laid out
// flat at the top level -- MapObjectEncoder.OpenNamespace collects them into a
// nested map, leaving only "db" as a single key at the top level. Measured: after
// Namespace("ns"), all three fields AddTo'd afterward land in ns's sub-map, and
// the top level's len == 1. Calling stringify directly would print Go's map[k:v]
// literal, which is both hard to read and makes consoleHiddenFields's stripping
// completely ineffective at the sub-level.
//
// Flattening it into db.host=... db.port=... solves both problems at once: the
// shape corresponds one-to-one with json sink's nested semantics (in json it's
// {"db":{"host":...}}), and the stripping check gets the full path to work with.
// A map[string]any passed in directly by the caller gets flattened the same way -- consistent with namespace's shape.
func (e *consoleEncoder) writeFieldsPrefixed(b *buffer.Buffer, m map[string]any, prefix string, first *bool, depth int) {
	if len(m) == 0 {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		full := k
		if prefix != "" {
			full = prefix + "." + k
		}
		if _, hidden := consoleHiddenFields[full]; hidden {
			continue
		}

		// Keep flattening nested maps; skip an empty sub-map entirely, don't leave a lone "key=" behind.
		if sub, ok := m[k].(map[string]any); ok && depth < maxConsoleDepth {
			e.writeFieldsPrefixed(b, sub, full, first, depth+1)
			continue
		}

		if !*first {
			b.AppendByte(' ')
		}
		*first = false

		e.paint(b, ansiCyan, full)
		b.AppendByte('=')

		s := stringify(m[k])
		if e.color && isErrorKey(k) {
			b.AppendString(ansiRed)
			writeConsoleValue(b, s)
			b.AppendString(ansiReset)
		} else {
			writeConsoleValue(b, s)
		}
	}
}

// paint writes with color when enabled; when color is off, it only writes the text.
// Pad first, then color -- ANSI sequences have zero width and won't break alignment.
func (e *consoleEncoder) paint(b *buffer.Buffer, c, s string) {
	if e.color {
		b.AppendString(c)
		b.AppendString(s)
		b.AppendString(ansiReset)
		return
	}
	b.AppendString(s)
}

func levelColor(lv zapcore.Level) string {
	switch lv {
	case zapcore.DebugLevel:
		return ansiCyan
	case zapcore.WarnLevel:
		return ansiYellow
	case zapcore.ErrorLevel, zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel:
		return ansiRed
	default:
		return ansiGreen
	}
}

func isErrorKey(k string) bool { return k == "err" || k == "error" }

// shortTrace takes the first 8 characters of trace_id. Returns an empty string when there's no trace (padRight pads it into a blank column).
func shortTrace(ms ...map[string]any) string {
	for _, m := range ms {
		if v, ok := m["trace_id"].(string); ok && v != "" {
			if len(v) > widthTrace {
				return v[:widthTrace]
			}
			return v
		}
	}
	return ""
}

func callerText(c zapcore.EntryCaller) string {
	if !c.Defined {
		return ""
	}
	return c.TrimmedPath()
}

// padRight / padCallerLeft count by rune, not by byte.
//
// Of the three leading columns (level, trace, caller), level and trace are always
// ASCII, but caller comes from a Go source file path, and a user's directory name
// can be Chinese. Measured consequence of counting by byte:
// padRight("中文", 5) returns unchanged because len == 6 >= 5, but it actually only
// occupies 2 display columns, misaligning the whole row.
//
// Only does rune alignment, not East Asian double-width handling (a CJK rune takes
// two terminal columns) -- that would need an out-of-plan dependency like runewidth,
// and these three columns are ASCII-dominated anyway.
// Conclusion: a caller path containing CJK will still have skewed width, but will never produce invalid UTF-8.
func padRight(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// padCallerLeft right-aligns to w wide. When too long, truncates from the left and adds "…",
// preserving the line-number side -- locating code relies on the file name and line number, not the top-most directory.
//
// Truncation cuts on rune boundaries. Cutting by byte (s[len(s)-(w-1):]) would land
// mid-way through a multi-byte character, producing a U+FFFD replacement character
// on disk, and the resulting width wouldn't even be w.
func padCallerLeft(s string, w int) string {
	rs := []rune(s)
	if len(rs) > w {
		return "…" + string(rs[len(rs)-(w-1):])
	}
	return strings.Repeat(" ", w-len(rs)) + s
}

// stringify turns a field value into a string.
// Doesn't use strconv.Quote -- it would turn Chinese into \uXXXX, garbling Chinese log messages.
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	// zap.ByteString / zap.Binary / zap.Any([]byte) are all []byte in
	// MapObjectEncoder. Measured consequence of not special-casing this: prints [104 105] instead of hi.
	case []byte:
		return string(x)
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case nil:
		return "<nil>"
	default:
		return fmt.Sprint(v)
	}
}

// writeConsoleValue writes a value. Quotes and escapes it when it contains a space,
// quote, equals sign, or newline, guaranteeing one log entry always occupies exactly one line and can be grepped for the whole field.
func writeConsoleValue(b *buffer.Buffer, s string) {
	if s == "" {
		b.AppendString(`""`)
		return
	}
	if !strings.ContainsAny(s, " \t\r\n\"=") {
		b.AppendString(s)
		return
	}
	b.AppendByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.AppendString(`\"`)
		case '\\':
			b.AppendString(`\\`)
		case '\n':
			b.AppendString(`\n`)
		case '\r':
			b.AppendString(`\r`)
		case '\t':
			b.AppendString(`\t`)
		default:
			b.AppendRune(r)
		}
	}
	b.AppendByte('"')
}

// wantColor decides whether to colorize.
// The auto decision order: NO_COLOR not set → TERM isn't dumb → output is a TTY.
func wantColor(mode string, w io.Writer) bool {
	switch mode {
	case ColorAlways:
		return true
	case ColorNever:
		return false
	}
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -v
```

Expected: 全部 PASS。特别确认 `TestConsoleColorDoesNotBreakAlignment` 与 `TestConsoleKeepsCJKLiteral`。

- [ ] **Step 5: 加一个肉眼验收用例并跑一次**

自动化测试断言不了"看着舒服"。追加到 `log/console_test.go`：

```go
// TestConsoleDemo writes one line per level to stdout, for eyeballing alignment and coloring.
// Left in the repo long-term; run it on a whim after changing the encoder:
//   go test ./log/ -run TestConsoleDemo -v
//
// Visual acceptance can't be asserted, but "every line was actually produced" can --
// a look-only, no-assertion test case would pass green even while the encoder
// silently returns an empty string, failing to act as a guard.
func TestConsoleDemo(t *testing.T) {
	if testing.Short() {
		t.Skip("演示用例，-short 下跳过")
	}
	// Write to both stdout (for humans) and buf (for assertions) at the same time
	var buf bytes.Buffer
	sink := zapcore.NewMultiWriteSyncer(zapcore.Lock(os.Stdout), zapcore.AddSync(&buf))
	enc := newConsoleEncoder(true) // force coloring, so the effect is visible even outside a TTY
	core := zapcore.NewCore(enc, sink, zapcore.DebugLevel)
	l := zap.New(core, zap.AddCaller()).
		With(zap.String("trace_id", "01926f7e1a2b3c4d5e6f708192a3b4c5"))

	l.Debug("连接池已就绪", zap.Int("size", 10))
	l.Info("校验通过", zap.Int("order_id", 1001), zap.Float64("amount", 99.5))
	l.Warn("重试", zap.Int("attempt", 2), zap.Duration("backoff", 300*time.Millisecond))
	l.Error("支付失败", zap.Error(errors.New("connection refused")))
	l.Info("值里有空格", zap.String("reason", "余额 不足"))

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	require.Len(t, lines, 5, "五次调用要产出恰好五行")
	for i, ln := range lines {
		assert.NotEmpty(t, strings.TrimSpace(stripANSI(ln)), "第 %d 行不能是空的", i+1)
	}
	assert.Contains(t, buf.String(), "余额 不足", "中文原样保留")
	assert.Contains(t, buf.String(), `reason="余额 不足"`, "含空格的值要被引号包住")
	assert.NotContains(t, buf.String(), "trace_id=", "trace_id 已占固定列，不该再进 KV 区")
}
```

需要在 `console_test.go` 的 import 里补上 `"os"`（`bytes`、`errors`、`strings`、`time`、`unicode/utf8`、`require`、`zap`、`zapcore`、`testing` 已在）。

```bash
go test ./log/ -run TestConsoleDemo -v
```

**逐项确认：** 四列上下对齐；DEBUG 青 / INFO 绿 / WARN 黄 / ERROR 红；时间、trace、caller 暗灰；key 青；`error` 的值红；中文原样显示不是 `余`；`余额 不足` 被引号包住。

- [ ] **Step 6: Commit**

```bash
git add log/console.go log/console_test.go
git commit -m "feat(log): console encoder，对齐着色与 KV 渲染"
```

---

### Task 6: 按日滚动

**Files:**
- Create: `log/rotate.go`
- Test: `log/rotate_test.go`

**Interfaces:**
- Consumes: 无（只依赖 lumberjack）
- Produces:
  - `var nowFunc = time.Now` —— 全包共用的时间钩子，测试可替换（Task 8 的 Span 也用它）
  - `func setNow(f func() time.Time) (restore func())` —— 定义在 `rotate_test.go`，Task 8 的测试直接调
  - `type dailyRotator struct{...}`，实现 `io.Writer` + `Sync() error` + `Close() error`
  - `func newDailyRotator(lj *lumberjack.Logger) *dailyRotator`

**为什么排在 binding 之前：** Task 7 装配文件 sink 时要用它，依赖必须单向。

**实现取向：** lumberjack 只按大小滚，日期滚动得自己补 —— 但它的 `Rotate()` 是导出方法。所以只加"跨天检测 + 触发一次 `Rotate()`"这一件事，清理、压缩、backup 数量全部复用 lumberjack，代码量从两百行降到四十行。

**诚实说明（写进代码注释）：** 跨天滚出的归档文件名带的是**触发时刻**的时间戳（`app-2026-08-25T00-00-03.000.log`），而内容是**前一天**的。这是 lumberjack 的既定命名行为，不改；`max_age` 的计算也按这个时间戳走。

- [ ] **Step 1: 写失败测试**

创建 `log/rotate_test.go`：

```go
package log

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// setNow replaces the package-wide time hook, returning a restore function. Task 8's Span tests use it too.
func setNow(f func() time.Time) (restore func()) {
	old := nowFunc
	nowFunc = f
	return func() { nowFunc = old }
}

func TestDailyRotatorTriggersOnDayChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 23, 59, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 3, LocalTime: true})

	_, err := d.Write([]byte("day1\n"))
	require.NoError(t, err)

	fake = fake.Add(2 * time.Minute) // crosses midnight
	_, err = d.Write([]byte("day2\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "跨天后应有一个归档文件加一个当前文件")

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "day2\n", string(cur), "当前文件只含跨天之后的内容")
}

func TestDailyRotatorDoesNotRotateWithinSameDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 8, 0, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, LocalTime: true})

	_, err := d.Write([]byte("a\n"))
	require.NoError(t, err)
	fake = fake.Add(13 * time.Hour) // still within the same day, spanning most of it
	_, err = d.Write([]byte("b\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "同一天内不滚")

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "a\nb\n", string(cur))
}

func TestDailyRotatorConcurrentWritesRotateOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 23, 59, 59, 0, time.Local)
	var mu sync.Mutex
	defer setNow(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fake
	})()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 10, LocalTime: true})
	_, err := d.Write([]byte("seed\n"))
	require.NoError(t, err)

	mu.Lock()
	fake = fake.Add(time.Second) // every goroutine sees the day change at the same time
	mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Write([]byte("x\n"))
		}()
	}
	wg.Wait()
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "32 个并发写只该触发一次滚动")
}

func TestDailyRotatorSyncIsNoop(t *testing.T) {
	d := newDailyRotator(&lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "a.log")})
	assert.NoError(t, d.Sync(), "lumberjack 不缓冲，Sync 无事可做但必须存在以满足 WriteSyncer")
	assert.NoError(t, d.Close())
}
```

并发用例记得开竞态检测跑。

**再补两条测试（探针实测所得）：**

```go
func TestDailyRotatorSkipsRotateOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, nil, 0o600)) // a 0-byte current file

	fake := time.Date(2026, 8, 24, 23, 59, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 3, LocalTime: true})
	fake = fake.Add(2 * time.Minute) // crosses midnight
	_, err := d.Write([]byte("day2\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "空文件跨天不该滚出 0 字节归档")
}

func TestDailyRotatorCreatesDirWith0750(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	d := newDailyRotator(&lumberjack.Logger{Filename: filepath.Join(dir, "app.log")})
	defer d.Close()

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	// [SEC-INFO] lumberjack creates the directory itself as 0755; we must preempt it and create it as 0750
	assert.Equal(t, os.FileMode(0o750), fi.Mode().Perm(), "日志目录权限")
}
```

**测试注意（实测踩过）：** lumberjack 的 `MaxBackups` 清理由后台 `millRun` goroutine 做，**不保证 `Rotate()` 返回时已完成**。凡是断言 backup 文件数量的用例，必须轮询等待（例如 `require.Eventually`，1.5 秒超时、50ms 间隔），直接断言会 flaky。上面几个用例断言的是"滚了 / 没滚"而不是清理后的数量，不受影响。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'Daily' -v
```

Expected: 编译失败，`undefined: newDailyRotator` / `undefined: nowFunc`。

- [ ] **Step 3: 写实现**

创建 `log/rotate.go`：

```go
package log

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// nowFunc is the package-wide time hook; it is only replaced in tests.
var nowFunc = time.Now

// dailyRotator adds daily rotation on top of lumberjack.
//
// lumberjack itself only rotates by file size, but its Rotate() is exported --
// so all we do here is one thing: check before each write whether the day has
// changed, and if so, trigger one Rotate(). Cleanup, compression, and backup
// count are all handled by lumberjack according to its existing config.
//
// Known behavior: the timestamp in an archive file rotated at day-crossing is
// the trigger moment's timestamp (app-2026-08-25T00-00-03.000.log), while the
// content is the previous day's. This is lumberjack's established naming
// convention and is not changed; max_age is also computed against this timestamp.
type dailyRotator struct {
	lj *lumberjack.Logger

	mu  sync.Mutex
	day string
}

func newDailyRotator(lj *lumberjack.Logger) *dailyRotator {
	// [SEC-INFO] The log directory must be 0750. lumberjack hardcodes 0755 when it
	// creates the directory itself (measured: -rwxr-xr-x), which violates our security
	// policy, so we create it first: os.MkdirAll returns nil for an existing directory
	// without touching its permissions, which makes lumberjack's later call a no-op.
	// The error is ignored here -- if the directory truly cannot be created, lumberjack
	// will surface the real reason on its first write.
	_ = os.MkdirAll(filepath.Dir(lj.Filename), 0o750)
	return &dailyRotator{lj: lj, day: nowFunc().Format(time.DateOnly)}
}

func (d *dailyRotator) Write(p []byte) (int, error) {
	d.mu.Lock()
	if today := nowFunc().Format(time.DateOnly); today != d.day {
		d.day = today
		// Skip rotation for an empty file: Rotate() on a 0-byte current file still
		// produces a 0-byte archive (measured), and a process's first start on a new
		// day would otherwise rotate out this kind of junk, accumulating over time.
		if fi, err := os.Stat(d.lj.Filename); err != nil || fi.Size() > 0 {
			// A failed rotation must not block the write: a log that fails to rotate
			// is an ops problem, but a lost log entry is an incident.
			_ = d.lj.Rotate()
		}
	}
	d.mu.Unlock()

	// lumberjack.Write has its own lock, placed outside d.mu -- don't widen the
	// lock's scope to cover the entire disk write.
	return d.lj.Write(p)
}

// Sync satisfies zapcore.WriteSyncer. lumberjack writes directly to the fd
// without buffering, so there is nothing to do.
//
// Note that lumberjack.Logger has **no** Sync method (its method set only has
// Close/Rotate/Write) -- don't forward to a method that doesn't exist.
func (d *dailyRotator) Sync() error { return nil }

func (d *dailyRotator) Close() error { return d.lj.Close() }
```

`time.DateOnly` 是 Go 1.20 引入的 `"2006-01-02"` 常量。

- [ ] **Step 4: 跑测试确认通过（带竞态检测）**

```bash
go test ./log/ -race -v
```

Expected: 全部 PASS，无 DATA RACE 报告。

- [ ] **Step 5: Commit**

```bash
git add log/rotate.go log/rotate_test.go
git commit -m "feat(log): 给 lumberjack 补按日滚动"
```

---

### Task 7: zap binding

**Files:**
- Create: `log/zap.go`
- Test: `log/zap_test.go`

**Interfaces:**
- Consumes: `Level` / `Logger` / `ZapProvider` / `CallerSkipper` / `Nop`（Task 1）、`Trace` / `TraceFrom`（Task 2）、`Config` / `FormatJSON` / `RotateDaily`（Task 3）、`newMasker` / `newMaskCore`（Task 4）、`newConsoleEncoder` / `wantColor`（Task 5）、`newDailyRotator`（Task 6）
- Produces:
  - `func Init(cfg Config) error`
  - `func SetLogger(l Logger)`
  - `func L() Logger`
  - `func Ctx(ctx context.Context) Logger`
  - `func NewContext(ctx context.Context, l Logger) context.Context`
  - `func Sync() error`
  - `func Close() error`
  - `func Zap(ctx context.Context) (*zap.Logger, bool)`
  - `func newZapLogger(raw *zap.Logger) *zapLogger` —— Task 8、9 的测试用它构造后端
  - `func traceKV(t Trace) []any` —— Task 8、10 用
  - `func toFields(kv []any) []zap.Field`
  - `const badKeyName = "!BADKEY"`

**三个必须写对的地方：**

1. **两个 caller skip 实例。** `zapLogger` 同时持有 `raw`（未加门面 skip）与 `z`（`raw` + `AddCallerSkip(1)`）。门面方法走 `z`，`Zap()` 逃生舱口返回 `raw`。gfa 用单一 `AddCallerSkip(2)` 又把 `*Logger` 暴露出去，拿到手直接调方法时 caller 就指进 logger.go 内部 —— 这里用两个实例根治。

2. **Core 的装配顺序：`sampler → tee → [mask(console), mask(file), mask(error)]`。** 每个叶子 sink 各包一层 maskCore，mask 在 Tee 之内；sampler 在最外。

   两个包裹关系都不能颠倒，而且**颠倒之后是静默失效，不报任何错**：

   - mask 若包在 Tee 之外，`maskCore.Check` 会把自己挂进 CheckedEntry，`multiCore.Check`（`zapcore/tee.go:74-79` —— per-sink 级别过滤的唯一发生地）便再也不会执行，`error_path` 收到全量日志。`TestInitErrorPathOnlyReceivesErrors` 守这条。
   - sampler 若包在 mask 之内，`sampler.Check`（`zapcore/sampler.go:214-229` —— 采样的唯一发生地）被同样的机制短路，`log.sampling` 完全失效。`TestSamplingWrapsOutsideMaskCore` 守这条。

   脱敏语义一字不减：每条 Write 仍然经过 maskCore，`Zap()` 逃生舱口拿到的 logger 同样被覆盖。sampler 在最外还顺带保住了「被丢弃的日志连脱敏开销都省掉」。

3. **`L()` 用 `atomic.Pointer[Logger]` 而不是 `atomic.Value`。** 后者要求每次 `Store` 的动态类型一致，`SetLogger` 换后端时会直接 panic。

- [ ] **Step 1: 写失败测试**

创建 `log/zap_test.go`：

```go
package log

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// installObserver swaps the global backend for an in-memory observer and
// restores it automatically when the test ends.
func installObserver(t *testing.T, opts ...zap.Option) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	SetLogger(newZapLogger(zap.New(core, opts...)))
	t.Cleanup(func() { SetLogger(Nop()) })
	return logs
}

func TestToFields(t *testing.T) {
	fs := toFields([]any{"a", 1, "b", "x"})
	require.Len(t, fs, 2)
	assert.Equal(t, "a", fs[0].Key)
	assert.Equal(t, "b", fs[1].Key)

	assert.Nil(t, toFields(nil))
	assert.Nil(t, toFields([]any{}))
}

func TestToFieldsOddCountProducesBadKey(t *testing.T) {
	fs := toFields([]any{"a", 1, "dangling"})
	require.Len(t, fs, 2)
	assert.Equal(t, "a", fs[0].Key)
	assert.Equal(t, badKeyName, fs[1].Key, "落单的参数进 !BADKEY，不能静默吞掉")
}

// A call migrated from gfa's Sprintln semantics will hit this case.
func TestToFieldsNonStringKeyProducesBadKey(t *testing.T) {
	fs := toFields([]any{1001, 99.5}) // the kv portion of TInfo(ctx, "支付", id, amt)
	require.Len(t, fs, 2)
	assert.Equal(t, badKeyName, fs[0].Key)
	assert.Equal(t, badKeyName, fs[1].Key)
}

func TestToFieldsRealignsAfterBadKey(t *testing.T) {
	fs := toFields([]any{42, "user", "alice"})
	require.Len(t, fs, 2)
	assert.Equal(t, badKeyName, fs[0].Key, "42 当不了 key")
	assert.Equal(t, "user", fs[1].Key, "后面的合法 KV 必须重新对齐")
}

func TestFacadeCallerPointsToCallSite(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	_, _, line, _ := runtime.Caller(0)
	L().Info("hi") // line + 1

	require.Len(t, logs.All(), 1)
	e := logs.All()[0]
	assert.Equal(t, line+1, e.Caller.Line, "caller 必须指向业务代码，不是 zap.go")
	assert.Contains(t, e.Caller.File, "zap_test.go")
}

// Zap() must return the instance without the facade's caller skip, otherwise
// the escape hatch's caller would be off by one frame.
func TestZapEscapeHatchReturnsRawLogger(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	z, ok := Zap(context.Background())
	require.True(t, ok)

	_, _, line, _ := runtime.Caller(0)
	z.Info("raw") // line + 1

	require.Len(t, logs.All(), 1)
	assert.Equal(t, line+1, logs.All()[0].Caller.Line)
}

func TestZapEscapeHatchFalseForNonZapBackend(t *testing.T) {
	SetLogger(Nop())
	t.Cleanup(func() { SetLogger(Nop()) })

	z, ok := Zap(context.Background())
	assert.False(t, ok)
	assert.Nil(t, z)
}

func TestWithCallerSkipCachesSkipOne(t *testing.T) {
	l := newZapLogger(zap.NewNop())
	a := l.WithCallerSkip(1)
	b := l.WithCallerSkip(1)
	assert.Same(t, a, b, "skip+1 是 T 系列热路径，必须缓存复用")
	assert.NotSame(t, a, l.WithCallerSkip(2))
}

func TestCtxFastPathReturnsStoredLogger(t *testing.T) {
	installObserver(t)
	stored := L().With("svc", "order")
	ctx := NewContext(context.Background(), stored)
	assert.Same(t, stored, Ctx(ctx), "存过 logger 就直接取，不再派生")
}

func TestCtxSlowPathDerivesFromTrace(t *testing.T) {
	logs := installObserver(t)

	tr := NewTrace("http.request")
	Ctx(WithTrace(context.Background(), tr)).Info("hit")

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, tr.TraceID().String(), m["trace_id"])
	assert.Equal(t, tr.SpanID().String(), m["span_id"])
	assert.Equal(t, tr.RequestID, m["request_id"])
	assert.Equal(t, "http.request", m["span_name"])
}

func TestCtxFallsBackToGlobal(t *testing.T) {
	installObserver(t)
	assert.NotPanics(t, func() {
		Ctx(context.Background()).Info("no trace")
		Ctx(nil).Info("nil ctx") //nolint:staticcheck
	})
}

func TestTraceKVSkipsZeroValues(t *testing.T) {
	kv := traceKV(Trace{})
	assert.Empty(t, kv, "零值 Trace 不该产出任何字段")
}

func TestEnabledReflectsCoreLevel(t *testing.T) {
	core, _ := observer.New(zapcore.WarnLevel)
	l := newZapLogger(zap.New(core))
	assert.False(t, l.Enabled(InfoLevel))
	assert.True(t, l.Enabled(WarnLevel))
	assert.True(t, l.Enabled(ErrorLevel))
}

func TestWithEmptyKVReturnsSameLogger(t *testing.T) {
	l := newZapLogger(zap.NewNop())
	assert.Same(t, Logger(l), l.With(), "空 With 不该白白克隆一个 logger")
}

// ── Init assembly ────────────────────────────────────────

func TestInitWithNoSinkYieldsNop(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { SetLogger(Nop()) })

	assert.False(t, L().Enabled(ErrorLevel))
	assert.NotPanics(t, func() { L().Error("boom") })
}

func TestInitRejectsBadConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Level = "verbose"
	assert.Error(t, Init(cfg))
}

func TestInitFileSinkWritesJSONLWithMasking(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.jsonl") // extension implies json
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("下单", "order_id", 1001, "password", "hunter2")
	require.NoError(t, Close())

	data, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(data), &m))
	assert.Equal(t, "下单", m["msg"])
	assert.EqualValues(t, 1001, m["order_id"])
	assert.Equal(t, maskPlaceholder, m["password"], "脱敏必须覆盖文件 sink")
	assert.Contains(t, m, "caller")
	assert.Contains(t, m, "ts")
	assert.Equal(t, "info", m["level"])
}

func TestInitErrorPathOnlyReceivesErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	cfg.File.ErrorPath = filepath.Join(dir, "error.jsonl")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("普通信息")
	L().Error("出事了", "password", "hunter2")
	require.NoError(t, Close())

	all, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Contains(t, string(all), "普通信息")
	assert.Contains(t, string(all), "出事了")

	errOnly, err := os.ReadFile(cfg.File.ErrorPath)
	require.NoError(t, err)
	assert.NotContains(t, string(errOnly), "普通信息", "error sink 只收 error 及以上")
	assert.Contains(t, string(errOnly), "出事了")

	// Every leaf sink is wrapped in its own maskCore layer -- missing any one of
	// them would be exposed right here.
	assert.NotContains(t, string(all), "hunter2", "app sink 必须脱敏")
	assert.NotContains(t, string(errOnly), "hunter2", "error sink 必须同样脱敏")
	assert.Contains(t, string(errOnly), maskPlaceholder)
}

// Regression test: the sampler must be wrapped outside maskCore (i.e., outside
// the Tee).
//
// If the order were reversed to newMaskCore(sampler), maskCore.Check would
// hang itself onto the CheckedEntry, and sampler.Check
// (zapcore/sampler.go:214-229 -- the only place sampling actually happens)
// would never run again, silently disabling log.sampling: all 5 duplicate log
// lines would be written to disk instead of just 1.
func TestSamplingWrapsOutsideMaskCore(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.jsonl")
	cfg.Sampling.Initial = 1
	cfg.Sampling.Thereafter = 0 // drop everything after the first in the window
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	for i := 0; i < 5; i++ {
		L().Info("重复消息")
	}
	require.NoError(t, Close())

	b, err := os.ReadFile(cfg.File.Path)
	require.NoError(t, err)
	assert.Equal(t, 1, bytes.Count(b, []byte("重复消息")),
		"采样必须生效：5 条相同消息只应落盘 1 条")
}

func TestInitCreatesMissingLogDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deep", "nested")
	cfg := DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = true
	cfg.File.Path = filepath.Join(dir, "app.log")
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	L().Info("x")
	require.NoError(t, Close())
	assert.FileExists(t, cfg.File.Path)
}

func TestSyncOnStdoutIsNotAnError(t *testing.T) {
	cfg := DefaultConfig() // console is on, points to stdout
	require.NoError(t, Init(cfg))
	t.Cleanup(func() { _ = Close(); SetLogger(Nop()) })

	assert.NoError(t, Sync(), "对终端/管道 fsync 会返回 EINVAL，那不是错误")
}

// ── SetLogger's safety warning ────────────────────────────

type fakeBackend struct{ Logger }

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	old := os.Stderr
	os.Stderr = w

	fn()

	os.Stderr = old
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

func TestSetLoggerWarnsWhenBackendReplaced(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })

	out := captureStderr(t, func() { SetLogger(fakeBackend{Nop()}) })
	assert.Contains(t, out, "脱敏", "换后端等于换掉内置脱敏，必须显式警示")
}

func TestSetLoggerSilentForDefaultAndNop(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })

	out := captureStderr(t, func() {
		SetLogger(Nop())
		SetLogger(newZapLogger(zap.NewNop()))
	})
	assert.Empty(t, out, "默认 binding 与显式禁用都不该刷警告")
}

func TestSetLoggerNilFallsBackToNop(t *testing.T) {
	t.Cleanup(func() { SetLogger(Nop()) })
	assert.NotPanics(t, func() {
		SetLogger(nil)
		L().Info("still fine")
	})
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'ToFields|Caller|Ctx|Init|Sync|SetLogger|Enabled|WithEmpty|TraceKV' -v
```

Expected: 编译失败，`undefined: Init` / `undefined: newZapLogger`。

- [ ] **Step 3: 写实现**

创建 `log/zap.go`：

```go
package log

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// badKeyName holds invalid KV arguments. Kept consistent with log/slog's
// convention.
const badKeyName = "!BADKEY"

type loggerKey struct{}

var (
	// atomic.Pointer instead of atomic.Value -- the latter requires the dynamic
	// type to be identical on every Store, and SetLogger swapping backends
	// would panic outright.
	global atomic.Pointer[Logger]

	// closerSlot holds the close function for the current sink; Init calls it
	// when the config is swapped, and Close calls it on shutdown.
	closerSlot atomic.Pointer[[]func() error]
)

// ── Facade implementation ─────────────────────────────────

// zapLogger is the default binding.
//
// It holds two zap instances so the caller frame points to the right place:
//   - raw: without the facade's caller skip; Zap()'s escape hatch returns it,
//     so the caller is correct when a caller invokes raw.Info directly
//   - z: raw + AddCallerSkip(1); facade methods go through it, skipping the
//     zapLogger.Info frame itself
type zapLogger struct {
	raw *zap.Logger
	z   *zap.Logger

	once sync.Once
	next *zapLogger // lazily built skip+1 version, used by the T-series sugar
}

func newZapLogger(raw *zap.Logger) *zapLogger {
	return &zapLogger{raw: raw, z: raw.WithOptions(zap.AddCallerSkip(1))}
}

func (l *zapLogger) Debug(msg string, kv ...any) { l.z.Debug(msg, toFields(kv)...) }
func (l *zapLogger) Info(msg string, kv ...any)  { l.z.Info(msg, toFields(kv)...) }
func (l *zapLogger) Warn(msg string, kv ...any)  { l.z.Warn(msg, toFields(kv)...) }
func (l *zapLogger) Error(msg string, kv ...any) { l.z.Error(msg, toFields(kv)...) }

func (l *zapLogger) With(kv ...any) Logger {
	if len(kv) == 0 {
		return l
	}
	return newZapLogger(l.raw.With(toFields(kv)...))
}

func (l *zapLogger) Enabled(lv Level) bool {
	return l.raw.Core().Enabled(zapcore.Level(lv))
}

// Zap implements ZapProvider. Returns the instance without the facade's
// caller skip.
func (l *zapLogger) Zap() *zap.Logger { return l.raw }

// WithCallerSkip implements CallerSkipper.
func (l *zapLogger) WithCallerSkip(n int) Logger {
	if n != 1 {
		return newZapLogger(l.raw.WithOptions(zap.AddCallerSkip(n)))
	}
	// skip+1 is the T-series' hot path; cache it so we don't clone the logger
	// on every call.
	l.once.Do(func() {
		l.next = &zapLogger{
			raw: l.raw.WithOptions(zap.AddCallerSkip(1)),
			z:   l.z.WithOptions(zap.AddCallerSkip(1)),
		}
	})
	return l.next
}

// toFields converts a KV sequence into zap.Field values.
//
// Produces a !BADKEY field when the key isn't a string or an argument is
// left dangling, without panicking or silently swallowing it.
// A call migrated from gfa's Sprintln semantics (TInfo(ctx, "支付", id, amt),
// where id is an int) turns into two !BADKEY fields here, immediately
// visible.
func toFields(kv []any) []zap.Field {
	if len(kv) == 0 {
		return nil
	}
	fs := make([]zap.Field, 0, (len(kv)+1)/2)
	for i := 0; i < len(kv); {
		if i == len(kv)-1 { // the last, dangling item
			fs = append(fs, zap.Any(badKeyName, kv[i]))
			break
		}
		k, ok := kv[i].(string)
		if !ok { // the key position isn't a string: record it separately, retry the next item as a key
			fs = append(fs, zap.Any(badKeyName, kv[i]))
			i++
			continue
		}
		fs = append(fs, zap.Any(k, kv[i+1]))
		i += 2
	}
	return fs
}

// ── Global binding ─────────────────────────────────────────

// L returns the global Logger. Before Init, returns Nop, so logging never
// panics.
func L() Logger {
	if p := global.Load(); p != nil {
		return *p
	}
	return Nop()
}

// SetLogger replaces the global backend, corresponding to an SLF4J binding
// swap.
//
// Security note: replacing the default backend also replaces the built-in
// sensitive-field masking -- that's implemented in this package's maskCore,
// and a third-party implementation won't automatically carry it over. This
// warns explicitly once.
func SetLogger(l Logger) {
	if l == nil {
		l = Nop()
	}
	global.Store(&l)

	switch l.(type) {
	case *zapLogger, nopLogger:
		// The default binding, or explicitly disabling logging, needs no warning
	default:
		fmt.Fprintln(os.Stderr,
			"xbc/log: 已替换默认日志后端，内置敏感字段脱敏随之失效 —— "+
				"请确认新后端自行实现了脱敏，否则 password/token 等字段会明文落盘")
	}
}

// NewContext binds a Logger into ctx.
func NewContext(ctx context.Context, l Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, loggerKey{}, l)
}

// Ctx retrieves the Logger stored on ctx.
//
// Fast path: entry middleware uses NewContext to store a logger already
// carrying trace fields; this retrieves it directly with zero allocations.
// Slow path: when only a Trace is present with no Logger, one is derived on
// the spot -- every call allocates, so the framework's entry middleware
// should always go through NewContext.
// If neither is present, falls back to the global Logger without panicking.
func Ctx(ctx context.Context) Logger {
	if ctx == nil {
		return L()
	}
	if l, ok := ctx.Value(loggerKey{}).(Logger); ok && l != nil {
		return l
	}
	if t := TraceFrom(ctx); t.Valid() {
		return L().With(traceKV(t)...)
	}
	return L()
}

// traceKV unrolls a trace into a KV sequence. Zero-value fields are omitted.
func traceKV(t Trace) []any {
	kv := make([]any, 0, 8)
	if t.TraceID().IsValid() {
		kv = append(kv, "trace_id", t.TraceID().String())
	}
	if t.SpanID().IsValid() {
		kv = append(kv, "span_id", t.SpanID().String())
	}
	if t.SpanName != "" {
		kv = append(kv, "span_name", t.SpanName)
	}
	if t.RequestID != "" {
		kv = append(kv, "request_id", t.RequestID)
	}
	return kv
}

// Zap returns the underlying *zap.Logger for zap-specific operations. ok is
// false when the backend isn't zap.
//
// The escape hatch is protected by masking too -- maskCore is part of this
// logger's composition and cannot be bypassed.
func Zap(ctx context.Context) (*zap.Logger, bool) {
	if zp, ok := Ctx(ctx).(ZapProvider); ok {
		return zp.Zap(), true
	}
	return nil, false
}

// ── Assembly ───────────────────────────────────────────────

// Init assembles the default zap backend from the config and sets it as
// global. Calling it repeatedly closes the previously opened file sink first.
func Init(cfg Config) error {
	if err := cfg.Normalize(); err != nil {
		return err
	}
	lv, err := ParseLevel(cfg.Level)
	if err != nil {
		return err
	}

	var (
		cores []zapcore.Core
		cls   []func() error
	)

	// The three sinks share the same masker, each wrapped in its own maskCore
	// layer (see the NewTee call below for why).
	m := newMasker(cfg.MaskFields)

	if cfg.Console.Enabled {
		cores = append(cores, newMaskCore(zapcore.NewCore(
			buildEncoder(cfg.Console.Format, wantColor(cfg.Console.Color, os.Stdout)),
			zapcore.Lock(os.Stdout),
			zapcore.Level(lv),
		), m))
	}

	if cfg.File.Enabled {
		w, closeFn, err := buildFileWriter(cfg.File, cfg.File.Path)
		if err != nil {
			closeAll(cls)
			return err
		}
		cls = append(cls, closeFn)
		cores = append(cores, newMaskCore(zapcore.NewCore(
			buildEncoder(cfg.File.Format, false), w, zapcore.Level(lv)), m))

		if cfg.File.ErrorPath != "" {
			ew, ecloseFn, err := buildFileWriter(cfg.File, cfg.File.ErrorPath)
			if err != nil {
				closeAll(cls)
				return err
			}
			cls = append(cls, ecloseFn)
			cores = append(cores, newMaskCore(zapcore.NewCore(
				buildEncoder(cfg.File.errorFormat, false), ew, zapcore.ErrorLevel), m))
		}
	}

	if len(cores) == 0 { // everything off is equivalent to Nop, common in test environments
		closeAll(swapClosers(nil))
		SetLogger(Nop())
		return nil
	}

	// Masking has already been wrapped per sink; this only fans out.
	//
	// Never reverse this by wrapping maskCore outside the Tee: maskCore.Check
	// would hang itself onto the CheckedEntry, so multiCore.Check
	// (zapcore/tee.go:74-79 -- the only place per-sink filtering happens)
	// would never run, and multiCore.Write would then unconditionally write
	// into every child core, turning error_path into a complete copy of
	// app.log.
	core := zapcore.NewTee(cores...)

	// Sampling must be at the outermost layer, wrapped outside maskCore --
	// sampler.Check (zapcore/sampler.go:214-229) is the only place sampling
	// happens; if it were short-circuited by maskCore.Check, log.sampling
	// would silently stop working. Side benefit: dropped logs also skip the
	// masking overhead.
	if cfg.Sampling.Initial > 0 {
		core = zapcore.NewSamplerWithOptions(core, time.Second,
			cfg.Sampling.Initial, cfg.Sampling.Thereafter)
	}

	opts := []zap.Option{zap.ErrorOutput(zapcore.Lock(os.Stderr))}
	if cfg.Caller {
		opts = append(opts, zap.AddCaller())
	}
	if st, err := ParseLevel(cfg.Stacktrace); err == nil {
		opts = append(opts, zap.AddStacktrace(zapcore.Level(st)))
	}

	closeAll(swapClosers(cls)) // close the previous round's sinks first, then hook up the new ones
	SetLogger(newZapLogger(zap.New(core, opts...)))
	return nil
}

func buildEncoder(format string, color bool) zapcore.Encoder {
	if format == FormatJSON {
		return zapcore.NewJSONEncoder(jsonEncoderConfig())
	}
	return newConsoleEncoder(color)
}

func jsonEncoderConfig() zapcore.EncoderConfig {
	c := zap.NewProductionEncoderConfig()
	c.TimeKey = "ts"
	c.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	c.LevelKey = "level"
	c.EncodeLevel = zapcore.LowercaseLevelEncoder
	c.MessageKey = "msg"
	c.CallerKey = "caller"
	c.EncodeCaller = zapcore.ShortCallerEncoder
	c.StacktraceKey = "stack"
	c.EncodeDuration = zapcore.MillisDurationEncoder
	return c
}

func buildFileWriter(cfg FileConfig, path string) (zapcore.WriteSyncer, func() error, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// 0o750 instead of 0o755: the log directory shouldn't be readable by
		// other users
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, nil, fmt.Errorf("log: 创建日志目录 %s 失败: %w", dir, err)
		}
	}
	lj := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    cfg.MaxSize,
		MaxAge:     cfg.MaxAge,
		MaxBackups: cfg.MaxBackups,
		Compress:   cfg.Compress,
		LocalTime:  true,
	}
	if cfg.Rotate == RotateDaily {
		d := newDailyRotator(lj)
		return zapcore.AddSync(d), d.Close, nil
	}
	return zapcore.AddSync(lj), lj.Close, nil
}

func swapClosers(next []func() error) []func() error {
	old := closerSlot.Swap(&next)
	if old == nil {
		return nil
	}
	return *old
}

func closeAll(fns []func() error) {
	for _, fn := range fns {
		if fn != nil {
			_ = fn()
		}
	}
}

// Sync flushes to disk. Call it before the process exits, typically via
// defer.
func Sync() error {
	zp, ok := L().(ZapProvider)
	if !ok {
		return nil
	}
	if err := zp.Zap().Sync(); err != nil && !isBenignSyncError(err) {
		return err
	}
	return nil
}

// Close flushes to disk and closes all file sinks. The framework calls this
// as the last step of a graceful shutdown.
func Close() error {
	err := Sync()
	closeAll(swapClosers(nil))
	return err
}

// isBenignSyncError recognizes the normal failure of calling Sync on
// stdout/stderr. Terminals and pipes aren't fsync-able objects, so the kernel
// returns EINVAL/ENOTTY -- this isn't a real error, just a well-known zap
// wart, and shouldn't make a deferred log.Sync() report an error on every
// exit.
func isBenignSyncError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTTY) ||
		errors.Is(err, syscall.EBADF) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid argument") ||
		strings.Contains(s, "inappropriate ioctl") ||
		strings.Contains(s, "bad file descriptor")
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -race -v
```

Expected: 全部 PASS。重点确认 `TestFacadeCallerPointsToCallSite`、`TestZapEscapeHatchReturnsRawLogger`、`TestInitFileSinkWritesJSONLWithMasking`。

- [ ] **Step 5: Commit**

```bash
git add log/zap.go log/zap_test.go
git commit -m "feat(log): zap binding，双 caller skip 与 sink 装配"
```

---

### Task 8: `Span`

**Files:**
- Modify: `log/trace.go`（在 Task 2 写的内容后面追加）
- Modify: `log/trace_test.go`（追加）

**Interfaces:**
- Consumes: `Trace` / `Fork` / `NewTrace` / `WithTrace` / `TraceFrom`（Task 2）、`nowFunc`（Task 6）、`setNow`（Task 6 的 `rotate_test.go`）、`L` / `NewContext` / `traceKV` / `CallerSkipper`（Task 7）、`installObserver`（Task 7 的 `zap_test.go`）
- Produces:
  - `type SpanOption func(*spanOptions)`
  - `func Span(ctx context.Context, name string) (context.Context, func())`
  - `func SpanWith(ctx context.Context, name string, opts ...SpanOption) (context.Context, func())`
  - `func SpanID(id trace.SpanID) SpanOption`

**闭包里的 caller：** 结束回调比正常调用多一层（闭包本身），所以回调里要 `WithCallerSkip(1)`，让 `span 结束` 这条日志的 caller 指向 `defer done()` 所在的函数，而不是 trace.go。

- [ ] **Step 1: 写失败测试**

追加到 `log/trace_test.go`（import 需补 `"time"`、`"github.com/stretchr/testify/require"` 若尚未引入）：

```go
func TestSpanForksFromParent(t *testing.T) {
	root := NewTrace("http.request")
	ctx := WithTrace(context.Background(), root)

	ctx2, done := Span(ctx, "db.query")
	defer done()

	child := TraceFrom(ctx2)
	assert.Equal(t, root.TraceID(), child.TraceID(), "trace_id 必须继承")
	assert.NotEqual(t, root.SpanID(), child.SpanID(), "span_id 必须是新的")
	assert.Equal(t, root.SpanID(), child.ParentSpanID)
	assert.Equal(t, "db.query", child.SpanName)
	assert.Equal(t, root.RequestID, child.RequestID)
}

func TestSpanWithoutParentStartsNewTrace(t *testing.T) {
	ctx, done := Span(context.Background(), "cron.cleanup")
	defer done()

	tr := TraceFrom(ctx)
	assert.True(t, tr.Valid())
	assert.False(t, tr.ParentSpanID.IsValid(), "没有上游就是根 span")
	assert.Equal(t, "cron.cleanup", tr.SpanName)
}

func TestSpanBindsLoggerIntoContext(t *testing.T) {
	logs := installObserver(t)

	ctx, done := Span(context.Background(), "svc.call")
	Ctx(ctx).Info("span 内部")
	done()

	require.GreaterOrEqual(t, len(logs.All()), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, TraceFrom(ctx).TraceID().String(), m["trace_id"])
	assert.Equal(t, "svc.call", m["span_name"])
}

func TestSpanIDOptionOverridesGenerated(t *testing.T) {
	want := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	ctx, done := SpanWith(context.Background(), "custom", SpanID(want))
	defer done()
	assert.Equal(t, want, TraceFrom(ctx).SpanID())
}

func TestSpanIDOptionIgnoresZeroValue(t *testing.T) {
	ctx, done := SpanWith(context.Background(), "x", SpanID(trace.SpanID{}))
	defer done()
	assert.True(t, TraceFrom(ctx).SpanID().IsValid(), "零值 span_id 不该覆盖掉生成的那个")
}

func TestSpanLogsElapsedOnDone(t *testing.T) {
	logs := installObserver(t)

	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.Local)
	fake := base
	defer setNow(func() time.Time { return fake })()

	_, done := Span(context.Background(), "slow.op")
	fake = base.Add(1500 * time.Millisecond)
	done()

	require.Len(t, logs.All(), 1)
	e := logs.All()[0]
	assert.Equal(t, zapcore.DebugLevel, e.Level)
	assert.Equal(t, "span 结束", e.Message)
	assert.Equal(t, "slow.op", e.ContextMap()["span_name"])
	assert.InDelta(t, 1500.0, e.ContextMap()["elapsed_ms"], 0.001)
}

func TestSpanNestsThreeLevels(t *testing.T) {
	c1, d1 := Span(context.Background(), "a")
	c2, d2 := Span(c1, "b")
	c3, d3 := Span(c2, "c")
	d3()
	d2()
	d1()

	t1, t2, t3 := TraceFrom(c1), TraceFrom(c2), TraceFrom(c3)
	assert.Equal(t, t1.TraceID(), t3.TraceID(), "三层共用一个 trace_id")
	assert.Equal(t, t1.SpanID(), t2.ParentSpanID)
	assert.Equal(t, t2.SpanID(), t3.ParentSpanID)
}
```

`trace_test.go` 的 import 需要补 `"go.uber.org/zap/zapcore"` 和 `"time"`。

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'Span' -v
```

Expected: 编译失败，`undefined: Span`。

- [ ] **Step 3: 写实现**

追加到 `log/trace.go` 末尾（import 需补 `"time"`）：

```go
// ── Span ─────────────────────────────────────────────────

type spanOptions struct {
	spanID trace.SpanID
}

// SpanOption adjusts how a span is created.
type SpanOption func(*spanOptions)

// SpanID sets the span_id, used to align with an external system. A zero
// value is ignored.
func SpanID(id trace.SpanID) SpanOption {
	return func(o *spanOptions) { o.spanID = id }
}

// Span opens a child span, returning a ctx carrying the trace and a done
// callback.
//
//	ctx, done := log.Span(ctx, "db.query")
//	defer done()
//
// The ctx already has a Logger bound to it with trace fields attached, so
// subsequent log.TInfo(ctx, ...) calls retrieve it with zero allocations.
func Span(ctx context.Context, name string) (context.Context, func()) {
	return SpanWith(ctx, name)
}

// SpanWith is the option-taking version of Span.
func SpanWith(ctx context.Context, name string, opts ...SpanOption) (context.Context, func()) {
	var o spanOptions
	for _, fn := range opts {
		fn(&o)
	}

	var child Trace
	if parent := TraceFrom(ctx); parent.Valid() {
		child = parent.Fork(name)
	} else {
		child = NewTrace(name)
	}
	if o.spanID.IsValid() {
		child.SpanContext = child.SpanContext.WithSpanID(o.spanID)
	}

	l := L().With(traceKV(child)...)
	ctx = NewContext(WithTrace(ctx, child), l)

	start := nowFunc()
	return ctx, func() {
		done := l
		// The callback adds one extra closure frame vs. a normal call; skip one
		// more so the caller points to the function containing defer done()
		if cs, ok := done.(CallerSkipper); ok {
			done = cs.WithCallerSkip(1)
		}
		done.Debug("span 结束", "elapsed_ms",
			float64(nowFunc().Sub(start).Microseconds())/1000)
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -race -v
```

Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add log/trace.go log/trace_test.go
git commit -m "feat(log): Span 与耗时统计"
```

---

### Task 9: T 系列包级语法糖

**Files:**
- Create: `log/sugar.go`
- Test: `log/sugar_test.go`

**Interfaces:**
- Consumes: `Logger` / `Level` / `CallerSkipper`（Task 1）、`Ctx`（Task 7）、`installObserver`（Task 7 的 `zap_test.go`）
- Produces:
  - `func TDebug/TInfo/TWarn/TError(ctx context.Context, msg string, kv ...any)`
  - `func TDebugf/TInfof/TWarnf/TErrorf(ctx context.Context, format string, args ...any)`

**定位：** `TInfo(ctx, ...)` 是 `Ctx(ctx).Info(...)` 的语法糖，两条路径完全等价，用哪条随意。`f` 版本走 printf 语义，用于确实不需要结构化的场合（启动横幅、调试字符串）；**能拆成 KV 的都别用 f 版本** —— 拼进 msg 的字段检索不到。

- [ ] **Step 1: 写失败测试**

创建 `log/sugar_test.go`：

```go
package log

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The T-series adds one more stack frame than the facade methods; the caller
// must still point to the calling business code.
// gfa's single AddCallerSkip(2) would point to the wrong frame when going
// through the facade methods -- both paths are tested here.
func TestTSeriesCallerPointsToCallSite(t *testing.T) {
	logs := installObserver(t, zap.AddCaller())

	_, _, line, _ := runtime.Caller(0)
	TInfo(context.Background(), "via T")     // line + 1
	L().Info("via facade")                   // line + 2
	TInfof(context.Background(), "via %s", "Tf") // line + 3

	require.Len(t, logs.All(), 3)
	assert.Equal(t, line+1, logs.All()[0].Caller.Line, "TInfo 的 caller")
	assert.Equal(t, line+2, logs.All()[1].Caller.Line, "门面方法的 caller")
	assert.Equal(t, line+3, logs.All()[2].Caller.Line, "TInfof 的 caller")

	for _, e := range logs.All() {
		assert.Contains(t, e.Caller.File, "sugar_test.go")
	}
}

func TestTSeriesLevels(t *testing.T) {
	logs := installObserver(t)
	ctx := context.Background()

	TDebug(ctx, "d")
	TInfo(ctx, "i")
	TWarn(ctx, "w")
	TError(ctx, "e")

	got := make([]zapcore.Level, 0, 4)
	for _, e := range logs.All() {
		got = append(got, e.Level)
	}
	assert.Equal(t, []zapcore.Level{
		zapcore.DebugLevel, zapcore.InfoLevel, zapcore.WarnLevel, zapcore.ErrorLevel,
	}, got)
}

func TestTSeriesCarriesKV(t *testing.T) {
	logs := installObserver(t)
	TInfo(context.Background(), "下单", "order_id", 1001, "amount", 99.5)

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.EqualValues(t, 1001, m["order_id"])
	assert.InDelta(t, 99.5, m["amount"], 0.001)
}

func TestTSeriesEquivalentToFacade(t *testing.T) {
	logs := installObserver(t)
	ctx := context.Background()

	TInfo(ctx, "同一条", "k", "v")
	Ctx(ctx).Info("同一条", "k", "v")

	require.Len(t, logs.All(), 2)
	assert.Equal(t, logs.All()[0].Message, logs.All()[1].Message)
	assert.Equal(t, logs.All()[0].ContextMap(), logs.All()[1].ContextMap(),
		"两条路径必须产出完全相同的日志")
}

func TestTSeriesInheritsContextTrace(t *testing.T) {
	logs := installObserver(t)

	tr := NewTrace("http.request")
	TInfo(WithTrace(context.Background(), tr), "带链路")

	require.Len(t, logs.All(), 1)
	assert.Equal(t, tr.TraceID().String(), logs.All()[0].ContextMap()["trace_id"])
}

func TestTSeriesFormatVariants(t *testing.T) {
	logs := installObserver(t)
	ctx := context.Background()

	TDebugf(ctx, "conn=%d", 10)
	TInfof(ctx, "user=%s age=%d", "alice", 30)
	TWarnf(ctx, "retry %d/%d", 2, 3)
	TErrorf(ctx, "failed: %v", context.Canceled)

	require.Len(t, logs.All(), 4)
	assert.Equal(t, "conn=10", logs.All()[0].Message)
	assert.Equal(t, "user=alice age=30", logs.All()[1].Message)
	assert.Equal(t, "retry 2/3", logs.All()[2].Message)
	assert.Equal(t, "failed: context canceled", logs.All()[3].Message)
}

type stringerFunc func() string

func (f stringerFunc) String() string { return f() }

// The f version must check the level before formatting, otherwise a disabled
// log level still pays the formatting cost.
func TestTInfofSkipsFormattingWhenLevelDisabled(t *testing.T) {
	core, _ := observer.New(zapcore.ErrorLevel) // info not enabled
	SetLogger(newZapLogger(zap.New(core)))
	t.Cleanup(func() { SetLogger(Nop()) })

	calls := 0
	arg := stringerFunc(func() string { calls++; return "expensive" })

	TInfof(context.Background(), "%s", arg)
	assert.Zero(t, calls, "级别不启用时不该触发格式化")

	TErrorf(context.Background(), "%s", arg)
	assert.Equal(t, 1, calls, "启用的级别照常格式化")
}

func TestTSeriesNilContextDoesNotPanic(t *testing.T) {
	installObserver(t)
	assert.NotPanics(t, func() {
		TInfo(nil, "nil ctx")          //nolint:staticcheck
		TInfof(nil, "nil ctx %d", 1)   //nolint:staticcheck
	})
}

func TestTSeriesWorksWithBackendLackingCallerSkipper(t *testing.T) {
	SetLogger(Nop()) // nopLogger doesn't implement CallerSkipper
	t.Cleanup(func() { SetLogger(Nop()) })

	assert.NotPanics(t, func() {
		TInfo(context.Background(), "后端不支持 skip 也要能打")
		TInfof(context.Background(), "%d", 1)
	})
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'TSeries|TInfof' -v
```

Expected: 编译失败，`undefined: TInfo`。

- [ ] **Step 3: 写实现**

创建 `log/sugar.go`：

```go
package log

import (
	"context"
	"fmt"
)

// tLogger retrieves the Logger on ctx and adds one more caller skip -- the
// T-series has one more call frame than the facade methods, and without the
// skip the caller would point into sugar.go.
//
// If the backend doesn't implement CallerSkipper, it's returned as-is: logs
// still go out, just with an inaccurate caller.
func tLogger(ctx context.Context) Logger {
	l := Ctx(ctx)
	if cs, ok := l.(CallerSkipper); ok {
		return cs.WithCallerSkip(1)
	}
	return l
}

// The T-series is sugar for Ctx(ctx).Xxx(...); the two paths are entirely
// equivalent.
//
//	log.TInfo(ctx, "下单", "order_id", 1001)
//	log.Ctx(ctx).Info("下单", "order_id", 1001)

func TDebug(ctx context.Context, msg string, kv ...any) { tLogger(ctx).Debug(msg, kv...) }
func TInfo(ctx context.Context, msg string, kv ...any)  { tLogger(ctx).Info(msg, kv...) }
func TWarn(ctx context.Context, msg string, kv ...any)  { tLogger(ctx).Warn(msg, kv...) }
func TError(ctx context.Context, msg string, kv ...any) { tLogger(ctx).Error(msg, kv...) }

// The f-series uses printf semantics, for cases that genuinely don't need
// structure (startup banners, debug strings). Anything that can be broken
// into KV pairs shouldn't use it -- fields folded into msg can't be searched.
//
// Checks the level before formatting: a disabled log level shouldn't pay the
// formatting cost.

func TDebugf(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(DebugLevel) {
		l.Debug(fmt.Sprintf(format, args...))
	}
}

func TInfof(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(InfoLevel) {
		l.Info(fmt.Sprintf(format, args...))
	}
}

func TWarnf(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(WarnLevel) {
		l.Warn(fmt.Sprintf(format, args...))
	}
}

func TErrorf(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(ErrorLevel) {
		l.Error(fmt.Sprintf(format, args...))
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -race -v
```

Expected: 全部 PASS。`TestTSeriesCallerPointsToCallSite` 是本 task 的核心 —— 它把 gfa 那个 caller 指错的问题钉成了回归测试。

- [ ] **Step 5: Commit**

```bash
git add log/sugar.go log/sugar_test.go
git commit -m "feat(log): T 系列语法糖，两条路径 caller 都指向调用点"
```

---

### Task 10: 跨服务传播、集成验收与 README

**Files:**
- Create: `log/propagate.go`
- Create: `log/integration_test.go`
- Create: `log/README.md`
- Test: `log/propagate_test.go`

**Interfaces:**
- Consumes: 全部前序产物
- Produces:
  - `const RequestIDHeader = "X-Request-Id"`
  - `func Extract(ctx context.Context, carrier propagation.TextMapCarrier, spanName string) context.Context`
  - `func Inject(ctx context.Context, carrier propagation.TextMapCarrier)`

**为什么用 `propagation.TextMapCarrier` 而不是 `http.Header`：** log 包不该知道 HTTP。`propagation.HeaderCarrier` 是 otel 提供的 `http.Header` 适配，HTTP 层（Plan 3）套一层就行；gRPC metadata 也能用同一套接口。

**`Extract` 的一个巧妙点：** 上游只给了 `traceparent` 没给 `X-Request-Id` 时，从 trace_id 反推 request_id —— 两者是同一个 128 bit 值的两种编码（Task 2），反推无损。

- [ ] **Step 1: 写传播的失败测试**

创建 `log/propagate_test.go`：

```go
package log

import (
	"context"
	"net/http"
	"testing"

	"github.com/oklog/ulid/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestInjectThenExtractInheritsTrace(t *testing.T) {
	upstream := NewTrace("client.call")
	h := http.Header{}
	Inject(WithTrace(context.Background(), upstream), propagation.HeaderCarrier(h))

	require.NotEmpty(t, h.Get("traceparent"), "必须写 W3C 标准头")
	assert.Equal(t, upstream.RequestID, h.Get(RequestIDHeader))

	ctx := Extract(context.Background(), propagation.HeaderCarrier(h), "GET /orders")
	got := TraceFrom(ctx)

	assert.Equal(t, upstream.TraceID(), got.TraceID(), "trace_id 跨进程继承")
	assert.Equal(t, upstream.SpanID(), got.ParentSpanID, "上游 span 成为 parent")
	assert.NotEqual(t, upstream.SpanID(), got.SpanID(), "本进程开新 span")
	assert.Equal(t, upstream.RequestID, got.RequestID)
	assert.Equal(t, "GET /orders", got.SpanName)
}

func TestExtractWithoutHeaderStartsNewTrace(t *testing.T) {
	ctx := Extract(context.Background(), propagation.HeaderCarrier(http.Header{}), "cron.job")

	tr := TraceFrom(ctx)
	assert.True(t, tr.Valid())
	assert.False(t, tr.ParentSpanID.IsValid())
	assert.Equal(t, "cron.job", tr.SpanName)
	assert.NotEmpty(t, tr.RequestID)
}

func TestExtractIgnoresMalformedTraceparent(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "garbage")

	tr := TraceFrom(Extract(context.Background(), propagation.HeaderCarrier(h), "svc"))
	assert.True(t, tr.Valid(), "上游头畸形时降级为新链路，不能让请求失败")
	assert.False(t, tr.ParentSpanID.IsValid())
}

// When upstream only supplies traceparent and not X-Request-Id, derive it
// from trace_id.
func TestExtractDerivesRequestIDFromTraceID(t *testing.T) {
	h := http.Header{}
	// The standard sample from the W3C spec document
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	tr := TraceFrom(Extract(context.Background(), propagation.HeaderCarrier(h), "svc"))

	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", tr.TraceID().String())
	assert.Equal(t, "00f067aa0ba902b7", tr.ParentSpanID.String())

	u, err := ulid.Parse(tr.RequestID)
	require.NoError(t, err)
	assert.Equal(t, tr.TraceID(), trace.TraceID(u), "request_id 是 trace_id 的 ULID 编码")
}

func TestExtractPrefersUpstreamRequestID(t *testing.T) {
	h := http.Header{}
	h.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.Set(RequestIDHeader, "01M0RX90K2CGPJ7V41N2SN057D")

	tr := TraceFrom(Extract(context.Background(), propagation.HeaderCarrier(h), "svc"))
	assert.Equal(t, "01M0RX90K2CGPJ7V41N2SN057D", tr.RequestID, "上游给了就用上游的")
}

func TestExtractBindsLoggerIntoContext(t *testing.T) {
	logs := installObserver(t)

	ctx := Extract(context.Background(), propagation.HeaderCarrier(http.Header{}), "GET /x")
	TInfo(ctx, "handled")

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, TraceFrom(ctx).TraceID().String(), m["trace_id"])
	assert.Equal(t, "GET /x", m["span_name"])
}

func TestInjectIsNoopWithoutTrace(t *testing.T) {
	h := http.Header{}
	Inject(context.Background(), propagation.HeaderCarrier(h))
	assert.Empty(t, h, "没有链路就什么都不写，别造出无效的 traceparent")
}
```

- [ ] **Step 2: 跑测试确认失败**

```bash
go test ./log/ -run 'Extract|Inject' -v
```

Expected: 编译失败，`undefined: Extract`。

- [ ] **Step 3: 写传播实现**

创建 `log/propagate.go`：

```go
package log

import (
	"context"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader is this framework's extension header for the request ID.
// W3C only specifies traceparent; request_id is the human-friendly half.
const RequestIDHeader = "X-Request-Id"

// Only W3C TraceContext is used. Formats like B3 or Jaeger can be wired up
// by the caller if needed.
var propagator = propagation.TraceContext{}

// Extract decodes the upstream trace from the inbound carrier, opens a new
// span for this process, and binds a Logger carrying the trace fields into
// ctx.
//
// If upstream doesn't provide a valid traceparent, a new trace is started --
// a malformed header shouldn't fail the request.
//
// carrier uses propagation.TextMapCarrier instead of http.Header: the log
// package shouldn't know about HTTP. The HTTP layer wraps it with
// propagation.HeaderCarrier, and gRPC uses a metadata adapter -- both go
// through the same interface.
func Extract(ctx context.Context, carrier propagation.TextMapCarrier, spanName string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	var t Trace
	if sc := trace.SpanContextFromContext(propagator.Extract(ctx, carrier)); sc.IsValid() {
		t = Trace{
			SpanContext:  sc.WithSpanID(newSpanID()),
			ParentSpanID: sc.SpanID(),
			SpanName:     spanName,
			RequestID:    requestIDFrom(carrier, sc.TraceID()),
		}
	} else {
		t = NewTrace(spanName)
	}

	return NewContext(WithTrace(ctx, t), L().With(traceKV(t)...))
}

// requestIDFrom prefers the upstream-supplied X-Request-Id; falling back to
// deriving it from trace_id when absent.
// trace_id and ULID are both 128 bits, two encodings of the same value, so
// the derivation is lossless.
func requestIDFrom(carrier propagation.TextMapCarrier, tid trace.TraceID) string {
	if v := carrier.Get(RequestIDHeader); v != "" {
		return v
	}
	return ulid.ULID(tid).String()
}

// Inject writes the current trace into the outbound carrier: W3C traceparent
// plus X-Request-Id.
// Does nothing when ctx has no trace -- don't manufacture an invalid
// traceparent.
func Inject(ctx context.Context, carrier propagation.TextMapCarrier) {
	t := TraceFrom(ctx)
	if !t.Valid() {
		return
	}
	propagator.Inject(trace.ContextWithSpanContext(ctx, t.SpanContext), carrier)
	if t.RequestID != "" {
		carrier.Set(RequestIDHeader, t.RequestID)
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

```bash
go test ./log/ -run 'Extract|Inject' -race -v
```

Expected: 全部 PASS。

- [ ] **Step 5: Commit 传播**

```bash
git add log/propagate.go log/propagate_test.go
git commit -m "feat(log): W3C traceparent 跨服务传播"
```

- [ ] **Step 6: 写集成验收测试**

创建 `log/integration_test.go`：

```go
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
```

- [ ] **Step 7: 跑全量测试**

```bash
go test ./log/ -race -count=1 -v
go vet ./log/
```

Expected: 全部 PASS，`go vet` 无输出。

- [ ] **Step 8: 写 README**

创建 `log/README.md`：

````markdown
# xbc/log

结构化日志门面 + zap 默认实现。零框架依赖，可脱离 xbc 单独用。

```bash
go get github.com/xbcio/xbc/log
```

## 快速开始

```go
package main

import (
	"context"

	"github.com/xbcio/xbc/log"
)

func main() {
	cfg := log.DefaultConfig()
	cfg.Level = "debug"
	cfg.File.Enabled = true
	cfg.File.Path = "logs/app.jsonl" // extension determines the format: .jsonl -> json
	if err := log.Init(cfg); err != nil {
		panic(err)
	}
	defer log.Close()

	ctx := context.Background()
	log.TInfo(ctx, "服务启动", "port", 8080, "env", "prod")
}
```

## 退出前刷盘

```go
defer log.Close()   // flush + close file handles, use this on process exit
log.Sync()          // flush only, don't close; use when a long-lived process wants to flush immediately
```

`Close()` 内部先 `Sync()` 再关文件，两者都对「往终端/管道 fsync 返回 EINVAL」这个 zap 的著名毛刺做了吞掉处理 —— 不会让 `defer` 在每次正常退出时报一个假错。

## 两条等价路径

```go
log.TInfo(ctx, "下单", "order_id", 1001)     // sugar
log.Ctx(ctx).Info("下单", "order_id", 1001)  // facade
```

产出完全相同，caller 都指向你的调用行。链式派生时用门面：

```go
l := log.Ctx(ctx).With("module", "payment")
l.Info("发起支付", "amount", 99.5)
l.Warn("网关超时", "retry", 1)
```

`printf` 语义走 `f` 版本，但**能拆成 KV 的都别用** —— 拼进 msg 的字段检索不到：

```go
log.TInfof(ctx, "启动耗时 %.2fs", 1.35)   // fine: a one-off startup banner
log.TInfof(ctx, "订单 %d 金额 %.2f", id, amt)  // don't do this, order_id can't be searched for
log.TInfo(ctx, "下单", "order_id", id, "amount", amt)  // do it this way
```

## 链路追踪

```go
// Entry: decode the trace from the upstream header, start a new one if absent
ctx := log.Extract(r.Context(), propagation.HeaderCarrier(r.Header), "GET /orders/:id")

// Child span
dbCtx, done := log.Span(ctx, "db.query")
defer done()   // automatically logs one debug line with elapsed_ms
log.TInfo(dbCtx, "查询订单", "order_id", 1001)

// Outbound: pass the trace along to the downstream service
log.Inject(ctx, propagation.HeaderCarrier(req.Header))
```

`trace_id` 与 `request_id` 是同一个 128 bit 值的两种编码：前者是 W3C 的 32 位 hex，后者是 ULID 的 26 位 Crockford Base32。ULID 前 6 字节是毫秒时间戳，所以 `request_id` 天然按时间有序、肉眼可比大小。

跟 OTel SDK 混用时，`Trace` 内嵌了 `trace.SpanContext`，直接传即可：

```go
tr := log.TraceFrom(ctx)
otelCtx := trace.ContextWithSpanContext(ctx, tr.SpanContext)
```

## 配置

```yaml
log:
  level: info           # debug | info | warn | error
  caller: true          # whether to record the call site
  stacktrace: error     # from which level a stacktrace is attached

  console:
    enabled: true
    format: console     # console | json
    color: auto         # auto | always | never

  file:
    enabled: false
    path: logs/app.log  # extension determines the format: .log -> console, .jsonl/.json/.ndjson -> json
    format: ""          # empty = infer from extension; set to override the inference
    rotate: daily       # daily | size
    max_size: 100       # MB, threshold for the size strategy, also applies under daily
    max_age: 30         # days
    max_backups: 30     # count
    compress: true
    error_path: ""      # non-empty opens an extra sink that only receives error and above, format inferred from its own extension

  sampling:
    initial: 100        # log every one of the first N per second
    thereafter: 100     # then log 1 out of every N; set to 0 to disable sampling
  mask_fields: []       # additional fields to mask
```

格式是 sink 级而不是全局的 —— "终端 console + 文件 json" 是最常见的组合，全局单一 format 表达不了。

`color: auto` 的判定顺序：`NO_COLOR` 未设置 → `TERM` 不是 `dumb` → 输出是 TTY。

`rotate: daily` 是本包在 lumberjack 之上补的（它本身只按大小滚）。跨天滚出的归档文件名带的是**触发时刻**的时间戳而内容是**前一天**的 —— 这是 lumberjack 的既定命名规则。

## 脱敏

敏感字段在 `zapcore.Core` 层被拦截替换成 `***`，调用方绕不过去，`log.Zap()` 逃生舱口也一样。

内置黑名单**始终生效、不可通过配置移除**：`password` / `token` / `access_token` / `refresh_token` / `secret` / `private_key` / `ak` / `sk` / `db_url` / `dsn` / `id_card` / `bank_card` / `phone` / `authorization` / `cookie` 等及其常见变体。字段名匹配前会归一化（转小写、去掉 `_` `-` `.`），所以 `accessToken`、`ACCESS_TOKEN`、`access-token` 命中同一条规则。

匹配是精确的不是前缀的 —— `phone` 命中，`phone_masked` 和 `token_count` 不命中。

`mask_fields` 只能追加：

```yaml
log:
  mask_fields: [salary, home_address]
```

## 换后端

```go
log.SetLogger(myLogger)   // just implement the log.Logger interface
```

**换后端等于换掉内置脱敏** —— 那是实现在本包里的，第三方实现不会自动带上。`SetLogger` 会往 stderr 打一条警示。

可选能力接口按需实现：

- `ZapProvider` —— 让 `log.Zap(ctx)` 能返回底层 `*zap.Logger`
- `CallerSkipper` —— 让 `TInfo` 等语法糖的 caller 指向调用点而不是本包内部

两个都不实现也能工作，只是失去对应能力。

## 从 gfa 迁移

gfa 的 `TInfo(ctx, args...)` 是 **Sprintln 语义**，本包是 **KV 语义**。签名兼容但含义不同，编译不会报错：

```go
log.TInfo(ctx, "支付成功", orderID, amount)
```

- `orderID` 是 `int` → 产出两个 `!BADKEY` 字段，日志里一眼看得出来
- `orderID` 是 `string` → **静默变成 `<订单号>=<金额>` 这样一个字段**，key 是订单号本身

第二种是真陷阱，运行时检测不出来（它是完全合法的 KV 用法）。迁移时逐个改写，别批量替换：

```go
log.TInfo(ctx, "支付成功", "order_id", orderID, "amount", amount)  // KV
log.TInfof(ctx, "支付成功 %s %.2f", orderID, amount)               // or go through printf explicitly
```

`grep -rn 'T\(Info\|Warn\|Error\|Debug\)(' ` 过一遍，确认每个调用的第三个参数往后都是 `"key", value` 交替。

## console 输出格式

```
10:23:45.123 INFO  01926f7e         order/service.go:42  校验通过  amount=99 order_id=1001
10:23:45.201 ERROR 01926f7e        payment/client.go:33  支付失败  err="connection refused"
```

| 段 | 宽度 | 说明 |
|---|---|---|
| 时间 | 12 | `15:04:05.000`，不打日期（日期在文件名里） |
| level | 5 | 左对齐，按级着色 |
| trace | 8 | `trace_id` 前 8 位，无链路时留空 |
| caller | 24 | 右对齐，超长从左侧截断加 `…` |
| msg | 变长 | 后跟两个空格 |
| KV | 变长 | `key=value`，含空格的值加引号 |

字段按 key 字母序输出：既保证输出确定性，也让同名字段每行落在相似位置。

console 下 `span_id` 与 `request_id` 不进 KV 区（`trace_id` 已占固定列），json sink 全留 —— 一个给人扫读，一个给机器检索。
````

- [ ] **Step 9: 跑最后一遍全量并确认覆盖率**

```bash
go test ./log/ -race -count=1 -cover
gofmt -l log/
```

Expected: PASS，覆盖率不低于 85%；`gofmt -l` 无输出。

- [ ] **Step 10: Commit**

```bash
git add log/integration_test.go log/README.md
git commit -m "test(log): 集成验收与依赖方向门禁；docs: log 包 README"
```

---

## 完成标准

全部 10 个 task 结束后，下面每一条都应成立：

1. `go test ./log/ -race -count=1` 全绿，覆盖率 ≥ 85%
2. `go vet ./log/` 与 `gofmt -l log/` 无输出
3. `go list -deps github.com/xbcio/xbc/log | grep xbcio` 只有 `github.com/xbcio/xbc/log` 自己
4. `go test ./log/ -run TestConsoleDemo -v` 的输出肉眼确认四列对齐、着色正确、中文不乱码
5. 仓库里除 `log/`、`go.mod`、`go.sum`、`docs/` 外没有其他 Go 代码 —— 内核是下一个 plan 的事

## 与 spec 的对照

| spec 章节 | 落在哪个 task |
|---|---|
| §8.1 链路标识模型（ULID/trace_id 同值双编码） | Task 2 |
| §8.2 链路怎么串（W3C 传播、Fork） | Task 2、8、10 |
| §8.3 门面接口（Logger / ZapProvider / T 系列 / Span / Trace） | Task 1、7、8、9 |
| §8.4 业务代码的样子 | Task 9 + README |
| §8.5 与 OTel SDK 互操作 | Task 2（内嵌 SpanContext）+ README |
| §8.6 配置：sink 各自决定格式 | Task 3、7 |
| §8.7 console 渲染（对齐/着色/TTY 探测） | Task 5 |
| §8.8 脱敏做在 encoder 层 | Task 4（实现落在 Core 层，见该 task 的说明） |
| §8.9 框架侧接入 | 不在本 plan —— 属于内核与 HTTP 层 |
| §8.10 其他仓库怎么用 | Task 10 的 README |
| §9 零依赖子包（log 部分） | 全局约束 + Task 10 的依赖门禁测试 |
| §13 实施顺序第 0 步 | 本 plan 全部 |

**本 plan 刻意不做的：** `log.body.enabled`（accesslog 记不记请求体）属于 HTTP 层；`doctor` 对非默认后端的提示属于内核诊断命令；配置的 `default:` / `validate:` tag 解析属于配置插件 —— 本包自带 `DefaultConfig()` + `Normalize()`，不依赖内核。

## 已知取舍

| 取舍 | 说明 |
|---|---|
| 多个 `!BADKEY` 字段会同名 | json 里出现重复 key（zap 不去重），console 里 map 只留最后一个。不修 —— 出现 `!BADKEY` 本身就是要改的信号，不是要精确计数的数据 |
| 跨天归档文件名时间戳错位 | 归档名带触发时刻、内容是前一天的。lumberjack 的既定行为，`max_age` 也按这个时间戳算 |
| `Ctx(ctx)` 的慢路径每次分配 | 只有 Trace 没有 Logger 时现场派生。框架入口中间件应总走 `NewContext`，把这条路径压到零 |
| console 字段按字母序而非写入顺序 | `MapObjectEncoder` 本就无序。字母序换来输出确定性和扫读一致性 |
| 换后端后内置脱敏失效 | `SetLogger` 会警示，但无法强制。这是门面模式的固有代价 |
| `log.UseTracer` 未实现 | spec §8.5 的互操作模式对照表列了两种模式：默认模式（本包自管 Trace，可读取 OTel 的 SpanContext 但不回写）和 `UseTracer` 模式（`Span()` 委托给真 OTel tracer，span 同时进日志和 Jaeger/Tempo，`done()` 需调 `span.End()`）。当前只落地了默认模式。`UseTracer` 涉及 tracer 生命周期管理、`done()` 语义变化、额外的 SDK 依赖，是一个完整特性而不是一处修补，半吊子实现比不实现更糟，故推迟到后续 plan。`TraceFrom` 已能读到外部 OTel SDK 设置的 SpanContext（本地 Trace 优先、OTel 回退），但 `log.Span()` 目前只写本包自己的 key，不会把 span 送进 OTel SDK——互操作是单向的 |

---

## 后续 plan

spec 覆盖多个独立子系统，按每个自身可交付可测试的原则拆成五个 plan，本文是第一个：

| # | 范围 | spec 章节 | 状态 |
|---|---|---|---|
| 1 | **log 子包** | §8、§9、§13-0 | 本文 |
| 2 | 内核：配置 + 依赖解析 + 装配管线 | §4、§5、§6 | 待写 |
| 3 | HTTP 层 | §7 | 待写 |
| 4 | 首批插件（cors / jwt / gorm / redis / ratelimit / cron） | §11 | 待写 |
| 5 | examples + 根 README | §13-7 | 待写 |

后四个 plan 等前一个实现落地后再写 —— 基于真实代码写的计划，比基于设想写的准。
