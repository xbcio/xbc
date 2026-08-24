package log

import (
	"context"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel/trace"
)

// Trace 是一次调用的链路上下文。
//
// 内嵌 OTel 的 trace.SpanContext，所以能直接喂给 OTel SDK，
// 也能被 W3C traceparent 头解析出来的值填充 —— 不需要任何转换层。
//
// TraceID 与 RequestID 是同一个 128 bit 值的两种编码：
// 前者是 W3C 要求的 32 位 hex，后者是 ULID 的 26 位 Crockford Base32。
// ULID 前 6 字节是毫秒时间戳，所以 request_id 天然按时间有序、肉眼可比大小。
type Trace struct {
	trace.SpanContext

	// ParentSpanID 是上游 span。根 span 为零值。
	ParentSpanID trace.SpanID

	// SpanName 是当前 span 的名字，如 "GET /orders/:id"、"db.query"。
	SpanName string

	// RequestID 是 TraceID 的 ULID 编码，贯穿整条链路不变。
	RequestID string
}

type traceKey struct{}

// NewTrace 开一条新链路。
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

// newSpanID 生成 8 字节 span_id。
// 取 ULID 的 [8:16] —— ULID 布局是 6 字节时间戳 + 10 字节随机熵，
// 这一段整个落在随机区内，够用且省掉直接依赖 crypto/rand。
func newSpanID() trace.SpanID {
	u := ulid.Make()
	return trace.SpanID(u[8:])
}

// Valid 报告这条链路是否有效。零值 Trace 返回 false。
func (t Trace) Valid() bool { return t.TraceID().IsValid() }

// Fork 派生子 span：trace_id 与 request_id 不变，span_id 换新，
// parent 指向当前 span。值语义，不改调用者。
func (t Trace) Fork(name string) Trace {
	child := t
	child.ParentSpanID = t.SpanID()
	child.SpanContext = t.SpanContext.WithSpanID(newSpanID())
	child.SpanName = name
	return child
}

// WithTrace 把链路存进 ctx。
func WithTrace(ctx context.Context, t Trace) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom 从 ctx 取链路。取不到返回零值 Trace（Valid() == false），不 panic。
func TraceFrom(ctx context.Context) Trace {
	if ctx == nil {
		return Trace{}
	}
	t, _ := ctx.Value(traceKey{}).(Trace)
	return t
}
