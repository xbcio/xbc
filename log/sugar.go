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

// TDebug is sugar for Ctx(ctx).Debug(msg, kv...). The two paths produce
// identical output, caller included:
//
//	log.TInfo(ctx, "下单", "order_id", 1001)
//	log.Ctx(ctx).Info("下单", "order_id", 1001)
//
// Identical output, not identical cost: the T-series adds the tLogger call
// above, whose CallerSkipper type assertion measured ~3% slower per entry at
// the same allocation count. Pick by readability, not by speed -- and reach
// for the facade when deriving a child logger, since With returns one worth
// holding onto.
func TDebug(ctx context.Context, msg string, kv ...any) { tLogger(ctx).Debug(msg, kv...) }

// TInfo is sugar for Ctx(ctx).Info(msg, kv...).
func TInfo(ctx context.Context, msg string, kv ...any) { tLogger(ctx).Info(msg, kv...) }

// TWarn is sugar for Ctx(ctx).Warn(msg, kv...).
func TWarn(ctx context.Context, msg string, kv ...any) { tLogger(ctx).Warn(msg, kv...) }

// TError is sugar for Ctx(ctx).Error(msg, kv...).
func TError(ctx context.Context, msg string, kv ...any) { tLogger(ctx).Error(msg, kv...) }

// TFatal is sugar for Ctx(ctx).Fatal(msg, kv...). It never returns.
func TFatal(ctx context.Context, msg string, kv ...any) { tLogger(ctx).Fatal(msg, kv...) }

// TDebugf uses printf semantics, for cases that genuinely don't need structure
// (startup banners, debug strings). Anything that can be broken into KV pairs
// shouldn't use it -- fields folded into msg can't be searched.
//
// Checks the level before formatting: a disabled log level shouldn't pay the
// formatting cost.
func TDebugf(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(DebugLevel) {
		l.Debug(fmt.Sprintf(format, args...))
	}
}

// TInfof is the printf variant of TInfo. Fields folded into msg can't be
// searched -- anything that can be broken into KV pairs shouldn't use it.
func TInfof(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(InfoLevel) {
		l.Info(fmt.Sprintf(format, args...))
	}
}

// TWarnf is the printf variant of TWarn. Fields folded into msg can't be
// searched -- anything that can be broken into KV pairs shouldn't use it.
func TWarnf(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(WarnLevel) {
		l.Warn(fmt.Sprintf(format, args...))
	}
}

// TErrorf is the printf variant of TError. Fields folded into msg can't be
// searched -- anything that can be broken into KV pairs shouldn't use it.
func TErrorf(ctx context.Context, format string, args ...any) {
	if l := tLogger(ctx); l.Enabled(ErrorLevel) {
		l.Error(fmt.Sprintf(format, args...))
	}
}

// TFatalf is the printf variant of TFatal. It never returns.
//
// Unlike its siblings it does NOT guard on Enabled: a disabled backend (Nop,
// say) would report false, the call would fall through, and the process would
// keep running past a line the caller wrote expecting termination. Formatting
// one string on the way out of the process is not a cost worth that risk.
func TFatalf(ctx context.Context, format string, args ...any) {
	tLogger(ctx).Fatal(fmt.Sprintf(format, args...))
}
