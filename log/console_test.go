package log

import (
	"bytes"
	"errors"
	"os"
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

// 中文不能被转义成 \uXXXX —— 这正是不能用 strconv.Quote 的原因。
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

// 每个级别的 level 段都必须恰好占 widthLevel 宽 —— DPANIC 是 6 个字符，
// 是全部级别里最长的，widthLevel 小于它就会让这一行整体右移。
func TestConsoleLevelColumnWidthIsUniform(t *testing.T) {
	levels := []zapcore.Level{
		zapcore.DebugLevel, zapcore.InfoLevel, zapcore.WarnLevel,
		zapcore.ErrorLevel, zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel,
	}
	for _, lv := range levels {
		ent := sampleEntry()
		ent.Level = lv
		out := encodeOne(t, false, ent, nil, nil)
		// 时间段固定 12 宽 + 1 空格，其后 widthLevel 宽即 level 段
		seg := out[13 : 13+widthLevel]
		assert.Equal(t, lv.CapitalString(), strings.TrimRight(seg, " "), "级别 %s 的文本", lv)
		assert.Equal(t, widthLevel, len(seg), "级别 %s 的列宽", lv)
		assert.Equal(t, byte(' '), out[13+widthLevel], "级别 %s 后必须紧跟分隔空格", lv)
	}
}

// 着色码宽度为 0：上色与不上色，去掉 ANSI 后必须逐字节相同。
func TestConsoleColorDoesNotBreakAlignment(t *testing.T) {
	ent := sampleEntry()
	fs := []zapcore.Field{zap.Int("n", 1)}
	plain := encodeOne(t, false, ent, nil, fs)
	colored := stripANSI(encodeOne(t, true, ent, nil, fs))
	assert.Equal(t, plain, colored)
}

// zap.Namespace 之后的字段被 MapObjectEncoder 收进嵌套 map，顶层只剩空间名。
// console 必须把它展平成点号全路径，而不是打印 Go 的 map[k:v] 字面量。
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

// 展平后 consoleHiddenFields 用全路径判定：顶层 trace_id 照旧剔除，
// 命名空间里的同名字段是调用方显式放进去的，保留。
func TestConsoleHiddenFieldsUseFullPath(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("trace_id", "0192abcd0192abcd"),
		zap.Namespace("upstream"),
		zap.String("trace_id", "ffffffffffffffff"),
	})
	assert.NotContains(t, out, "trace_id=0192abcd0192abcd", "顶层 trace_id 已占固定列，不进 KV 区")
	assert.Contains(t, out, "upstream.trace_id=ffffffffffffffff")
}

// 自引用 map 不能让展平递归停不下来。
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

// ByteString / Binary 在 MapObjectEncoder 里都是 []byte，
// 不特判就会落盘 [104 105] 而不是 hi。
func TestConsoleRendersByteStringAsText(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.ByteString("body", []byte("hi")),
	})
	assert.Contains(t, out, "body=hi")
	assert.NotContains(t, out, "[104 105]")
}

// caller 路径含中文时不能切出 U+FFFD，也不能因为按字节计数而错位。
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
	var buf bytes.Buffer // 不是 *os.File，不可能是 TTY

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

// TestConsoleDemo 把各级别各写一行到 stdout 供肉眼验收对齐与配色。
// 长期留在仓库里，改动 encoder 后随手跑一次：
//
//	go test ./log/ -run TestConsoleDemo -v
//
// 肉眼验收断言不了，但"每行都真的产出了"断言得了 —— 光看不断言的用例在
// encoder 悄悄返回空串时会绿着通过，起不到守护作用。
func TestConsoleDemo(t *testing.T) {
	if testing.Short() {
		t.Skip("演示用例，-short 下跳过")
	}
	// 同时写 stdout（给人看）与 buf（给断言看）
	var buf bytes.Buffer
	sink := zapcore.NewMultiWriteSyncer(zapcore.Lock(os.Stdout), zapcore.AddSync(&buf))
	enc := newConsoleEncoder(true) // 强制着色，非 TTY 下也能看到效果
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
