package log

import (
	"strings"

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

// normalizeMaskKey 归一化字段名：转小写、去掉分隔符。
// 这样 accessToken / access_token / access-token / ACCESS_TOKEN 命中同一条规则。
//
// 归一化后做精确匹配而非前缀匹配 —— phone 命中，phone_masked 不命中，
// 免得已经脱敏过的字段被二次替换成 ***。
func normalizeMaskKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	for _, r := range k {
		switch r {
		case '_', '-', '.', ' ':
			// 分隔符全部丢弃
		default:
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m *masker) hit(key string) bool {
	_, ok := m.keys[normalizeMaskKey(key)]
	return ok
}

// apply 返回脱敏后的字段切片。没有命中时原样返回入参，不做任何分配。
// 有命中时复制一份再改，绝不写坏调用方的切片。
func (m *masker) apply(fs []zapcore.Field) []zapcore.Field {
	var out []zapcore.Field
	for i := range fs {
		if !m.hit(fs[i].Key) {
			continue
		}
		if out == nil {
			out = make([]zapcore.Field, len(fs))
			copy(out, fs)
		}
		out[i] = zapcore.Field{
			Key:    fs[i].Key,
			Type:   zapcore.StringType,
			String: maskPlaceholder,
		}
	}
	if out == nil {
		return fs
	}
	return out
}

// maskCore 在 Core 层拦截敏感字段。
//
// 装配时包在 Tee 之外，一次拦截覆盖全部 sink；
// 因为它是 *zap.Logger 的组成部分，log.Zap() 逃生舱口同样被覆盖。
type maskCore struct {
	zapcore.Core
	m *masker
}

func newMaskCore(c zapcore.Core, m *masker) zapcore.Core {
	return &maskCore{Core: c, m: m}
}

func (c *maskCore) With(fs []zapcore.Field) zapcore.Core {
	return &maskCore{Core: c.Core.With(c.m.apply(fs)), m: c.m}
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
	return c.Core.Write(ent, c.m.apply(fs))
}
