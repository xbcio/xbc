// Package log 是 xbc 的日志门面。
//
// 它遵循 SLF4J 的思路：业务代码与插件只依赖本包的 Logger 接口，
// 具体后端由 Init 装配（默认 zap）或 SetLogger 替换。
// 本包零框架依赖，可脱离 xbc 单独使用。
package log

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// Level 是日志级别。
// 数值刻意与 zapcore.Level 对齐（Debug=-1），binding 里可直接类型转换。
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

// ParseLevel 解析级别名。未知级别返回错误而不是静默降级，
// 免得配置写错时线上悄悄丢日志。
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

// Logger 是门面接口。变参是 KV 序列：key1, val1, key2, val2, ...
// key 必须是 string；不是 string 或落单的参数会被归到 "!BADKEY" 字段，
// 不会 panic 也不会静默吞掉。
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)

	// With 派生带固定字段的子 Logger。
	With(kv ...any) Logger

	// Enabled 报告该级别是否会真的输出，用来短路昂贵的字段构造。
	Enabled(lv Level) bool
}

// ZapProvider 是可选能力接口。后端若基于 zap 就实现它，
// 调用方可通过 log.Zap(ctx) 拿到强类型入口做 zap 特有的操作。
// 换成非 zap 后端时不实现即可，log.Zap 会返回 ok=false。
type ZapProvider interface {
	Zap() *zap.Logger
}

// CallerSkipper 是可选能力接口。包级语法糖（TInfo 等）比门面方法
// 多一层调用栈，靠它把 caller 指回业务代码而不是 log 包内部。
// 第三方 binding 不实现也能工作，代价是 caller 指向 sugar.go。
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

// Nop 返回丢弃一切输出的 Logger。
// 用于测试，以及 Init 之前 L() 的兜底 —— 未初始化时打日志不该 panic。
func Nop() Logger { return nopLogger{} }
