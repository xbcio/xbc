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
