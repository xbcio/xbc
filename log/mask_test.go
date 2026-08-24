package log

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func maskedLogger(extra []string) (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zapcore.DebugLevel)
	return zap.New(newMaskCore(core, newMasker(extra))), logs
}

func TestMaskAppliesToWriteFields(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("login", zap.String("password", "hunter2"), zap.String("user", "alice"))

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, maskPlaceholder, m["password"])
	assert.Equal(t, "alice", m["user"], "非敏感字段不受影响")
}

// 这条是 Core 层方案的价值证明：With 派生的字段同样被拦。
func TestMaskAppliesToWithFields(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.With(zap.String("access_token", "abc.def.ghi")).Info("call upstream")

	require.Len(t, logs.All(), 1)
	assert.Equal(t, maskPlaceholder, logs.All()[0].ContextMap()["access_token"])
}

// 回归测试：包装 zapcore.Core 时若忘了覆盖 Check，
// CheckedEntry 会挂上内层 Core，Write 直接绕过脱敏。
func TestMaskCoreCheckRoutesThroughWrapper(t *testing.T) {
	l, logs := maskedLogger(nil)
	ce := l.Check(zapcore.InfoLevel, "manual check")
	require.NotNil(t, ce)
	ce.Write(zap.String("secret", "s3cr3t"))

	require.Len(t, logs.All(), 1)
	assert.Equal(t, maskPlaceholder, logs.All()[0].ContextMap()["secret"])
}

func TestMaskNormalizesKeyStyle(t *testing.T) {
	m := newMasker(nil)
	for _, k := range []string{
		"accessToken", "ACCESS_TOKEN", "access-token", "Access.Token", "access token",
	} {
		assert.True(t, m.hit(k), "应命中：%q", k)
	}
}

func TestMaskMatchesExactlyNotByPrefix(t *testing.T) {
	m := newMasker(nil)
	assert.True(t, m.hit("phone"))
	assert.False(t, m.hit("phone_masked"), "已脱敏的字段不该被二次替换")
	assert.False(t, m.hit("mobile_type"), "前缀相同但语义无关的字段不该误伤")
	assert.False(t, m.hit("token_count"), "token_count 是数量不是凭据")
}

func TestBuiltinBlacklistCannotBeRemoved(t *testing.T) {
	// 试图把内置项"配置掉"是没有 API 的；这里验证追加不影响内置。
	m := newMasker([]string{"salary", "  ", ""})
	assert.True(t, m.hit("salary"), "配置项生效")
	assert.True(t, m.hit("password"), "内置黑名单始终生效")
	assert.True(t, m.hit("private_key"))
	assert.True(t, m.hit("id_card"))
	assert.True(t, m.hit("bank_card"))
}

func TestMaskCoversOrgMandatedBlacklist(t *testing.T) {
	m := newMasker(nil)
	// 组织安全规范列的绝对黑名单，一个都不能少
	for _, k := range []string{
		"password", "token", "ulp-token", "access_token", "refresh_token",
		"AK", "SK", "private_key", "db_url", "bank_card", "id_card", "phone",
	} {
		assert.True(t, m.hit(k), "组织规范要求脱敏：%q", k)
	}
}

func TestMaskDoesNotAllocateWhenNothingHits(t *testing.T) {
	m := newMasker(nil)
	in := []zapcore.Field{zap.String("user", "alice"), zap.Int("age", 30)}
	out := m.apply(in)
	// 必须用 Same（同一地址）而非 Equal —— Equal 对指针走 DeepEqual，
	// 语义是"地址相同或所指值深度相等"，无条件复制也照样能通过。
	assert.Same(t, &in[0], &out[0], "无命中时应原样返回同一个切片，不复制")
}

func TestMaskDoesNotMutateInput(t *testing.T) {
	m := newMasker(nil)
	in := []zapcore.Field{zap.String("password", "hunter2")}
	out := m.apply(in)
	assert.Equal(t, "hunter2", in[0].String, "输入切片不能被改写")
	assert.Equal(t, maskPlaceholder, out[0].String)
}

// ---------------------------------------------------------------------------
// Fix 1：嵌套值不再绕过脱敏
//
// 以下测试一律走完整的 zap.New(core).Info() 路径 —— 直调 m.hit() 只能验证判定
// 函数，验证不了防线是否真的挂在写入路径上。
// ---------------------------------------------------------------------------

// loginObj 实现 zapcore.ObjectMarshaler，走 zap.Object / zap.Inline 路径。
type loginObj struct {
	User     string
	Password string
}

func (r loginObj) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("user", r.User)
	enc.AddString("password", r.Password)
	return nil
}

// loginReq 不实现任何 marshaler，zap.Any 会把它交给反射路径（ReflectType）。
type loginReq struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// plainReq 无 tag，字段名直接取 Go 字段名。
type plainReq struct {
	User     string
	Password string
}

// zap.Inline 产出的 Field Key 为空串，AddTo 把对象字段摊进顶层命名空间 ——
// 最终输出与 zap.String("password", ...) 逐字节无法区分，却没有任何 key 可查。
func TestMaskFiltersInlineMarshaler(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("inline", zap.Inline(loginObj{User: "alice", Password: "hunter2"}))

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()
	assert.Equal(t, maskPlaceholder, m["password"], "Inline 摊进顶层的字段必须同样被拦")
	assert.Equal(t, "alice", m["user"])
}

func TestMaskFiltersObjectMarshaler(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("object", zap.Object("payload", loginObj{User: "alice", Password: "hunter2"}))

	require.Len(t, logs.All(), 1)
	sub, ok := logs.All()[0].ContextMap()["payload"].(map[string]any)
	require.True(t, ok, "payload 应是嵌套对象")
	assert.Equal(t, maskPlaceholder, sub["password"])
	assert.Equal(t, "alice", sub["user"], "同层非敏感字段完好")
}

func TestMaskFiltersReflectedStruct(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any",
		zap.Any("req", loginReq{User: "alice", Password: "hunter2"}),
		zap.Any("raw", plainReq{User: "bob", Password: "hunter3"}),
	)

	require.Len(t, logs.All(), 1)
	m := logs.All()[0].ContextMap()

	req, ok := m["req"].(map[string]any)
	require.True(t, ok, "命中后应替换成 map，实际 %T", m["req"])
	assert.Equal(t, maskPlaceholder, req["password"], "json tag 名参与判定")
	assert.Equal(t, "alice", req["user"])

	raw, ok := m["raw"].(map[string]any)
	require.True(t, ok, "无 tag 时用 Go 字段名，实际 %T", m["raw"])
	assert.Equal(t, maskPlaceholder, raw["Password"])
	assert.Equal(t, "bob", raw["User"])
}

func TestMaskFiltersReflectedMap(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("payload", map[string]any{
		"token": "abc.def.ghi",
		"user":  "alice",
	}))

	require.Len(t, logs.All(), 1)
	sub, ok := logs.All()[0].ContextMap()["payload"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["payload"])
	assert.Equal(t, maskPlaceholder, sub["token"])
	assert.Equal(t, "alice", sub["user"])
}

func TestMaskFiltersReflectedSlice(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("reqs", []loginReq{
		{User: "alice", Password: "hunter2"},
		{User: "bob", Password: "hunter3"},
	}))

	require.Len(t, logs.All(), 1)
	arr, ok := logs.All()[0].ContextMap()["reqs"].([]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["reqs"])
	require.Len(t, arr, 2)
	for i, e := range arr {
		item, ok := e.(map[string]any)
		require.True(t, ok, "第 %d 个元素实际 %T", i, e)
		assert.Equal(t, maskPlaceholder, item["password"])
	}
	assert.Equal(t, "alice", arr[0].(map[string]any)["user"])
	assert.Equal(t, "bob", arr[1].(map[string]any)["user"])
}

// 实现了 json.Marshaler / TextMarshaler / Stringer 的类型不能被拆开 ——
// 否则 time.Time 会被拆成 wall/ext/loc 三个未导出字段，输出全毁。
func TestMaskKeepsSelfMarshalingTypesIntact(t *testing.T) {
	type auditRec struct {
		User      string    `json:"user"`
		Password  string    `json:"password"`
		CreatedAt time.Time `json:"created_at"`
	}
	now := time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC)

	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("rec", auditRec{User: "alice", Password: "hunter2", CreatedAt: now}))

	require.Len(t, logs.All(), 1)
	rec, ok := logs.All()[0].ContextMap()["rec"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["rec"])
	assert.Equal(t, maskPlaceholder, rec["password"])
	assert.Equal(t, now, rec["created_at"], "时间字段不能被拆开也不能被替换")
}

func TestMaskTruncatesOverDeepNesting(t *testing.T) {
	const levels = 24
	var deep any = "bottom"
	for i := 0; i < levels; i++ {
		deep = map[string]any{"n": deep}
	}

	l, logs := maskedLogger(nil)
	require.NotPanics(t, func() { l.Info("deep", zap.Any("root", deep)) })

	require.Len(t, logs.All(), 1)
	cur := logs.All()[0].ContextMap()["root"]
	walked := 0
	for walked <= levels {
		sub, ok := cur.(map[string]any)
		if !ok {
			break
		}
		cur = sub["n"]
		walked++
	}
	assert.Less(t, walked, levels, "超过深度上限就不该继续递归")
	assert.Equal(t, maskPlaceholder, cur, "超深子树应被整体替换")
}

type selfRef struct {
	Name     string   `json:"name"`
	Password string   `json:"password"`
	Self     *selfRef `json:"self"`
}

func TestMaskHandlesSelfReference(t *testing.T) {
	a := selfRef{Name: "a", Password: "hunter2"}
	a.Self = &a

	l, logs := maskedLogger(nil)
	require.NotPanics(t, func() { l.Info("cycle", zap.Any("req", a)) })

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"])
	inner, ok := req["self"].(map[string]any)
	require.True(t, ok, "自引用的一层应被展开，实际 %T", req["self"])
	assert.Equal(t, maskPlaceholder, inner["password"])
	assert.Equal(t, maskPlaceholder, inner["self"], "第二次遇到同一地址即截断")
}

// OpenNamespace 只是原样转发：命名空间内的 key 仍会经过过滤 encoder 的 Add*。
func TestMaskFiltersInsideNamespace(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("ns", zap.Namespace("nested"), zap.String("password", "hunter2"))

	require.Len(t, logs.All(), 1)
	sub, ok := logs.All()[0].ContextMap()["nested"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["nested"])
	assert.Equal(t, maskPlaceholder, sub["password"])
}

// zap.Objects 产出的是 ArrayMarshalerType，元素靠 AppendObject 写出 ——
// 只包 ObjectEncoder 而漏了 ArrayEncoder 的话，这条路径整条裸奔。
func TestMaskFiltersArrayMarshaler(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("array", zap.Objects("reqs", []loginObj{
		{User: "alice", Password: "hunter2"},
		{User: "bob", Password: "hunter3"},
	}))

	require.Len(t, logs.All(), 1)
	arr, ok := logs.All()[0].ContextMap()["reqs"].([]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["reqs"])
	require.Len(t, arr, 2)
	for i, e := range arr {
		item, ok := e.(map[string]any)
		require.True(t, ok, "第 %d 个元素实际 %T", i, e)
		assert.Equal(t, maskPlaceholder, item["password"])
	}
	assert.Equal(t, "alice", arr[0].(map[string]any)["user"])
}

// nestObj 是一个会自我嵌套的 marshaler，用来撞过滤 encoder 上的深度上限。
type nestObj struct{ left int }

func (n nestObj) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("password", "hunter2")
	if n.left == 0 {
		return nil
	}
	return enc.AddObject("n", nestObj{left: n.left - 1})
}

func TestMaskTruncatesOverDeepMarshaler(t *testing.T) {
	const levels = 30
	l, logs := maskedLogger(nil)
	require.NotPanics(t, func() { l.Info("deep", zap.Object("root", nestObj{left: levels})) })

	require.Len(t, logs.All(), 1)
	cur := logs.All()[0].ContextMap()["root"]
	walked := 0
	for walked <= levels {
		sub, ok := cur.(map[string]any)
		if !ok {
			break
		}
		assert.Equal(t, maskPlaceholder, sub["password"], "第 %d 层的 password", walked)
		cur = sub["n"]
		walked++
	}
	assert.Less(t, walked, levels, "过滤 encoder 也要有深度上限")
	assert.Equal(t, maskPlaceholder, cur, "超深子树应被整体替换")
}

// 只在指针上实现 MarshalJSON 的类型，按值打日志时 encoding/json 不会调用它 ——
// 若判成"自带序列化"而跳过遍历，json.Marshal 照样逐字段摊开，形成绕过路径。
type ptrOnlyCreds struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (c *ptrOnlyCreds) MarshalJSON() ([]byte, error) { return []byte(`"redacted"`), nil }

func TestMaskDoesNotTrustPointerOnlyMarshaler(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("creds", ptrOnlyCreds{User: "alice", Password: "hunter2"}))

	require.Len(t, logs.All(), 1)
	creds, ok := logs.All()[0].ContextMap()["creds"].(map[string]any)
	require.True(t, ok, "按值传入时不能当成自带序列化，实际 %T", logs.All()[0].ContextMap()["creds"])
	assert.Equal(t, maskPlaceholder, creds["password"])
	assert.Equal(t, "alice", creds["user"])
}

// ---------------------------------------------------------------------------
// Fix 2：内层 core 不能被反射掏出来
// ---------------------------------------------------------------------------

func TestMaskCoreHidesInnerCoreFromReflection(t *testing.T) {
	obs, _ := observer.New(zapcore.DebugLevel)
	c := newMaskCore(obs, newMasker(nil))

	rv := reflect.ValueOf(c).Elem()
	if f := rv.FieldByName("Core"); f.IsValid() {
		assert.False(t, f.CanInterface(),
			"嵌入产生的隐式字段名 Core 是导出标识符，会把未脱敏的内层 core 交出去")
	}
	f := rv.FieldByName("inner")
	require.True(t, f.IsValid(), "内层 core 应放在未导出的命名字段 inner 里")
	assert.False(t, f.CanInterface(), "未导出字段不能被反射取值")
}

// ---------------------------------------------------------------------------
// Fix 5：后缀词组匹配
// ---------------------------------------------------------------------------

func TestMaskMatchesCompoundNamesBySuffix(t *testing.T) {
	m := newMasker(nil)
	for _, k := range []string{
		"db_password", "user_password", "login_password", "redis_password",
		"user_phone", "userPhone",
		"auth_token", "bearer_token", "x-api-key", "req.token",
	} {
		assert.True(t, m.hit(k), "复合命名的中心词在尾部，应命中：%q", k)
	}
}

func TestMaskSuffixRuleDoesNotOverreach(t *testing.T) {
	m := newMasker(nil)
	for _, k := range []string{"token_count", "phone_masked", "mobile_type"} {
		assert.False(t, m.hit(k), "中心词不是凭据，不该误伤：%q", k)
	}
}

func TestMaskSplitsConsecutiveUpperCase(t *testing.T) {
	m := newMasker(nil)
	assert.True(t, m.hit("AK"), "两字母全大写不能被切成 a / k")
	assert.True(t, m.hit("xAPIKey"), "大写序列后跟小写时在最后一个大写字母前切")
	assert.True(t, m.hit("accessToken"))
}

func TestMaskHitDoesNotAllocate(t *testing.T) {
	m := newMasker(nil)
	n := testing.AllocsPerRun(200, func() {
		m.hit("db_password")
		m.hit("user_name")
		m.hit("accessToken")
		m.hit("trace_id")
	})
	assert.Zero(t, n, "hit 在每条日志的每个字段上调用，是真正的热路径")
}

// ---------------------------------------------------------------------------
// Fix 1 的性能边界：无命中的反射值不能被拷贝
// ---------------------------------------------------------------------------

func TestMaskDoesNotCopyUnrelatedReflectedValue(t *testing.T) {
	type profile struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	m := newMasker(nil)
	in := []zapcore.Field{zap.Any("profile", profile{Name: "alice", Age: 30})}
	out := m.apply(in)
	require.Len(t, out, 1)
	assert.Same(t, &in[0], &out[0], "反射遍历一路无命中时必须原样交回，不拷贝")
}
