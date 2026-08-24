// Package log is xbc's logging facade.
//
// It follows the SLF4J approach: business code and plugins depend only on
// this package's Logger interface, and the concrete backend is wired up by
// Init (zap by default) or replaced via SetLogger.
// This package has zero framework dependencies and can be used standalone, outside xbc.
package log

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Level is the log level.
// The numeric values are deliberately aligned with zapcore.Level (Debug=-1), so bindings can type-convert directly.
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

// ParseLevel parses a level name. An empty string is treated as "unspecified"
// and returns InfoLevel; any other unrecognized name (e.g. a misspelled
// "verbose") returns an error and is never silently downgraded -- otherwise a
// misconfigured level could silently drop logs in production with no way to trace it.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return DebugLevel, nil
	case "info", "": // Zero-value-usable convention: matches how zapcore.Level.UnmarshalText
		// treats an empty string, so a Level field left blank in YAML/env vars
		// falls through to info instead of erroring.
		return InfoLevel, nil
	case "warn", "warning":
		return WarnLevel, nil
	case "error":
		return ErrorLevel, nil
	default:
		return InfoLevel, fmt.Errorf("log: 未知日志级别 %q，可选 debug/info/warn/error", s)
	}
}

// Logger is the facade interface. Variadic args are a KV sequence: key1, val1, key2, val2, ...
// A key must be a string; a non-string key or an unpaired trailing argument
// is filed under the "!BADKEY" field -- it never panics and is never silently dropped.
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)

	// With derives a child Logger with fixed fields attached.
	With(kv ...any) Logger

	// Enabled reports whether this level will actually produce output, used to short-circuit expensive field construction.
	Enabled(lv Level) bool
}

// ZapProvider is an optional capability interface. A backend built on zap
// should implement it, letting callers reach a strongly-typed entry point
// via log.Zap(ctx) for zap-specific operations.
// When switching to a non-zap backend, simply don't implement it; log.Zap will return ok=false.
type ZapProvider interface {
	Zap() *zap.Logger
}

// CallerSkipper is an optional capability interface. Package-level syntax
// sugar (TInfo, etc.) adds one more stack frame than the facade methods, and
// this interface is what lets caller point back at the business code instead of into the log package itself.
// Third-party bindings can work without implementing it, at the cost of caller pointing at sugar.go.
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
// Used for tests, and as the fallback for L() before Init -- logging before initialization must not panic.
func Nop() Logger { return nopLogger{} }
