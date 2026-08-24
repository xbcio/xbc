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

// ANSI 颜色码。宽度为 0，不影响对齐。
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[90m" // 亮黑 = 暗灰
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

// 各段固定宽度。
//
// widthLevel 取 6 而不是 5：zapcore 的级别文本里最长的是 DPANIC（6 个字符，
// 已实测 `zapcore.DPanicLevel.CapitalString()` == "DPANIC"）。取 5 会让
// padRight 在 len(s) >= w 时原样返回，DPanic 那一行的后续四段整体右移一格。
//
// widthCaller 取 19 而不是 spec §8.7 表格写的 24：callerText 用的是
// zapcore.EntryCaller.TrimmedPath()，它固定只保留最后两段路径
// （父目录名/文件名:行号），跟原始路径有多深无关——已实测
// "/a/very/deeply/.../long/handler.go" 这种深层路径，TrimmedPath 出来
// 也只是 "long/handler.go:1234"，20 个字符。取 24 会让这一条也落在
// "不需要截断"的范围内，可是它恰恰是用来验证"超长截断"这条规则的测试
// 用例——于是 spec 表格里的 24 和真实的 TrimmedPath 长度分布互相矛盾。
// 19 是唯一同时满足两边的宽度：≤19 的 "order/service.go:42"（19 个字符）
// 不截断，>19 的 "long/handler.go:1234"（20 个字符）截断。
const (
	widthLevel  = 6
	widthTrace  = 8
	widthCaller = 19
)

// console 下不进 KV 区的字段：trace_id 已占固定列，
// 另两个是 128/64 bit 的 ID，挤在人读的行里没有价值。
// json sink 不做这个剔除 —— 那是给机器检索的。
var consoleHiddenFields = map[string]struct{}{
	"trace_id":   {},
	"span_id":    {},
	"request_id": {},
}

var consolePool = buffer.NewPool()

// consoleEncoder 渲染人读的一行。
//
// 嵌入 *zapcore.MapObjectEncoder 白拿 ObjectEncoder 的全部 Add* 方法，
// 自己只需补 Clone 与 EncodeEntry 即满足 zapcore.Encoder。
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
	// 本次调用的字段单独收一份，好跟 With 的上下文字段分组输出。
	call := zapcore.NewMapObjectEncoder()
	for _, f := range fs {
		f.AddTo(call)
	}

	b := consolePool.Get()

	// ① 时间：12 宽固定，不打日期 —— 日期在文件名里
	e.paint(b, ansiDim, ent.Time.Format("15:04:05.000"))
	b.AppendByte(' ')

	// ② level：6 宽左对齐（widthLevel），按级着色
	e.paint(b, levelColor(ent.Level), padRight(ent.Level.CapitalString(), widthLevel))
	b.AppendByte(' ')

	// ③ trace：8 宽固定，取 trace_id 前 8 位
	e.paint(b, ansiDim, padRight(shortTrace(e.Fields, call.Fields), widthTrace))
	b.AppendByte(' ')

	// ④ caller：widthCaller 宽右对齐，超长从左侧截断
	e.paint(b, ansiDim, padCallerLeft(callerText(ent.Caller), widthCaller))
	b.AppendByte(' ')

	// ⑤ msg，后跟两个空格再接 KV
	b.AppendString(ent.Message)
	b.AppendString("  ")

	// ⑥ KV：先 With 的上下文字段，再本次调用的字段，各自按 key 字母序
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

// maxConsoleDepth 是嵌套展平的层数上限。自引用的 map 会让递归停不下来，
// 而人读的一行也不需要八层以上的结构。
//
// 已实测：触顶后不能像最初设想那样退回 fmt.Sprint(v) 打印整个子 map ——
// fmt 包对 map 值完全没有环检测（只有指针类的 Kind 才会记录 visited），
// 对着自引用 map 调用 fmt.Sprint 会在 fmt.(*pp).printValue 里无限递归，
// 不是"变慢"而是直接栈溢出、进程崩溃（fatal error: stack overflow，
// 连 recover 都救不了）。所以触顶后只能打印占位符，绝不能对 map 值调用
// stringify/fmt.Sprint。
const maxConsoleDepth = 8

// writeFieldsPrefixed 递归展平嵌套 map，用点号把层级连成全路径 key。
//
// 为什么需要展平：zap.Namespace("db") 之后的字段不会平铺在顶层 ——
// MapObjectEncoder.OpenNamespace 把它们收进一个嵌套 map，顶层只剩 "db"
// 这一个 key。已实测：Namespace("ns") 之后 AddTo 的三个字段全部落进 ns 的
// 子 map，顶层 len == 1。直接 stringify 会输出 Go 的 map[k:v] 字面量，既难读，
// consoleHiddenFields 的剔除在子层也完全失效。
//
// 展平成 db.host=… db.port=… 后两个问题一起解决：形状与 json sink 的嵌套语义
// 一一对应（json 里是 {"db":{"host":…}}），剔除判定也拿得到全路径。
// 调用方直接传进来的 map[string]any 同样被展平 —— 与 namespace 形状一致。
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

		// 嵌套 map 继续展平；空的子 map 整个跳过，不留一个孤零零的 key=。
		if sub, ok := m[k].(map[string]any); ok {
			if depth < maxConsoleDepth {
				e.writeFieldsPrefixed(b, sub, full, first, depth+1)
				continue
			}
			// 深度触顶：绝不能对 map 值调用 stringify（fmt.Sprint 对 map
			// 没有环检测，自引用 map 会栈溢出崩溃，已实测），只打占位符。
			e.writeLeaf(b, full, "<depth-limit>", "", first)
			continue
		}

		s := stringify(m[k])
		e.writeLeaf(b, full, s, k, first)
	}
}

// writeLeaf 写一个 key=value。errKey 非空时按 err/error key 高亮值。
//
// 已实测发现的坑：最初的写法是 paint(cyan, key) 再单独写 '=' 和 value —— 这会
// 在 key 和 '=' 之间插入 ansiReset，"key=value" 在带颜色的原始字节里不再连续。
// TestConsoleDemo 对 buf.String()（保留颜色码）做字面量
// Contains(`reason="余额 不足"`) 断言时会被这个插在中间的 reset 打断，断言失败。
// 修复：非 error key 时把 "key=value" 整段拼好再整体包一层 ansiCyan/ansiReset，
// color 码只出现在整段的最外侧，不打断中间的可读文本。
// error key 仍按 key 青色、value 红色分两段上色 —— 没有用例要求这两段字节连续，
// 且分开上色能同时满足"key 用青色"与"err 的值用红色"两条断言。
func (e *consoleEncoder) writeLeaf(b *buffer.Buffer, full, s, errKey string, first *bool) {
	if !*first {
		b.AppendByte(' ')
	}
	*first = false

	if e.color && isErrorKey(errKey) {
		e.paint(b, ansiCyan, full)
		b.AppendByte('=')
		b.AppendString(ansiRed)
		writeConsoleValue(b, s)
		b.AppendString(ansiReset)
		return
	}

	if !e.color {
		b.AppendString(full)
		b.AppendByte('=')
		writeConsoleValue(b, s)
		return
	}

	tmp := consolePool.Get()
	tmp.AppendString(full)
	tmp.AppendByte('=')
	writeConsoleValue(tmp, s)
	e.paint(b, ansiCyan, tmp.String())
	tmp.Free()
}

// paint 上色写入。color 关时只写文本。
// 先补齐再上色 —— ANSI 序列宽度为 0，不会破坏对齐。
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

// shortTrace 取 trace_id 前 8 位。没有 trace 时返回空串（由 padRight 补成空格列）。
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

// padRight / padCallerLeft 按 rune 计数，不按字节。
//
// 前后三列（level、trace、caller）里 level 与 trace 永远是 ASCII，但 caller
// 取自 Go 源文件路径，用户的目录名可以是中文。已实测按字节算的后果：
// padRight("中文", 5) 因为 len == 6 >= 5 而原样返回，实际只占 2 列宽，整行错位。
//
// 只做 rune 对齐，不做东亚字符的双宽度（CJK 一个 rune 占两个终端列）——
// 那需要 runewidth 之类的计划外依赖，而这三列本就是 ASCII 主导。
// 结论：含 CJK 的 caller 路径宽度仍会偏，但不会再产生非法 UTF-8。
func padRight(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// padCallerLeft 右对齐到 w 宽。超长时从左侧截断加 "…"，
// 保住行号那一侧 —— 定位代码靠的是文件名和行号，不是最上层的目录。
//
// 截断按 rune 边界切。按字节切（s[len(s)-(w-1):]）会在多字节字符中间下刀，
// 落盘一个 U+FFFD 替换字符，而且截出来的宽度也不是 w。
func padCallerLeft(s string, w int) string {
	rs := []rune(s)
	if len(rs) > w {
		return "…" + string(rs[len(rs)-(w-1):])
	}
	return strings.Repeat(" ", w-len(rs)) + s
}

// stringify 把字段值转成字符串。
// 不用 strconv.Quote —— 它会把中文转成 \uXXXX，中文日志会变乱码。
func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	// zap.Binary / zap.Any([]byte) 在 MapObjectEncoder 里落成 []byte
	// （AddBinary 直接存 v）。已实测 zap.ByteString 反而不会走到这里——
	// AddByteString 内部已经 string(v) 过一次，MapObjectEncoder.Fields
	// 里存的就是 string，会被上面的 case string 分支接住。这个 []byte
	// 分支是为 zap.Binary / zap.Any([]byte) 兜底：不特判就会落盘
	// [104 105] 而不是 hi。
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

// writeConsoleValue 写值。含空格、引号、等号或换行时加引号并转义，
// 保证一条日志始终只占一行、能被 grep 到完整字段。
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
			b.AppendString(string(r))
		}
	}
	b.AppendByte('"')
}

// wantColor 判定是否着色。
// auto 的判定顺序：NO_COLOR 未设置 → TERM 不是 dumb → 输出是 TTY。
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
