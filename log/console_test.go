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
	assert.Contains(t, line, "INFO  ", "level 左对齐补到 6 宽（widthLevel）")
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

// 注意：不能靠加深路径层数来凑出超长 TrimmedPath —— TrimmedPath 只保留
// 最后两段路径，深层路径落到 TrimmedPath 上往往更短（父目录名可能很短）。
// 要触发截断，必须让"父目录名+文件名+行号"本身够长，例如把包名和文件名
// 都写长：TrimmedPath 出来是 "verylongpackagename/handlerimplementation.go:1234"，
// 49 个字符，稳超 widthCaller(24)。
func TestConsoleCallerTruncatedFromLeft(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/src/verylongpackagename/handlerimplementation.go"
	ent.Caller.Line = 1234

	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "…", "超长 caller 从左侧截断")
	assert.Contains(t, out, ".go:1234", "行号一侧必须完整保留")
}

// 断言 caller 段的实际渲染宽度恰好等于 widthCaller —— 只断言"前面有几个
// 空格"钉不住具体列宽，有人把 widthCaller 改掉这条测试也不会失败。
//
// 注意：下面几条基于 widthCaller 符号本身计算出来的断言（len(seg) ==
// widthCaller 之类）钉不住"widthCaller 该是 24"这件事本身——常量改了，
// 这些断言引用的是同一个常量，会跟着改，永远不会失败。真正钉住 24 这个
// 具体值的是最后一行 assert.Equal(t, 24, widthCaller) 和下面
// TestConsoleCallerAtSpecWidthNotTruncated 那条不引用 widthCaller 符号、
// 直接对 spec §8.7 示例行断言的用例。
func TestConsoleShortCallerRightAligned(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/x/a/b.go"
	ent.Caller.Line = 7

	out := encodeOne(t, false, ent, nil, nil)
	// 时间(12) + 空格(1) + level(widthLevel) + 空格(1) + trace(widthTrace) + 空格(1)
	// 是 caller 段开始的位置，caller 段本身宽 widthCaller。
	callerStart := len("10:23:45.123") + 1 + widthLevel + 1 + widthTrace + 1
	seg := out[callerStart : callerStart+widthCaller]
	assert.Equal(t, widthCaller, len(seg), "caller 段必须恰好占 widthCaller 宽")
	assert.Equal(t, "a/b.go:7", strings.TrimLeft(seg, " "), "短 caller 的内容")
	assert.Equal(t, strings.Repeat(" ", widthCaller-len("a/b.go:7"))+"a/b.go:7", seg,
		"短 caller 右对齐补空格，宽度必须恰好等于 widthCaller")
	assert.Equal(t, 24, widthCaller, "widthCaller 依据是 spec §8.7 表格，改动需要重新过评审")
}

// spec §8.7 表格的示例行 "payment/client.go:33" 恰好 20 个字符，卡在
// 24（不截断）与曾经错误采用的 19（会截断）两个候选宽度之间——这是这条
// 测试存在的全部意义：只要有人把 widthCaller 改回 19 或更小，这条 20
// 字符的 spec 示例行就会被截断出一个 "…"，测试必须能抓住这一步。
// 不引用 widthCaller 符号，直接对着已知的具体值断言，避免"改了常量、
// 断言跟着改"的自证陷阱。
func TestConsoleCallerAtSpecWidthNotTruncated(t *testing.T) {
	ent := sampleEntry()
	ent.Caller.File = "/src/payment/client.go"
	ent.Caller.Line = 33

	out := encodeOne(t, false, ent, nil, nil)
	assert.Contains(t, out, "    payment/client.go:33", "20 字符的 spec 示例行右对齐补 4 个空格，不截断")
	assert.NotContains(t, out, "…", "widthCaller 若被改回 19，这条 20 字符的路径就会被截断")
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

// writeConsoleValue 的反斜杠转义分支之前没有测试守护：把这个 case 删掉，
// 值里的反斜杠会被原样透传到引号内，破坏 grep/复制粘贴时的字面语义。
func TestConsoleEscapesBackslash(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.String("path", `C:\temp file`),
	})
	assert.Contains(t, out, `path="C:\\temp file"`, "反斜杠必须转义成两个字符")
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
//
// 之前的版本只断言"递归停下来了"（m.k=v 出现），删掉深度限制那两行代码
// 全部测试依然全绿——因为哪怕不限制深度，"m.k=v" 也会在第一层就产出，
// 后面是不是继续无限递归这条测试根本看不见。这里补上两条硬断言：
// 占位符确实在 maxConsoleDepth 层触顶时出现，且不会多递归一层。
func TestConsoleNestedDepthIsBounded(t *testing.T) {
	m := map[string]any{"k": "v"}
	m["self"] = m
	done := make(chan string, 1)
	go func() { done <- encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{zap.Any("m", m)}) }()
	select {
	case out := <-done:
		assert.Contains(t, out, "m.k=v")

		// 第 maxConsoleDepth 层触顶：full key 是 "m" 后面跟 maxConsoleDepth 个
		// ".self"，值必须是占位符而不是继续展开的子结构。
		atLimit := "m." + strings.Repeat("self.", maxConsoleDepth-1) + "self"
		assert.Contains(t, out, atLimit+"=<depth-limit>",
			"触顶后必须打印占位符，而不是悄悄丢弃或继续展开")

		// 不能比这再深一层——多一层 ".self" 说明深度限制没生效。
		assert.NotContains(t, out, atLimit+".self",
			"不能超过 maxConsoleDepth 层继续展开")
	case <-time.After(5 * time.Second):
		t.Fatal("展平递归没有停下来")
	}
}

// zap.ByteString 落进 MapObjectEncoder 时已经是 string（AddByteString 内部
// 做了 string(v)），会被 stringify 的 case string 分支接住，走不到 []byte
// 分支。这条测试断言的其实是 stringify 处理 string 的路径，不是 []byte 分支。
func TestConsoleRendersByteStringAsText(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.ByteString("body", []byte("hi")),
	})
	assert.Contains(t, out, "body=hi")
	assert.NotContains(t, out, "[104 105]")
}

// stringify 的 []byte 分支真正的入口是 zap.Binary（AddBinary 把值原样存成
// []byte，不像 AddByteString 那样先转成 string）。不特判就会落盘
// "[104 105]" 而不是 "hi"。
func TestConsoleRendersBinaryAsText(t *testing.T) {
	out := encodeOne(t, false, sampleEntry(), nil, []zapcore.Field{
		zap.Binary("body", []byte("hi")),
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

// Fix 2 回归：zapcore.ioCore.With 的实现是"Clone 一份 encoder，再把字段
// AddTo 进去"。zap.Namespace("db") 与后续字段分在两次 With 里时，
// Clone 必须让第二次的字段还能落进 db，而不是掉回顶层。
// 这里特意走 zap.New(...).With(...).With(...) 的真实路径，而不是直接
// 构造嵌套 map，因为触发条件正是"两次独立的 Clone 调用"。
func TestConsoleNamespacePreservedAcrossWith(t *testing.T) {
	var buf bytes.Buffer
	enc := newConsoleEncoder(false)
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel)
	l := zap.New(core).
		With(zap.Namespace("db")).
		With(zap.String("host", "10.0.0.1"))
	l.Info("连接建立")

	assert.Contains(t, buf.String(), "db.host=10.0.0.1", "跨 With 的字段必须留在 db 命名空间里")
	assert.NotContains(t, buf.String(), " host=10.0.0.1", "不能掉回顶层")
}

// 多层嵌套：With(Namespace("a")) → With(Namespace("b")) → With(String("k","v"))
// 要渲染成 a.b.k=v —— 验证 Clone 的命名空间重放不止支持一层。
func TestConsoleNestedNamespacePreservedAcrossMultipleWith(t *testing.T) {
	var buf bytes.Buffer
	enc := newConsoleEncoder(false)
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel)
	l := zap.New(core).
		With(zap.Namespace("a")).
		With(zap.Namespace("b")).
		With(zap.String("k", "v"))
	l.Info("m")

	assert.Contains(t, buf.String(), "a.b.k=v")
}

// clone 隔离：父 encoder 与 clone 各自往同一个已打开的命名空间里加字段，
// 互不出现在对方的输出里。Fields 若只是浅拷贝，clone 与父会共享同一个
// db 子 map，一方写入会串到另一方。
func TestConsoleCloneNamespaceIsolation(t *testing.T) {
	base := newConsoleEncoder(false)
	zap.Namespace("db").AddTo(base)
	zap.String("host", "10.0.0.1").AddTo(base)

	clone := base.Clone()
	zap.String("port", "5432").AddTo(clone)

	baseOut, err := base.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)
	cloneOut, err := clone.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)

	assert.Contains(t, baseOut.String(), "db.host=10.0.0.1")
	assert.NotContains(t, baseOut.String(), "db.port=5432", "clone 加的字段不能串回父 encoder")
	assert.Contains(t, cloneOut.String(), "db.host=10.0.0.1")
	assert.Contains(t, cloneOut.String(), "db.port=5432")
}

// TestConsoleCloneDeepCopiesNestedMap 钉住"Fields 的拷贝必须是递归深拷"这条，
// 且专门覆盖 TestConsoleCloneNamespaceIsolation 覆盖不到的路径。
//
// 已验证（mutation 测试）：TestConsoleCloneNamespaceIsolation 测的是
// zap.Namespace 打开的命名空间，而 Clone() 对命名空间的隔离其实是
// "重放 OpenNamespace" 这一步顺带做到的——OpenNamespace 每次都会造一个全新
// 的空 map，再把内容逐条搬进去，天然不会跟旧 map 共享对象，所以哪怕顶层
// Fields 拷贝退回浅拷贝，这条测试也不会失败（已用 mutation 验证：删掉
// deepCopyFields 换回浅拷贝，此测试仍然全绿）。
//
// 真正只有深拷贝才能保护的，是不经过 zap.Namespace、而是直接以嵌套
// map[string]any 值挂在某个 key 下的字段（例如 zap.Any("m", someMap)）——
// 这种子 map 不在 e.ns 记录的路径里，Clone 的命名空间重放逻辑碰不到它，
// 唯一能切断它与父 encoder 共享同一个 map 对象的手段就是深拷贝本身。
func TestConsoleCloneDeepCopiesNestedMap(t *testing.T) {
	base := newConsoleEncoder(false)
	shared := map[string]any{"k": "v"}
	zap.Any("m", shared).AddTo(base)

	clone := base.Clone()
	// Clone 之后才修改 shared——如果 clone 内部持有的是同一个 map 对象
	// （浅拷贝），这次修改会同时出现在 clone 的输出里；深拷贝下 clone
	// 在 Clone() 那一刻就已经拿到独立副本，不受影响。
	shared["leaked"] = "yes"

	baseOut, err := base.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)
	cloneOut, err := clone.EncodeEntry(sampleEntry(), nil)
	require.NoError(t, err)

	assert.Contains(t, baseOut.String(), "m.leaked=yes", "base 自己持有的就是 shared，改了会看见很正常")
	assert.NotContains(t, cloneOut.String(), "m.leaked=yes",
		"clone 必须在 Clone() 那一刻就拿到独立副本，Clone 之后再改 shared 不能泄露进 clone")
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
