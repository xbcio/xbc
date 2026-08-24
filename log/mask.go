package log

import (
	"go.uber.org/zap/zapcore"
)

// maskPlaceholder 是敏感字段被替换后的值。
const maskPlaceholder = "***"

// builtinMaskFields 是内置脱敏黑名单，始终生效、不可通过配置移除。
// 依据组织安全规范的"日志绝对黑名单"，外加常见变体。
//
// 调用点有几千个，指望每个都记得脱敏是不现实的；拦截点只有这一个。
var builtinMaskFields = []string{
	// 口令
	"password", "passwd", "pwd", "old_password", "new_password",
	// 确认口令：值就是明文口令本身，不是关于口令的元数据（对比不命中的
	// password_hash —— 那是哈希，留着有排查价值）。后缀词组规则在这几个词上
	// 给出的中心词是 confirm / repeat，靠规则命不中，只能作为整串补进来。
	"password_confirm", "password2", "password_repeat",
	// 令牌
	"token", "ulp-token", "access_token", "refresh_token", "id_token",
	"authorization", "cookie", "set-cookie", "session_id", "jwt",
	// 密钥
	"secret", "client_secret", "private_key", "api_key",
	"ak", "sk", "access_key", "access_key_id", "secret_key", "secret_access_key",
	// 连接串
	"db_url", "dsn", "database_url", "conn_str",
	// 个人信息
	"id_card", "bank_card", "credit_card", "card_no", "cvv", "phone", "mobile",
}

// masker 判定字段名是否需要脱敏。
type masker struct{ keys map[string]struct{} }

// newMasker 用内置黑名单加 extra 构造。extra 只能追加，无法移除内置项。
func newMasker(extra []string) *masker {
	m := &masker{keys: make(map[string]struct{}, len(builtinMaskFields)+len(extra))}
	for _, k := range builtinMaskFields {
		m.keys[normalizeMaskKey(k)] = struct{}{}
	}
	for _, k := range extra {
		if nk := normalizeMaskKey(k); nk != "" {
			m.keys[nk] = struct{}{}
		}
	}
	return m
}

// ---------------------------------------------------------------------------
// 字段名匹配：归一化 + 后缀词组
// ---------------------------------------------------------------------------

// 归一化把字段名按分隔符（_ - . 空格）与驼峰边界切成词，全部转小写、丢掉分隔符。
// 于是 accessToken / access_token / access-token / ACCESS_TOKEN 归一到同一串。
//
// 匹配规则是"所有后缀词组"是否在黑名单里，而非整串精确匹配：
//
//	db_password → [db, password]  → 查 "password"（命中）、"dbpassword"
//	x-api-key   → [x, api, key]   → 查 "key"、"apikey"（命中）、"xapikey"
//	token_count → [token, count]  → 查 "count"、"tokencount" → 不命中
//
// 语言学依据：英文复合名词的中心词在尾部。db_password 的中心是 password，
// 字段就是口令本身，db_ 只限定作用域；token_count 的中心是 count，字段是关于
// token 的元数据而非 token 本身。这条规则不是启发式补丁，是有依据的。
//
// 最长的后缀词组就是整串，所以旧的整串精确匹配是新规则的一个特例 ——
// private_key → "privatekey" 依然命中，phone_masked 依然不命中。
const (
	// maskKeyBufSize / maskKeyMaxWords 是 hit 栈上缓冲的容量。
	// 超出即退化到堆分配路径，正确性不变，只是慢。
	maskKeyBufSize  = 64
	maskKeyMaxWords = 12
)

func isMaskUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isMaskLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isMaskDigit(c byte) bool { return c >= '0' && c <= '9' }

// splitMaskKey 把 key 归一化写进 buf，并在 starts 里记录每个词在 buf 中的起始下标。
// 返回归一化后的长度 n、词数 wc，以及 buf/starts 容量是否够用。
//
// 因为分隔符全被丢弃，词 i..末尾 拼接起来就是 buf[starts[i]:n] —— 后缀词组
// 无需再做任何拼接，这正是零分配的前提。
//
// 驼峰切词要正确处理连续大写：AK → [ak]（不能切成 a / k）；
// accessToken → [access, token]；xAPIKey → [x, api, key]
// （大写序列后跟小写时，在最后一个大写字母前切）。
func splitMaskKey(key string, buf []byte, starts []int) (n, wc int, ok bool) {
	newWord := true
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch c {
		case '_', '-', '.', ' ':
			newWord = true
			continue
		}
		if isMaskUpper(c) {
			if !newWord {
				// newWord 为 false 说明 key[i-1] 一定不是分隔符
				prev := key[i-1]
				switch {
				case isMaskLower(prev) || isMaskDigit(prev):
					newWord = true
				case isMaskUpper(prev) && i+1 < len(key) && isMaskLower(key[i+1]):
					newWord = true
				}
			}
			c += 'a' - 'A'
		}
		if newWord {
			if wc >= len(starts) {
				return 0, 0, false
			}
			starts[wc] = n
			wc++
			newWord = false
		}
		if n >= len(buf) {
			return 0, 0, false
		}
		buf[n] = c
		n++
	}
	return n, wc, true
}

// normalizeMaskKey 返回整串归一化结果，只在 newMasker 构造黑名单时调用。
// 与 hit 共用 splitMaskKey，两侧的归一化规则不可能走偏。
func normalizeMaskKey(k string) string {
	if k == "" {
		return ""
	}
	buf := make([]byte, len(k))
	starts := make([]int, len(k))
	n, _, ok := splitMaskKey(k, buf, starts)
	if !ok {
		return ""
	}
	return string(buf[:n])
}

// hit 判定字段名是否需要脱敏。
//
// 它在每条日志的每个字段上调用（嵌套对象里还会再调一轮），是真正的热路径，
// 因此常见字段名走栈上缓冲、零分配：m.keys[string(buf[a:b])] 这个形式
// 编译器有专门优化，不会为 []byte→string 的转换分配。
func (m *masker) hit(key string) bool {
	if key == "" {
		return false
	}
	var buf [maskKeyBufSize]byte
	var starts [maskKeyMaxWords]int
	n, wc, ok := splitMaskKey(key, buf[:], starts[:])
	if !ok {
		return m.hitSlow(key)
	}
	// 从最短的后缀词组查到最长（最长即整串）
	for i := wc - 1; i >= 0; i-- {
		if _, found := m.keys[string(buf[starts[i]:n])]; found {
			return true
		}
	}
	return false
}

// hitSlow 是字段名超长或词数过多时的退化路径：改用堆上缓冲，规则完全一致。
func (m *masker) hitSlow(key string) bool {
	buf := make([]byte, len(key))
	starts := make([]int, len(key))
	n, wc, ok := splitMaskKey(key, buf, starts)
	if !ok {
		// 按最坏情况分配过了，不可能再溢出
		return false
	}
	for i := wc - 1; i >= 0; i-- {
		if _, found := m.keys[string(buf[starts[i]:n])]; found {
			return true
		}
	}
	return false
}

// hitWindow 判定字段名的任意**连续词窗口**是否命中黑名单，而不只是后缀词组。
//
// 只有 hitFieldName 的替换名候选走它。替换名只在 json tag 被保留字符写坏时才
// 构造，而那条路径上垃圾可以同时夹在敏感中心词的两侧（`json:"db\password\x"`
// 归一化成 db / password / x 三个词），后缀词组永远够不着夹在中间的 password。
// 正常字段名一律走 hit 的后缀规则，窗口匹配的宽松度不会外溢到它们身上。
func (m *masker) hitWindow(key string) bool {
	if key == "" {
		return false
	}
	var buf [maskKeyBufSize]byte
	var starts [maskKeyMaxWords]int
	n, wc, ok := splitMaskKey(key, buf[:], starts[:])
	if !ok {
		return m.hitWindowSlow(key)
	}
	return m.matchWindow(buf[:], starts[:wc], n)
}

// hitWindowSlow 是名字超长或词数过多时的退化路径，规则与 hitWindow 完全一致。
func (m *masker) hitWindowSlow(key string) bool {
	buf := make([]byte, len(key))
	starts := make([]int, len(key))
	n, wc, ok := splitMaskKey(key, buf, starts)
	if !ok {
		// 按最坏情况分配过了，不可能再溢出
		return false
	}
	return m.matchWindow(buf, starts[:wc], n)
}

// matchWindow 枚举全部连续词窗口 [i, j)。词数上限 12 时最坏 78 次查表，
// 且只发生在写坏的 tag 上，不在热路径。
func (m *masker) matchWindow(buf []byte, starts []int, n int) bool {
	for i := range starts {
		for j := i + 1; j <= len(starts); j++ {
			end := n
			if j < len(starts) {
				end = starts[j]
			}
			if _, found := m.keys[string(buf[starts[i]:end])]; found {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 字段过滤
// ---------------------------------------------------------------------------

// apply 返回脱敏后的字段切片。一路无命中时原样返回入参，不做任何分配。
// 有命中时复制一份再改，绝不写坏调用方的切片。
func (m *masker) apply(fs []zapcore.Field) []zapcore.Field {
	var out []zapcore.Field
	for i := range fs {
		nf, changed := m.maskField(fs[i], 0)
		if !changed {
			continue
		}
		if out == nil {
			out = make([]zapcore.Field, len(fs))
			copy(out, fs)
		}
		out[i] = nf
	}
	if out == nil {
		return fs
	}
	return out
}

// maskField 脱敏单个字段。changed 为 false 时调用方必须用原字段。
//
// 顺序是先查 f.Key —— key 命中就整体替换成 ***，不必再进去看。
// key 不命中时才按 Field 类型决定是否要深入值内部（见 mask_nested.go）。
func (m *masker) maskField(f zapcore.Field, depth int) (zapcore.Field, bool) {
	switch f.Type {
	case zapcore.NamespaceType, zapcore.SkipType:
		// 命名空间只有名字没有值，替换它会把后续字段的层级打乱；
		// 空间内的字段本来就要各自经过一次 maskField / 过滤 encoder。
		return f, false
	}

	if m.hit(f.Key) {
		return zapcore.Field{
			Key:    f.Key,
			Type:   zapcore.StringType,
			String: maskPlaceholder,
		}, true
	}

	switch f.Type {
	case zapcore.InlineMarshalerType:
		// Inline 的 Field Key 是空串，字段会被摊进当前命名空间 ——
		// 没有 key 可查，只能包一层过滤 encoder 进去看。
		om, ok := f.Interface.(zapcore.ObjectMarshaler)
		if !ok {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.InlineMarshalerType,
			Interface: &maskObjectMarshaler{om: om, m: m, depth: depth + 1},
		}, true

	case zapcore.ObjectMarshalerType:
		om, ok := f.Interface.(zapcore.ObjectMarshaler)
		if !ok {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.ObjectMarshalerType,
			Interface: &maskObjectMarshaler{om: om, m: m, depth: depth + 1},
		}, true

	case zapcore.ArrayMarshalerType:
		am, ok := f.Interface.(zapcore.ArrayMarshaler)
		if !ok {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.ArrayMarshalerType,
			Interface: &maskArrayMarshaler{am: am, m: m, depth: depth + 1},
		}, true

	case zapcore.ReflectType:
		nv, changed := m.maskReflected(f.Interface, depth+1)
		if !changed {
			return f, false
		}
		return zapcore.Field{
			Key:       f.Key,
			Type:      zapcore.ReflectType,
			Interface: nv,
		}, true
	}

	return f, false
}

// ---------------------------------------------------------------------------
// Core 层拦截
// ---------------------------------------------------------------------------

// maskCore 在 Core 层拦截敏感字段。
//
// 正确装配：每个叶子 sink 各包一层 maskCore，maskCore 在 Tee 之内。
//
//	zapcore.NewTee(
//	    newMaskCore(consoleCore, m),
//	    newMaskCore(fileCore, m),
//	    newMaskCore(errFileCore, m),
//	)
//
// 不能反过来包在 Tee 之外 —— maskCore.Check 会把自己挂进 CheckedEntry，
// Tee 的 per-sink 级别过滤（zapcore/tee.go:74-79）便再也不会执行，
// error_path 会收到全量日志。采样器则相反，包在 Tee 之外。
//
// 因为 maskCore 是 *zap.Logger 的组成部分，log.Zap() 逃生舱口同样被覆盖。
//
// 内层 core 必须放在未导出的命名字段 inner 里，不能嵌入 zapcore.Core：
// 嵌入产生的隐式字段名 Core 是导出标识符，reflect 的 CanInterface() 为 true
// （与外层类型是否导出无关），任何拿到本 core 的代码三行就能掏出未脱敏的
// 内层 core 直接写日志。第二重收益是 zapcore.Core 将来新增方法时嵌入会
// 静默继承内层实现（一条新的绕过路径），显式实现则编译报错。
type maskCore struct {
	inner zapcore.Core
	m     *masker
}

func newMaskCore(c zapcore.Core, m *masker) zapcore.Core {
	return &maskCore{inner: c, m: m}
}

func (c *maskCore) Enabled(l zapcore.Level) bool { return c.inner.Enabled(l) }

func (c *maskCore) Sync() error { return c.inner.Sync() }

func (c *maskCore) With(fs []zapcore.Field) zapcore.Core {
	return &maskCore{inner: c.inner.With(c.m.apply(fs)), m: c.m}
}

// Check 必须覆盖。基类的 Check 会把内层 Core 挂进 CheckedEntry，
// 之后的 Write 直接打到内层，整个脱敏被绕过。
func (c *maskCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c *maskCore) Write(ent zapcore.Entry, fs []zapcore.Field) error {
	return c.inner.Write(ent, c.m.apply(fs))
}
