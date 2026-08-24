// mask_nested.go 处理"敏感值藏在值内部"的情况。
//
// mask.go 的 key 黑名单只看 Field.Key，而 zap 有四种 Field 会把真正的数据
// 装进值里：Inline / Object / Array marshaler，以及走反射序列化的 Reflect。
// 其中 zap.Inline 最危险 —— 它产出的 Field Key 是空串，AddTo 会把对象字段
// 摊进顶层命名空间，最终输出与 zap.String("password", ...) 逐字节无法区分，
// 却从未经过任何 key 检查。
//
// 对策是两条：marshaler 类的字段包一层过滤 encoder（转发全部写入，命中黑名单
// 的 key 写 *** 而非真值）；Reflect 类的值做一次反射遍历。
//
// # 防线覆盖不到的地方
//
// 以下四条不在本防线的覆盖范围内，读代码的人不要以为它是全覆盖的：
//
//  1. 敏感值写在 Entry.Message 里（log.L().Info("password=" + pwd)）——
//     字段级黑名单管不到消息体。
//  2. 类型自己实现了 MarshalJSON / MarshalText 并在其中输出敏感内容 ——
//     反射遍历会原样保留这类类型（否则 time.Time 会被拆成 wall/ext/loc 三个
//     未导出字段而输出全毁），那是调用方显式定制的序列化，由调用方负责。
//     注意只有这两个接口算数，fmt.Stringer 不算，理由见 hasSelfMarshal。
//  3. 深度超过 maxMaskDepth 的子树被整体替换为 ***，这是有意的信息损失，
//     用来防御恶意或病态的深嵌套，不是性能优化。maxMaskDepth 数的是结构嵌套
//     层数，指针解引用与 interface 拆箱不计入。
//  4. 直传给 zap.Any 的 fmt.Stringer / error —— zap.Any 的类型 switch 里
//     这两个分支排在 Reflect 之前，值会走 StringerType / ErrorType，落盘的是
//     String() / Error() 的结果，压根进不了本文件。与第 2 条同类：调用方显式
//     决定了输出形式。字段 key 仍然受检，zap.Any("password", stringerValue)
//     拦得住；拦不住的是 zap.Any("creds", v) 里 v.String() 自己吐出口令。
//
// 匿名嵌入字段按 encoding/json 的规则平铺进父层（无 json 名字 + 解一层指针后是
// struct 才平铺），冲突时外层优先。这一点必须与 json 对齐：形状只在命中时才变，
// 恰好是最需要日志形状稳定的时候 —— {"Base":{"password":"***"}} 会让针对
// .password 的检索规则失效。多路同深度冲突不做 encoding/json 那套完整消歧，
// 先出现的嵌入胜出。
//
// # 与 encoding/json 对齐时的注意事项
//
// Go 1.27 起 GOEXPERIMENT 默认打开 jsonv2，encoding/json 的实现换成了
// encoding/json/v2 + jsontext（encode.go 里的 newMapEncoder / typeFields /
// isValidTag 已经不参与编译）。本文件的对齐依据一律以**实测落盘字节**为准，
// 不以 v1 源码为准 —— 两者在 map key、非法 tag 名两处的行为并不相同。

package log

import (
	"encoding"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.uber.org/zap/zapcore"
)

// maxMaskDepth 是嵌套脱敏的深度上限，超过即把该子树整体写成 maskPlaceholder。
const maxMaskDepth = 8

// ---------------------------------------------------------------------------
// marshaler 包装
// ---------------------------------------------------------------------------

// maskObjectMarshaler 把内层 marshaler 的写入引到过滤 encoder 上。
type maskObjectMarshaler struct {
	om    zapcore.ObjectMarshaler
	m     *masker
	depth int
}

func (w *maskObjectMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	return w.om.MarshalLogObject(&maskObjectEncoder{enc: enc, m: w.m, depth: w.depth})
}

// maskArrayMarshaler 同上，用于数组。
type maskArrayMarshaler struct {
	am    zapcore.ArrayMarshaler
	m     *masker
	depth int
}

func (w *maskArrayMarshaler) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	return w.am.MarshalLogArray(&maskArrayEncoder{enc: enc, m: w.m, depth: w.depth})
}

// ---------------------------------------------------------------------------
// 过滤 encoder
// ---------------------------------------------------------------------------

// maskObjectEncoder 转发所有写入，命中黑名单的 key 写 *** 而非真值。
//
// 用命名字段 enc 而非嵌入 zapcore.ObjectEncoder：嵌入意味着 zap 将来给接口
// 加方法时，新方法会被静默转发到内层 encoder —— 一条不会报错的绕过路径。
// 显式实现全部方法则编译报错，逼着人来补。
type maskObjectEncoder struct {
	enc   zapcore.ObjectEncoder
	m     *masker
	depth int
}

var _ zapcore.ObjectEncoder = (*maskObjectEncoder)(nil)

// AddArray：key 命中整体替换，否则递归包装并加深一层。
func (e *maskObjectEncoder) AddArray(k string, v zapcore.ArrayMarshaler) error {
	if e.m.hit(k) || e.depth >= maxMaskDepth {
		e.enc.AddString(k, maskPlaceholder)
		return nil
	}
	return e.enc.AddArray(k, &maskArrayMarshaler{am: v, m: e.m, depth: e.depth + 1})
}

// AddObject：key 命中整体替换，否则递归包装并加深一层。
func (e *maskObjectEncoder) AddObject(k string, v zapcore.ObjectMarshaler) error {
	if e.m.hit(k) || e.depth >= maxMaskDepth {
		e.enc.AddString(k, maskPlaceholder)
		return nil
	}
	return e.enc.AddObject(k, &maskObjectMarshaler{om: v, m: e.m, depth: e.depth + 1})
}

// AddReflected：key 命中整体替换，否则值先走一遍反射遍历。
func (e *maskObjectEncoder) AddReflected(k string, v any) error {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return nil
	}
	if nv, changed := e.m.maskReflected(v, e.depth+1); changed {
		return e.enc.AddReflected(k, nv)
	}
	return e.enc.AddReflected(k, v)
}

// OpenNamespace 原样转发：命名空间内的 key 仍然会经过本 encoder 的 Add*，
// 天然被覆盖，不需要额外处理。
func (e *maskObjectEncoder) OpenNamespace(k string) { e.enc.OpenNamespace(k) }

func (e *maskObjectEncoder) AddBinary(k string, v []byte) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddBinary(k, v)
}

func (e *maskObjectEncoder) AddByteString(k string, v []byte) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddByteString(k, v)
}

func (e *maskObjectEncoder) AddBool(k string, v bool) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddBool(k, v)
}

func (e *maskObjectEncoder) AddComplex128(k string, v complex128) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddComplex128(k, v)
}

func (e *maskObjectEncoder) AddComplex64(k string, v complex64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddComplex64(k, v)
}

func (e *maskObjectEncoder) AddDuration(k string, v time.Duration) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddDuration(k, v)
}

func (e *maskObjectEncoder) AddFloat64(k string, v float64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddFloat64(k, v)
}

func (e *maskObjectEncoder) AddFloat32(k string, v float32) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddFloat32(k, v)
}

func (e *maskObjectEncoder) AddInt(k string, v int) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt(k, v)
}

func (e *maskObjectEncoder) AddInt64(k string, v int64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt64(k, v)
}

func (e *maskObjectEncoder) AddInt32(k string, v int32) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt32(k, v)
}

func (e *maskObjectEncoder) AddInt16(k string, v int16) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt16(k, v)
}

func (e *maskObjectEncoder) AddInt8(k string, v int8) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddInt8(k, v)
}

func (e *maskObjectEncoder) AddString(k, v string) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddString(k, v)
}

func (e *maskObjectEncoder) AddTime(k string, v time.Time) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddTime(k, v)
}

func (e *maskObjectEncoder) AddUint(k string, v uint) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint(k, v)
}

func (e *maskObjectEncoder) AddUint64(k string, v uint64) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint64(k, v)
}

func (e *maskObjectEncoder) AddUint32(k string, v uint32) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint32(k, v)
}

func (e *maskObjectEncoder) AddUint16(k string, v uint16) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint16(k, v)
}

func (e *maskObjectEncoder) AddUint8(k string, v uint8) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUint8(k, v)
}

func (e *maskObjectEncoder) AddUintptr(k string, v uintptr) {
	if e.m.hit(k) {
		e.enc.AddString(k, maskPlaceholder)
		return
	}
	e.enc.AddUintptr(k, v)
}

// maskArrayEncoder 是数组版本。数组元素没有 key，标量直接转发；
// 只有能再套一层结构的三个方法（Object / Array / Reflected）需要过滤。
type maskArrayEncoder struct {
	enc   zapcore.ArrayEncoder
	m     *masker
	depth int
}

var _ zapcore.ArrayEncoder = (*maskArrayEncoder)(nil)

func (e *maskArrayEncoder) AppendArray(v zapcore.ArrayMarshaler) error {
	if e.depth >= maxMaskDepth {
		e.enc.AppendString(maskPlaceholder)
		return nil
	}
	return e.enc.AppendArray(&maskArrayMarshaler{am: v, m: e.m, depth: e.depth + 1})
}

func (e *maskArrayEncoder) AppendObject(v zapcore.ObjectMarshaler) error {
	if e.depth >= maxMaskDepth {
		e.enc.AppendString(maskPlaceholder)
		return nil
	}
	return e.enc.AppendObject(&maskObjectMarshaler{om: v, m: e.m, depth: e.depth + 1})
}

func (e *maskArrayEncoder) AppendReflected(v any) error {
	if nv, changed := e.m.maskReflected(v, e.depth+1); changed {
		return e.enc.AppendReflected(nv)
	}
	return e.enc.AppendReflected(v)
}

func (e *maskArrayEncoder) AppendBool(v bool)             { e.enc.AppendBool(v) }
func (e *maskArrayEncoder) AppendByteString(v []byte)     { e.enc.AppendByteString(v) }
func (e *maskArrayEncoder) AppendComplex128(v complex128) { e.enc.AppendComplex128(v) }
func (e *maskArrayEncoder) AppendComplex64(v complex64)   { e.enc.AppendComplex64(v) }
func (e *maskArrayEncoder) AppendDuration(v time.Duration) {
	e.enc.AppendDuration(v)
}
func (e *maskArrayEncoder) AppendFloat64(v float64) { e.enc.AppendFloat64(v) }
func (e *maskArrayEncoder) AppendFloat32(v float32) { e.enc.AppendFloat32(v) }
func (e *maskArrayEncoder) AppendInt(v int)         { e.enc.AppendInt(v) }
func (e *maskArrayEncoder) AppendInt64(v int64)     { e.enc.AppendInt64(v) }
func (e *maskArrayEncoder) AppendInt32(v int32)     { e.enc.AppendInt32(v) }
func (e *maskArrayEncoder) AppendInt16(v int16)     { e.enc.AppendInt16(v) }
func (e *maskArrayEncoder) AppendInt8(v int8)       { e.enc.AppendInt8(v) }
func (e *maskArrayEncoder) AppendString(v string)   { e.enc.AppendString(v) }
func (e *maskArrayEncoder) AppendTime(v time.Time)  { e.enc.AppendTime(v) }
func (e *maskArrayEncoder) AppendUint(v uint)       { e.enc.AppendUint(v) }
func (e *maskArrayEncoder) AppendUint64(v uint64)   { e.enc.AppendUint64(v) }
func (e *maskArrayEncoder) AppendUint32(v uint32)   { e.enc.AppendUint32(v) }
func (e *maskArrayEncoder) AppendUint16(v uint16)   { e.enc.AppendUint16(v) }
func (e *maskArrayEncoder) AppendUint8(v uint8)     { e.enc.AppendUint8(v) }
func (e *maskArrayEncoder) AppendUintptr(v uintptr) { e.enc.AppendUintptr(v) }

// ---------------------------------------------------------------------------
// 反射遍历
// ---------------------------------------------------------------------------

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// hasSelfMarshal 判断类型是否自带序列化。这类类型必须原样保留 ——
// 否则 time.Time 会被拆成 wall/ext/loc 三个未导出字段，输出全毁。
//
// 只查 json.Marshaler 与 encoding.TextMarshaler 两个接口，**不查 fmt.Stringer**。
// 这不是漏了一个，别顺手补上：zap 对 ReflectType 走 json 编码，而 encoding/json
// 只认这两个接口，压根不看 String()。把只实现了 String() 的类型判成"自带序列化"
// 就等于主动跳过遍历，随后 json 照样把它逐字段摊开 —— 纯粹的漏放，没有任何补偿
// 收益。time.Time 由 MarshalJSON / MarshalText 挡住，net.IP、uuid.UUID 走
// MarshalText，time.Duration、zapcore.Level 是标量 Kind 根本不进 struct 分支。
//
// 同理只查类型自身的方法集，不查 reflect.PointerTo(t)。指针接收者的方法不在值
// 类型的方法集里，encoding/json 对不可寻址的值同样不会调用它们 —— 如果这里认了，
// 一个只在指针上实现 MarshalJSON 的类型按值打日志时会被判为"自带序列化"而跳过
// 遍历，同样形成绕过路径。
func hasSelfMarshal(t reflect.Type) bool {
	if t == nil {
		return false
	}
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType)
}

// maskReflected 递归脱敏任意值。changed 为 false 时调用方必须用原值，不要用返回值。
//
// 无命中时一个字节都不拷贝：递归一路返回 changed=false，原值原样交回。
// 绝大多数 struct 不含敏感字段，这条路径必须零开销 —— 与 apply 的
// copy-on-first-hit 是同一个设计。
func (m *masker) maskReflected(v any, depth int) (out any, changed bool) {
	if v == nil {
		return v, false
	}
	var seen map[uintptr]struct{}
	nv, ch := m.maskValue(reflect.ValueOf(v), depth, &seen)
	if !ch {
		return v, false
	}
	return nv, true
}

// maskValue 是 maskReflected 的递归体。seen 记录递归路径上已访问的指针地址，
// 只在真正递归到指针时才分配，无嵌套指针时保持 nil。
//
// depth 只数**结构嵌套层数**：指针解引用与 interface 拆箱都是间接层，不是嵌套层，
// 不递增 depth。否则 maxMaskDepth 的实际含义会随值的表示方式漂移 ——
// map[string]any 每层要吃掉 2 个额度（Interface + Map），8 只剩 4 层可用。
// 不递增是安全的：循环引用由 seen 独立挡住，从不依赖 depth 兜底；
// interface 装箱最终必须装一个具体类型，不存在无限间接。
func (m *masker) maskValue(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if !rv.IsValid() {
		return nil, false
	}

	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Struct, reflect.Map,
		reflect.Slice, reflect.Array:
	default:
		// 标量、chan、func 等没有可深入的内部结构
		return nil, false
	}

	if hasSelfMarshal(rv.Type()) {
		return nil, false
	}
	if depth > maxMaskDepth {
		return maskPlaceholder, true
	}

	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, false
		}
		addr := rv.Pointer()
		if *seen == nil {
			*seen = make(map[uintptr]struct{}, 4)
		}
		if _, dup := (*seen)[addr]; dup {
			return maskPlaceholder, true
		}
		(*seen)[addr] = struct{}{}
		nv, ch := m.maskValue(rv.Elem(), depth, seen)
		delete(*seen, addr)
		return nv, ch

	case reflect.Interface:
		if rv.IsNil() {
			return nil, false
		}
		return m.maskValue(rv.Elem(), depth, seen)

	case reflect.Struct:
		return m.maskStruct(rv, depth, seen)

	case reflect.Map:
		return m.maskMap(rv, depth, seen)

	case reflect.Slice, reflect.Array:
		return m.maskSlice(rv, depth, seen)
	}

	return nil, false
}

// maskStruct 遍历导出字段。字段名取 json tag 的名字部分，无 tag 则用 Go 字段名。
// 没有任何导出字段的 struct 原样返回（time.Time 之外的同类兜底）。
//
// 分两遍：先本层命名字段，再平铺的匿名嵌入字段（合并时不覆盖已有 key）。
// 顺序不能反 —— 这就是"外层优先"，与 encoding/json 的浅层胜出一致。
func (m *masker) maskStruct(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	t := rv.Type()
	n := t.NumField()
	var out map[string]any

	// 第一遍：本层命名字段。
	for i := range n {
		sf := t.Field(i)
		if !maskNamedField(sf) {
			continue
		}
		name := maskFieldName(sf)
		if name == "" { // json:"-"
			continue
		}
		fv := rv.Field(i)

		if m.hitFieldName(sf, name) {
			if out == nil {
				out = maskNamedRaw(rv)
			}
			out[name] = maskPlaceholder
			continue
		}
		if !fv.CanInterface() {
			// 未导出的匿名 struct 嵌入（json tag 给了名字，所以不平铺）。
			// 它是唯一一类"json 会收录、reflect 却不给 Interface()"的字段，
			// 无条件走重建路径 —— 下面"原样返回"那条路必须 Interface()，走不通。
			if out == nil {
				out = maskNamedRaw(rv)
			}
			out[name] = m.maskUnexported(fv, depth+1, seen)
			continue
		}
		nv, ch := m.maskValue(fv, depth+1, seen)
		if ch {
			if out == nil {
				out = maskNamedRaw(rv)
			}
			out[name] = nv
		}
	}

	// 第二遍：平铺的匿名嵌入字段。
	for i := range n {
		sf := t.Field(i)
		if !maskEmbedFlatten(sf) {
			continue
		}
		fv := rv.Field(i)
		if fv.Kind() == reflect.Pointer && fv.IsNil() {
			continue // nil 嵌入指针：encoding/json 同样不产出任何键
		}

		nv, ch := m.maskEmbedStruct(fv, depth, seen)
		if !ch {
			if out != nil {
				maskStructRaw(out, fv, maxMaskDepth)
			}
			continue
		}
		if out == nil {
			out = maskNamedRaw(rv)
			for j := range i { // 本字段之前的嵌入字段原样补上
				if maskEmbedFlatten(t.Field(j)) {
					maskStructRaw(out, rv.Field(j), maxMaskDepth)
				}
			}
		}
		if sub, isMap := nv.(map[string]any); isMap {
			for k, v := range sub {
				if _, dup := out[k]; !dup {
					out[k] = v
				}
			}
			continue
		}
		// 循环引用等退化情况：平铺不了，按类型名挂上去。
		if _, dup := out[sf.Name]; !dup {
			out[sf.Name] = nv
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}

// maskNamedField 判断一个字段是否按"本层命名字段"处理，规则与 encoding/json
// 对齐（实测 Go 1.27 落盘字节，v1 的 typeFields 注释也是同一个意思）：
//
//   - 平铺的匿名嵌入不算命名字段，走第二遍；
//   - 导出字段都算；
//   - 未导出字段里，只有**匿名且解一层指针后是 struct**的那种算 —— json 的
//     跳过条件是"未导出**且**类型非 struct"，因为未导出的 struct 类型里可能有
//     导出字段。type S struct{ sHidden `json:"base"` } 就落在这里，实测
//     json.Marshal 产出 {"base":{"password":"…"}}。
//
// 此前这里无条件跳过所有未导出字段，比 json 严 —— 严在这里等于漏放：json 照样
// 把里面的 password 落盘，我们却根本没去看。
func maskNamedField(sf reflect.StructField) bool {
	if maskEmbedFlatten(sf) {
		return false
	}
	if sf.IsExported() {
		return true
	}
	if !sf.Anonymous {
		return false // 未导出的具名字段，json 直接忽略
	}
	t := sf.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// hitFieldName 查黑名单时用**多个候选名**，任一命中即脱敏。
//
// 起因：tag 名非法时（`json:"pass\"word"` 这种）encoding/json 不会原样采用它，
// 我们却拿着 tag 原名去查黑名单，必然不命中 —— 而字段值是货真价实的口令。
// 与其预测它用哪个名字输出，不如把它可能用的名字全挡住：
//
//  1. tag 的名字部分（也是我们输出用的成员名）
//  2. Go 字段名 —— v1 校验失败时用它，v2 在 tag 首字符非法时也用它
//  3. 截断名：tag 名截到第一个保留字符（反斜杠、单引号、双引号、反引号）之前
//  4. v2 名：从 tag 开头取的最长合法标识符前缀
//
// 3 与 4 的关系不是包含而是并集，两个方向的漏网都实测过：
//
//	json:"access_token-extra\y" → 截断名 access_token-extra 不命中，v2 名
//	                              access_token 命中（v2 落盘的就是这个成员名）
//	json:"db.password\"x"       → v2 名 db 不命中，截断名 db.password 命中
//
// 误伤面是可控的：Token string `json:"count"` 会因 Go 名命中而脱敏（值大概率
// 真是 token，脱敏是对的），而 TokenCount int `json:"n"` 的后缀词组是
// count / tokencount，几个候选都不命中。
func (m *masker) hitFieldName(sf reflect.StructField, name string) bool {
	if m.hit(name) {
		return true
	}
	if name != sf.Name && m.hit(sf.Name) {
		return true
	}
	trunc, v2 := maskTagAltNames(sf.Tag.Get("json"))
	if trunc != name && m.hit(trunc) {
		return true
	}
	return v2 != trunc && v2 != name && m.hit(v2)
}

// maskTagReserved 是 encoding/json v2 在 tag 名字部分保留的字符集，
// 逐字节抄自 $GOROOT/src/encoding/json/v2/fields.go 的 parseFieldOptions。
const maskTagReserved = ",\\'\"`"

// maskTagAltNames 返回 tag 名字部分之外的两个查名候选。tag 名合法（没有被保留
// 字符截断）时两者都是空串 —— 那种情况下 v1 与 v2 用的都是候选 1。
//
// v2 的规则（parseFieldOptions 435-462 → consumeTagOption 576-640）：名字先截到
// 第一个保留字符；若截断处不是逗号，则丢弃这个名字，从 tag 开头重新解析一个
// Go 标识符 —— 首 rune 是 '_' 或 unicode.IsLetter 时取最长的 isLetterOrDigit
// 前缀，否则报错、成员名退回 Go 字段名（已由候选 2 覆盖）。
//
// isLetterOrDigit 不含 '-' 和 '.'，所以 v2 名可能比截断名短得多；而 unicode
// 的字母判定含 CJK（`json:"密码\"x"` 实测落盘成员就是 密码），不能拿 ASCII
// 范围硬判。
func maskTagAltNames(tag string) (trunc, v2 string) {
	if tag == "" || strings.HasPrefix(tag, ",") {
		return "", ""
	}
	n := strings.IndexAny(tag, maskTagReserved)
	if n < 0 || tag[n] == ',' {
		return "", "" // 名字完整，或止于逗号 —— v1/v2 都原样采用
	}
	trunc = tag[:n]
	if r, _ := utf8.DecodeRuneInString(tag); r == '_' || unicode.IsLetter(r) {
		v2 = tag[:len(tag)-len(strings.TrimLeftFunc(tag, maskIsLetterOrDigit))]
	}
	return trunc, v2
}

// maskIsLetterOrDigit 与 encoding/json/v2 的 isLetterOrDigit 同义。
func maskIsLetterOrDigit(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// maskUnexported 处理拿不到 Interface() 的字段值，也就是未导出的匿名 struct
// 嵌入（带 json 名字 tag 因而不平铺的那种）。
//
// reflect 的只读标记只加在这一层：flagEmbedRO 不会传给它的导出子字段（这正是
// 提升方法能被调用的原因），所以按 json 的形状逐字段重建一个 map 就够了，
// 不需要 unsafe —— 为了读一个未导出字段而在安全关键代码里引入 unsafe.Pointer，
// 代价大于收益。
//
// 代价是形状固定为逐字段展开的对象：这类字段永远走重建路径，即使子树里一个
// 敏感字段都没有。若嵌入类型自带 MarshalJSON，encoding/json 会用它的自定义
// 形式而我们用不了（方法调不到只读值上），只能逐字段展开 —— 这是本函数与
// json 唯一的形状偏差，只落在"未导出 + 匿名 + 带 json 名字 tag + 自带序列化"
// 这一种形态上。
func (m *masker) maskUnexported(fv reflect.Value, depth int, seen *map[uintptr]struct{}) any {
	if depth > maxMaskDepth {
		return maskPlaceholder
	}
	if fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return nil // json 输出 null
		}
		// type a struct{ *a `json:"base"` } 是合法的，自引用得挡住。
		addr := fv.Pointer()
		if *seen == nil {
			*seen = make(map[uintptr]struct{}, 4)
		}
		if _, dup := (*seen)[addr]; dup {
			return maskPlaceholder
		}
		(*seen)[addr] = struct{}{}
		defer delete(*seen, addr)
		fv = fv.Elem()
	}
	if fv.Kind() != reflect.Struct {
		return maskPlaceholder // maskNamedField 保证到不了
	}
	if nv, ch := m.maskStruct(fv, depth, seen); ch {
		return nv
	}
	// maskStruct 说整棵子树一个字段都没命中，原样重建是安全的。
	out := make(map[string]any, fv.NumField())
	maskStructRaw(out, fv, maxMaskDepth)
	return out
}

// maskEmbedFlatten 判断一个字段是否按 encoding/json 的规则平铺进父层：
// 匿名嵌入、json tag 没给名字、且（解一层指针后）是 struct。
//
// json tag 给了名字（含 json:"-"）就按普通命名字段处理；嵌入非 struct 类型
// （type Req struct{ MyInt }）也按普通命名字段处理，key 用类型名。
// 嵌入类型未导出不影响平铺 —— encoding/json 照样提升它的导出字段，
// 反射对这些字段的 CanInterface 也是 true，不跟着平铺就会漏掉里面的敏感字段。
func maskEmbedFlatten(sf reflect.StructField) bool {
	if !sf.Anonymous {
		return false
	}
	if tag, ok := sf.Tag.Lookup("json"); ok {
		if name, _, _ := strings.Cut(tag, ","); name != "" {
			return false
		}
	}
	t := sf.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// maskEmbedStruct 展开一个平铺的嵌入字段，depth 不递增 —— 平铺后的字段与父层
// 同处一个结构层。
//
// 直接走 maskStruct 而不经 maskValue：encoding/json 决定平铺时只看类型形状，
// 不看嵌入类型自身有没有 MarshalJSON —— 那个方法要么被提升到外层、在
// maskValue 入口就把整个外层挡住了，要么因多路歧义而根本不会被调用。
// 经 maskValue 会被 hasSelfMarshal 拦下，与 json 的实际输出对不上。
func (m *masker) maskEmbedStruct(fv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if fv.Kind() != reflect.Pointer {
		return m.maskStruct(fv, depth, seen)
	}
	// type A struct{ *A } 是合法的，自引用得挡住。
	addr := fv.Pointer()
	if *seen == nil {
		*seen = make(map[uintptr]struct{}, 4)
	}
	if _, dup := (*seen)[addr]; dup {
		return maskPlaceholder, true
	}
	(*seen)[addr] = struct{}{}
	nv, ch := m.maskStruct(fv.Elem(), depth, seen)
	delete(*seen, addr)
	return nv, ch
}

// maskNamedRaw 在第一次命中时才建 map，把本层的命名字段（不含平铺嵌入）原样搬进去。
// 只在首次命中调用 —— 此前没有任何字段被改写过，整份原样搬是安全的。
func maskNamedRaw(rv reflect.Value) map[string]any {
	t := rv.Type()
	n := t.NumField()
	out := make(map[string]any, n)
	for i := range n {
		sf := t.Field(i)
		if !maskNamedField(sf) {
			continue
		}
		name := maskFieldName(sf)
		if name == "" {
			continue
		}
		fv := rv.Field(i)
		if !fv.CanInterface() {
			// 未导出的匿名 struct 嵌入。这里不预先物化它未脱敏的原样值 ——
			// maskStruct 对这类字段无条件走重建路径，一定会补上。
			continue
		}
		out[name] = fv.Interface()
	}
	return out
}

// maskStructRaw 把 rv 按平铺后的形状原样写进 out：先命名字段再嵌入字段，
// 已存在的 key 不覆盖。budget 只是兜底 —— 真正的自引用嵌入会在 maskEmbedStruct
// 里被 seen 判成 changed 而走不到这条原样路径，这里不依赖它保正确性。
//
// 只在"整棵子树都没命中"时才会走到这里（调用方拿到的是 changed==false），
// 所以原样搬运不会物化任何敏感值。
func maskStructRaw(out map[string]any, rv reflect.Value, budget int) {
	if budget <= 0 {
		return
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return
	}
	t := rv.Type()
	n := t.NumField()
	for i := range n {
		sf := t.Field(i)
		if !maskNamedField(sf) {
			continue
		}
		name := maskFieldName(sf)
		if name == "" {
			continue
		}
		if _, dup := out[name]; dup {
			continue
		}
		fv := rv.Field(i)
		if !fv.CanInterface() {
			// 未导出的匿名 struct 嵌入。上面那条"子树无命中"的前提保证走不到
			// 这里：maskStruct 对这类字段无条件 changed=true，调用方就不会
			// 选原样路径。真走到了也只是少一个键，不会 panic、更不会漏放。
			continue
		}
		out[name] = fv.Interface()
	}
	for i := range n {
		if maskEmbedFlatten(t.Field(i)) {
			maskStructRaw(out, rv.Field(i), budget-1)
		}
	}
}

// maskFieldName 取字段在日志里的名字：json tag 的名字部分优先，否则 Go 字段名。
// 返回空串表示该字段被 json:"-" 排除。
func maskFieldName(sf reflect.StructField) string {
	tag, ok := sf.Tag.Lookup("json")
	if !ok {
		return sf.Name
	}
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return sf.Name
	}
	return name
}

// maskMap 递归 map。string key 走零分配的快路径，其余 key 类型先按
// encoding/json 的规则字符串化再走同一套判定。
//
// 曾经这里对非 string key 直接放行，连 value 递归一起跳过了 —— 那是基于
// "encoding/json 处理不了非 string key 的 map"这个未经验证的假设。它处理得很好：
// map[int64]Order / map[uint64]User 是 Go 里最常见的按 ID 索引写法，实测落盘
// {"7":{"user":"alice","password":"hunter2"}}，整块明文。
func (m *masker) maskMap(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if rv.IsNil() {
		return nil, false
	}
	kt := rv.Type().Key()
	if kt.Kind() == reflect.String && !kt.Implements(textMarshalerType) {
		return m.maskStringKeyMap(rv, depth, seen)
	}
	if !maskMapKeyEncodable(kt) {
		// 这类 key encoding/json 编不出成员名，会报 unsupported value，
		// zap 把整个字段记成 "<key>Error":"json: unsupported value: …"，
		// 不落盘明文（实测 map[bool]X / map[struct]X 都是如此）。原样交回即可。
		return nil, false
	}
	return m.maskOtherKeyMap(rv, depth, seen)
}

// maskStringKeyMap 是 string key 的快路径：key 不用转换，无命中时一个字节都不拷贝。
func (m *masker) maskStringKeyMap(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	var out map[string]any
	snapshot := func() map[string]any {
		// map 迭代顺序随机，"已处理的前缀"无从谈起，第一次命中就整份快照。
		dst := make(map[string]any, rv.Len())
		it := rv.MapRange()
		for it.Next() {
			dst[it.Key().String()] = it.Value().Interface()
		}
		return dst
	}

	iter := rv.MapRange()
	for iter.Next() {
		k := iter.Key().String()
		if m.hit(k) {
			if out == nil {
				out = snapshot()
			}
			out[k] = maskPlaceholder
			continue
		}
		nv, ch := m.maskValue(iter.Value(), depth+1, seen)
		if ch {
			if out == nil {
				out = snapshot()
			}
			out[k] = nv
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}

// maskOtherKeyMap 处理非 string key 的 map，输出 map[string]any。
//
// 形状不变：map[int]X 本来就序列化成 {"1":{…}}，转成 map[string]any{"1":…}
// 序列化还是 {"1":{…}}。零拷贝原则照旧 —— 只在第一次命中时才整份快照。
//
// 任何一个 key 字符串化失败就整块原样交回：encoding/json 对 map 是全有全无的，
// 一个 key 编不出成员名，整个字段就编码失败记成 <key>Error。这里跟着它走，
// 是因为我们判断不了那个 key，而不是因为形状必须一致。
//
// 形状本来就不保证一致：map[any]X{1.5: …} 的 key 我们编得出来（"1.5"），
// encoding/json 却报 unsupported value（interface 装箱的 float 走不通它的
// map key 路径），于是命中脱敏时我们输出 {"1.5":{"password":"***"}}，而未脱敏
// 的同一份数据落盘是 vError。本函数保证的是不泄露明文，不保证与未脱敏时的
// json.Marshal 结果同形。
func (m *masker) maskOtherKeyMap(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	var out map[string]any
	snapshot := func() (map[string]any, bool) {
		dst := make(map[string]any, rv.Len())
		it := rv.MapRange()
		for it.Next() {
			name, ok := maskMapKeyName(it.Key())
			if !ok {
				return nil, false
			}
			dst[name] = it.Value().Interface()
		}
		return dst, true
	}

	iter := rv.MapRange()
	for iter.Next() {
		name, ok := maskMapKeyName(iter.Key())
		if !ok {
			return nil, false
		}
		// 字符串化后的 key 统一查一次黑名单：hit 走零分配路径，成本可忽略，
		// 收益是 key 类型的 MarshalText 产出有意义字符串时（比如一个
		// HeaderName 类型产出 "authorization"）能挡住。
		if m.hitMapKey(iter.Key(), name) {
			if out == nil {
				if out, ok = snapshot(); !ok {
					return nil, false
				}
			}
			out[name] = maskPlaceholder
			continue
		}
		nv, ch := m.maskValue(iter.Value(), depth+1, seen)
		if ch {
			if out == nil {
				if out, ok = snapshot(); !ok {
					return nil, false
				}
			}
			out[name] = nv
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}

// hitMapKey 查 map 成员名是否需要脱敏。除了成员名本身，还补查一种情况：
// 底层是 string 又自带 MarshalText 的 key 类型，jsonv2 用 MarshalText 的结果
// 当成员名，jsonv1 用原串 —— 两个名字都挡住，免得换个工具链就漏一条。
func (m *masker) hitMapKey(k reflect.Value, name string) bool {
	if m.hit(name) {
		return true
	}
	if k.Kind() == reflect.Interface && !k.IsNil() {
		k = k.Elem()
	}
	if k.Kind() != reflect.String {
		return false
	}
	raw := k.String()
	return raw != name && m.hit(raw)
}

// maskMapKeyEncodable 判断这类 key 有没有可能编出 JSON 成员名。
// 只做类型级筛查，具体到某个 key 能不能编（NaN、MarshalText 报错、
// map[any]V 里装了个 bool）由 maskMapKeyName 逐个判定。
func maskMapKeyEncodable(t reflect.Type) bool {
	if t.Implements(textMarshalerType) {
		return true
	}
	switch t.Kind() {
	case reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Uintptr, reflect.Float32, reflect.Float64,
		reflect.Interface: // map[any]V 合法，实测 {1:…} 落盘成 {"1":…}
		return true
	}
	return false
}

// maskMapKeyName 把 map key 转成它在 JSON 对象里的成员名，ok=false 表示编不出来。
//
// 规则以 Go 1.27 的实测落盘字节为准（此时 encoding/json 由 json/v2 实现，
// v1 的 resolveKeyName 已不参与编译，两者并不等价）：
//
//   - interface 先拆箱，按动态类型再判一次；
//   - 实现 encoding.TextMarshaler 的用 MarshalText 的结果，**优先于 Kind** ——
//     v2 连底层是 string 的类型也走 MarshalText，v1 则是 string 优先。分歧只
//     影响成员名的写法，查黑名单时下面会把原串也补查一次；
//   - 整数十进制，浮点按 JSON 数值格式；
//   - key 类型自己的 MarshalJSON **不算**：实测 v2 在成员名位置不调用它，
//     落盘的是底层数值。
func maskMapKeyName(k reflect.Value) (string, bool) {
	if k.Kind() == reflect.Interface {
		if k.IsNil() {
			return "", false
		}
		return maskMapKeyName(k.Elem())
	}
	if k.Type().Implements(textMarshalerType) {
		if k.Kind() == reflect.Pointer && k.IsNil() {
			return "", true // v1/v2 都写成空成员名
		}
		if !k.CanInterface() {
			return "", false
		}
		tm, ok := k.Interface().(encoding.TextMarshaler)
		if !ok {
			return "", false
		}
		b, err := tm.MarshalText()
		if err != nil {
			return "", false // json 同样会失败，不落盘
		}
		return string(b), true
	}
	switch k.Kind() {
	case reflect.String:
		return k.String(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), true
	case reflect.Float32:
		return maskFormatFloat(k.Float(), 32)
	case reflect.Float64:
		return maskFormatFloat(k.Float(), 64)
	}
	return "", false
}

// maskFormatFloat 复刻 encoding/json 写 JSON 数值的格式
// （internal/jsonwire.AppendFloat，即 ECMAScript 的 Number::toString）：
// |x| 落在 [1e-6, 1e21) 用 'f'，否则用 'e'，再把 e-09 收敛成 e-9。
//
// 直接用 strconv 的 'g' 会在 1e20 这一带写出 1e+20 而 json 写的是
// 100000000000000000000 —— 成员名对不上，日志检索规则就会失效。
// NaN / ±Inf 编不出 JSON 数值，json 报 unsupported value，返回 ok=false。
func maskFormatFloat(f float64, bits int) (string, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", false
	}
	if bits == 32 {
		f = float64(float32(f))
	}
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 {
		if bits == 64 && (abs < 1e-6 || abs >= 1e21) ||
			bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21) {
			format = 'e'
		}
	}
	b := strconv.AppendFloat(make([]byte, 0, 32), f, format, -1, bits)
	if format == 'e' {
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return string(b), true
}

// maskSlice 递归每个元素。[]byte 原样返回，否则会被渲染成一串数字。
func (m *masker) maskSlice(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if rv.Type().Elem().Kind() == reflect.Uint8 {
		return nil, false
	}
	if rv.Kind() == reflect.Slice && rv.IsNil() {
		return nil, false
	}

	n := rv.Len()
	var out []any
	for i := range n {
		nv, ch := m.maskValue(rv.Index(i), depth+1, seen)
		if ch {
			if out == nil {
				out = make([]any, n)
				for j := range i {
					out[j] = rv.Index(j).Interface()
				}
			}
			out[i] = nv
			continue
		}
		if out != nil {
			out[i] = rv.Index(i).Interface()
		}
	}

	if out == nil {
		return nil, false
	}
	return out, true
}
