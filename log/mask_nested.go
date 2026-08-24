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
// 以下三条不在本防线的覆盖范围内，读代码的人不要以为它是全覆盖的：
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
//
// 匿名嵌入字段按 encoding/json 的规则平铺进父层（无 json 名字 + 解一层指针后是
// struct 才平铺），冲突时外层优先。这一点必须与 json 对齐：形状只在命中时才变，
// 恰好是最需要日志形状稳定的时候 —— {"Base":{"password":"***"}} 会让针对
// .password 的检索规则失效。多路同深度冲突不做 encoding/json 那套完整消歧，
// 先出现的嵌入胜出。

package log

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"time"

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
		if maskEmbedFlatten(sf) || !sf.IsExported() {
			continue
		}
		name := maskFieldName(sf)
		if name == "" { // json:"-"
			continue
		}
		fv := rv.Field(i)

		if m.hit(name) {
			if out == nil {
				out = maskNamedRaw(rv)
			}
			out[name] = maskPlaceholder
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
		if maskEmbedFlatten(sf) || !sf.IsExported() {
			continue
		}
		name := maskFieldName(sf)
		if name == "" {
			continue
		}
		out[name] = rv.Field(i).Interface()
	}
	return out
}

// maskStructRaw 把 rv 按平铺后的形状原样写进 out：先命名字段再嵌入字段，
// 已存在的 key 不覆盖。budget 只是兜底 —— 真正的自引用嵌入会在 maskEmbedStruct
// 里被 seen 判成 changed 而走不到这条原样路径，这里不依赖它保正确性。
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
		if maskEmbedFlatten(sf) || !sf.IsExported() {
			continue
		}
		name := maskFieldName(sf)
		if name == "" {
			continue
		}
		if _, dup := out[name]; !dup {
			out[name] = rv.Field(i).Interface()
		}
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

// maskMap 只对 string key 的 map 做判定，其余原样返回。
func (m *masker) maskMap(rv reflect.Value, depth int, seen *map[uintptr]struct{}) (any, bool) {
	if rv.IsNil() || rv.Type().Key().Kind() != reflect.String {
		return nil, false
	}

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
