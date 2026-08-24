package log

import (
	"bytes"
	"encoding/json"
	"math"
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
	for range levels {
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

// stringerOnlyCreds 只实现 String()。encoding/json 的编码器只查 json.Marshaler
// 与 encoding.TextMarshaler，压根不看 String() —— 判它"自带序列化"而跳过遍历，
// json 照样把它逐字段摊开，是纯粹的漏放。
type stringerOnlyCreds struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (c stringerOnlyCreds) String() string { return "redacted" }

func TestMaskTraversesStringerFieldsInsideStruct(t *testing.T) {
	// 只覆盖"被包在外层 struct 里"的 Stringer 字段。直传给 zap.Any 的
	// Stringer / error 走 zap 的 StringerType / ErrorType 分支，落盘的是
	// String() / Error() 的结果，压根进不了反射遍历 —— 那归包注释的第 4 条边界，
	// 是有意豁免的，不要在这里加断言去"修"它。
	type wrapper struct {
		Creds stringerOnlyCreds `json:"creds"`
	}
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("w", wrapper{
		Creds: stringerOnlyCreds{User: "alice", Password: "hunter2"},
	}))

	require.Len(t, logs.All(), 1)
	w, ok := logs.All()[0].ContextMap()["w"].(map[string]any)
	require.True(t, ok, "只实现 String() 的类型不能当成自带序列化，实际 %T",
		logs.All()[0].ContextMap()["w"])
	creds, ok := w["creds"].(map[string]any)
	require.True(t, ok, "实际 %T", w["creds"])
	assert.Equal(t, maskPlaceholder, creds["password"])
	assert.Equal(t, "alice", creds["user"])
}

// maxMaskDepth 数的是结构嵌套层数，不是反射的间接层数。
// 把指针/接口解引用也算进去的话，map[string]any 每层要吃 2 个 depth，
// 上限 8 实际只剩 4 层 —— 这个常量的含义就变得不可预测了。
func TestMaskDepthCountsStructureNotIndirection(t *testing.T) {
	const levels = 6
	var deep any = map[string]any{"leaf": "visible", "token": "s3cr3t"}
	for i := 1; i < levels; i++ {
		deep = map[string]any{"n": deep}
	}

	l, logs := maskedLogger(nil)
	l.Info("deep", zap.Any("root", deep))

	require.Len(t, logs.All(), 1)
	cur := logs.All()[0].ContextMap()["root"]
	for i := 1; i < levels; i++ {
		sub, ok := cur.(map[string]any)
		require.True(t, ok, "第 %d 层应仍是 map，实际 %T", i, cur)
		cur = sub["n"]
	}
	bottom, ok := cur.(map[string]any)
	require.True(t, ok, "第 %d 层应仍是 map，实际 %T", levels, cur)
	assert.Equal(t, "visible", bottom["leaf"], "上限之内的正常字段不能被整体替换")
	assert.Equal(t, maskPlaceholder, bottom["token"], "遍历确实走到了最深一层")
}

// ---------------------------------------------------------------------------
// 匿名嵌入字段按 encoding/json 的规则平铺
//
// 形状只在命中时才变 —— 恰好是最需要日志形状正确的时候。
// {"Base":{"password":"***"}} 会让针对 .password 的检索规则失效。
// ---------------------------------------------------------------------------

type MaskBase struct {
	Password string `json:"password"`
	Region   string `json:"region"`
}

type embedReq struct {
	MaskBase
	User string `json:"user"`
}

type embedPtrReq struct {
	*MaskBase
	User string `json:"user"`
}

type taggedEmbedReq struct {
	MaskBase `json:"base"`
	User     string `json:"user"`
}

// maskUnexportedBase 是未导出的嵌入类型。encoding/json 照样提升它的导出字段，
// 不平铺就等于放行 —— reflect 允许读取它的导出子字段。
type maskUnexportedBase struct {
	Password string `json:"password"`
}

type embedUnexportedReq struct {
	maskUnexportedBase
	User string `json:"user"`
}

// MaskInt 是非 struct 的嵌入类型，encoding/json 按类型名当作普通字段。
type MaskInt int

type embedNonStructReq struct {
	MaskInt
	Password string `json:"password"`
}

type embedConflictReq struct {
	MaskBase
	Region string `json:"region"` // 与嵌入体同名，外层优先
}

func TestMaskFlattensEmbeddedStruct(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedReq{
		MaskBase: MaskBase{Password: "hunter2", Region: "cn-north"},
		User:     "alice",
	}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"], "应平铺到顶层而不是 MaskBase.password")
	assert.Equal(t, "cn-north", req["region"], "嵌入体的非敏感字段也要平铺")
	assert.Equal(t, "alice", req["user"])
	assert.NotContains(t, req, "MaskBase", "不能留下按类型名嵌套的那一层")
}

func TestMaskFlattensEmbeddedStructPointer(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedPtrReq{
		MaskBase: &MaskBase{Password: "hunter2", Region: "cn-north"},
		User:     "alice",
	}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"])
	assert.Equal(t, "cn-north", req["region"])
	assert.Equal(t, "alice", req["user"])
}

func TestMaskFlattensUnexportedEmbeddedStruct(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedUnexportedReq{
		maskUnexportedBase: maskUnexportedBase{Password: "hunter2"},
		User:               "alice",
	}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"], "encoding/json 会提升它，不平铺就是放行")
	assert.Equal(t, "alice", req["user"])
}

// 带 json tag 的匿名嵌入按普通命名字段处理，不平铺。
func TestMaskKeepsTaggedEmbeddedStructNested(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", taggedEmbedReq{
		MaskBase: MaskBase{Password: "hunter2", Region: "cn-north"},
		User:     "alice",
	}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	base, ok := req["base"].(map[string]any)
	require.True(t, ok, "有 tag 就该按普通字段嵌套，实际 %T", req["base"])
	assert.Equal(t, maskPlaceholder, base["password"])
	assert.NotContains(t, req, "password", "不该平铺")
}

// 嵌入非 struct 类型时按类型名当普通字段。
func TestMaskKeepsNonStructEmbeddedAsNamedField(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedNonStructReq{MaskInt: 7, Password: "hunter2"}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"])
	assert.Equal(t, MaskInt(7), req["MaskInt"], "非 struct 的嵌入按类型名成为普通字段")
}

// 字段名冲突时外层优先。
func TestMaskEmbeddedFlatteningPrefersOuterField(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedConflictReq{
		MaskBase: MaskBase{Password: "hunter2", Region: "inner"},
		Region:   "outer",
	}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"])
	assert.Equal(t, "outer", req["region"], "同名时外层字段胜出")
}

type MaskPlain struct {
	Zone string `json:"zone"`
}

type embedRawReq struct {
	MaskPlain
	Token string `json:"token"`
}

type embedTwoReq struct {
	MaskPlain
	MaskBase
	User string `json:"user"`
}

// 命中发生在本层命名字段上时，未命中的嵌入体也要按平铺形状原样搬过来，
// 不能整块丢掉、也不能退回按类型名嵌套。
func TestMaskFlattensUnchangedEmbeddedOnOuterHit(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedRawReq{
		MaskPlain: MaskPlain{Zone: "az-1"},
		Token:     "s3cr3t",
	}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["token"])
	assert.Equal(t, "az-1", req["zone"], "未命中的嵌入体也要平铺")
	assert.NotContains(t, req, "MaskPlain")
}

// 命中发生在靠后的嵌入体里时，它之前的嵌入体同样要补进结果。
func TestMaskFlattensEarlierEmbeddedOnLaterHit(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedTwoReq{
		MaskPlain: MaskPlain{Zone: "az-1"},
		MaskBase:  MaskBase{Password: "hunter2", Region: "cn-north"},
		User:      "alice",
	}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"])
	assert.Equal(t, "cn-north", req["region"])
	assert.Equal(t, "alice", req["user"])
	assert.Equal(t, "az-1", req["zone"], "命中字段之前的嵌入体不能丢")
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

// ---------------------------------------------------------------------------
// Fix 6-10：落盘字节级断言
//
// 反射脱敏的最终效果由 encoding/json 决定，observer 的 ContextMap 只是中间表示 ——
// 它不会告诉你 map[int]X 到底能不能序列化。以下测试一律走真实 JSON encoder。
// ---------------------------------------------------------------------------

// maskedJSONLogger 组装一条写进内存 buffer 的真实日志管线。
func maskedJSONLogger() (*zap.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	enc := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		MessageKey:  "msg",
		LevelKey:    "lvl",
		EncodeLevel: zapcore.LowercaseLevelEncoder,
	})
	core := zapcore.NewCore(enc, zapcore.AddSync(buf), zapcore.DebugLevel)
	return zap.New(newMaskCore(core, newMasker(nil))), buf
}

// logJSON 打一条日志并把落盘字节同时以原文和解析结果交回。
func logJSON(t *testing.T, fs ...zapcore.Field) (string, map[string]any) {
	t.Helper()
	l, buf := maskedJSONLogger()
	l.Info("m", fs...)
	raw := buf.String()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &parsed), "落盘字节应是合法 JSON：%s", raw)
	return raw, parsed
}

// subMap 取出嵌套的一层对象，失败时把落盘原文打出来。
func subMap(t *testing.T, raw string, m map[string]any, key string) map[string]any {
	t.Helper()
	sub, ok := m[key].(map[string]any)
	require.True(t, ok, "%q 应是对象，实际 %T；落盘：%s", key, m[key], raw)
	return sub
}

type mapCreds struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// 14：非 string key 的 map 直传。map[int64]Order 是最常见的按 ID 索引写法，
// encoding/json 把 key 十进制字符串化后照常序列化 value —— 不递归就是明文落盘。
func TestMaskRecursesIntoNonStringKeyMap(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[int]mapCreds{1: {User: "alice", Password: "hunter2"}}))

	assert.NotContains(t, raw, "hunter2", "落盘字节里不能出现明文口令")
	entry := subMap(t, raw, subMap(t, raw, m, "v"), "1")
	assert.Equal(t, maskPlaceholder, entry["password"])
	assert.Equal(t, "alice", entry["user"], "非敏感字段不受影响")
}

type byIDReq struct {
	ByID map[int64]mapCreds `json:"by_id"`
	Name string             `json:"name"`
}

// 15：非 string key 的 map 作为 struct 字段，嵌套位置同样要脱敏。
func TestMaskRecursesIntoNestedNonStringKeyMap(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", byIDReq{
		ByID: map[int64]mapCreds{7: {User: "alice", Password: "hunter2"}},
		Name: "n",
	}))

	assert.NotContains(t, raw, "hunter2")
	entry := subMap(t, raw, subMap(t, raw, subMap(t, raw, m, "v"), "by_id"), "7")
	assert.Equal(t, maskPlaceholder, entry["password"])
	assert.Equal(t, "n", subMap(t, raw, m, "v")["name"])
}

type byIDWithHitReq struct {
	ByID  map[int64]mapCreds `json:"by_id"`
	Token string             `json:"token"`
}

// 16：同层一个 key 命中、一个是非 string key 的 map。
// 这是最难看的形态 —— 一条日志里一半脱敏一半明文，看上去像"防线生效了"。
func TestMaskDoesNotLeaveHalfMaskedRecord(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", byIDWithHitReq{
		ByID:  map[int64]mapCreds{7: {User: "alice", Password: "hunter2"}},
		Token: "abc.def",
	}))

	assert.NotContains(t, raw, "hunter2")
	assert.NotContains(t, raw, "abc.def")
	v := subMap(t, raw, m, "v")
	assert.Equal(t, maskPlaceholder, v["token"])
	assert.Equal(t, maskPlaceholder, subMap(t, raw, subMap(t, raw, v, "by_id"), "7")["password"])
}

// maskHeaderKey 是 key 类型自带 MarshalText 的形态：底层是 int，
// 但 encoding/json 用 MarshalText 的结果当成员名，于是成员名是有意义的英文词。
type maskHeaderKey int

func (maskHeaderKey) MarshalText() ([]byte, error) { return []byte("authorization"), nil }

// 17：字符串化后的 key 要统一查一次黑名单。
func TestMaskChecksStringifiedMapKey(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[maskHeaderKey]string{7: "Bearer topsecret"}))

	assert.NotContains(t, raw, "topsecret")
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["authorization"])
}

// 18：float key 与 bool key。
//
// 实测（Go 1.27，encoding/json 由 json/v2 实现）：float key 是**支持**的，
// 序列化成 {"1.5":…}；bool key 报 unsupported value，zap 记成 vError，
// 不落盘明文。两种都不能 panic。
func TestMaskHandlesFloatAndBoolMapKeys(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[float64]mapCreds{1.5: {User: "alice", Password: "hunter2"}}))
	assert.NotContains(t, raw, "hunter2", "float key 的 map 是能序列化的，必须递归")
	assert.Equal(t, maskPlaceholder,
		subMap(t, raw, subMap(t, raw, m, "v"), "1.5")["password"])

	raw, m = logJSON(t, zap.Any("v", map[bool]mapCreds{true: {User: "alice", Password: "hunter2"}}))
	assert.NotContains(t, raw, "hunter2", "bool key 编不出成员名，json 报错而非落盘")
	assert.Contains(t, m, "vError", "zap 把编码失败记成 <key>Error，落盘：%s", raw)
	assert.NotContains(t, m, "v")
}

// sMaskHidden 是未导出的嵌入类型。带 json 名字 tag 时它不平铺，而是作为普通
// 命名字段嵌套输出 —— encoding/json 的 typeFields 对匿名字段的跳过条件是
// "未导出**且**类型非 struct"，所以它照样被收录。
type sMaskHidden struct {
	Password string `json:"password"`
	Region   string `json:"region"`
}

type sMaskTagged struct {
	sMaskHidden `json:"base"`
	User        string `json:"user"`
}

type sMaskTaggedPtr struct {
	*sMaskHidden `json:"base"`
	User         string `json:"user"`
}

// 19：未导出匿名嵌入 + json 名字 tag，值嵌入。
func TestMaskTraversesUnexportedTaggedEmbed(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskTagged{
		sMaskHidden: sMaskHidden{Password: "hunter2", Region: "cn-north"},
		User:        "alice",
	}))

	assert.NotContains(t, raw, "hunter2")
	v := subMap(t, raw, m, "v")
	base := subMap(t, raw, v, "base")
	assert.Equal(t, maskPlaceholder, base["password"])
	assert.Equal(t, "cn-north", base["region"], "同层的非敏感字段不能丢")
	assert.Equal(t, "alice", v["user"])
}

// 20：同上，指针嵌入。
func TestMaskTraversesUnexportedTaggedEmbedPointer(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskTaggedPtr{
		sMaskHidden: &sMaskHidden{Password: "hunter2", Region: "cn-north"},
		User:        "alice",
	}))

	assert.NotContains(t, raw, "hunter2")
	base := subMap(t, raw, subMap(t, raw, m, "v"), "base")
	assert.Equal(t, maskPlaceholder, base["password"])
	assert.Equal(t, "cn-north", base["region"])
}

type sMaskPlainBase struct {
	Region string `json:"region"`
}

type sMaskTaggedClean struct {
	sMaskPlainBase `json:"base"`
	Token          string `json:"token"`
}

// 21：未导出匿名嵌入自身无命中，但同层兄弟字段命中。
//
// 这是 Interface() 那个坑的回归测试：兄弟命中会触发"把本层命名字段原样搬进 map"，
// 而未导出匿名嵌入字段的 reflect.Value 拿不到 Interface()（只有它自己拿不到，
// 它的导出子字段是可以取值的），照搬就 panic。
func TestMaskDoesNotPanicOnUnexportedEmbedWithSiblingHit(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskTaggedClean{
		sMaskPlainBase: sMaskPlainBase{Region: "cn-north"},
		Token:          "abc.def",
	}))

	assert.NotContains(t, raw, "abc.def")
	v := subMap(t, raw, m, "v")
	assert.Equal(t, maskPlaceholder, v["token"])
	assert.Equal(t, "cn-north", subMap(t, raw, v, "base")["region"], "无命中的嵌入体不能丢")
}

// sMaskBadTag 的 json tag 名字非法（含引号）。encoding/json 不会原样采用它，
// 拿这个名字查黑名单必然不命中 —— 而字段值是货真价实的口令。
type sMaskBadTag struct {
	Password string `json:"pass\"word"`
	User     string `json:"user"`
}

// 22：非法 tag 名 —— 双名查生效。
func TestMaskChecksGoFieldNameWhenTagNameIsInvalid(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskBadTag{Password: "hunter2", User: "alice"}))
	assert.NotContains(t, raw, "hunter2", "tag 名查不中时要退回 Go 字段名，落盘：%s", raw)
}

type sMaskRenamed struct {
	Token string `json:"count"`
}

// 23：tag 名无害、Go 字段名是凭据 —— 任一命中即脱敏。
func TestMaskChecksBothTagAndGoFieldName(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskRenamed{Token: "abc.def"}))
	assert.NotContains(t, raw, "abc.def")
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["count"])
}

type sMaskTokenCount struct {
	TokenCount int `json:"n"`
}

// 24：防误伤回归 —— 双名查不能把 TokenCount 这类计数字段也拖下水。
func TestMaskDoubleNameLookupDoesNotOverreach(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskTokenCount{TokenCount: 42}))
	assert.EqualValues(t, 42, subMap(t, raw, m, "v")["n"], "计数字段不是凭据，落盘：%s", raw)
}

type sMaskConfirm struct {
	PasswordConfirm string `json:"passwordConfirm"`
	Password2       string `json:"password2"`
	PasswordRepeat  string `json:"password_repeat"`
}

// 25：确认口令类字段的值就是明文口令本身，不是关于口令的元数据。
func TestMaskCoversPasswordConfirmVariants(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskConfirm{
		PasswordConfirm: "hunter2",
		Password2:       "hunter2",
		PasswordRepeat:  "hunter2",
	}))

	assert.NotContains(t, raw, "hunter2")
	v := subMap(t, raw, m, "v")
	for _, k := range []string{"passwordConfirm", "password2", "password_repeat"} {
		assert.Equal(t, maskPlaceholder, v[k], "%q 的值就是明文口令", k)
	}
}

// 26：既有裁决的回归 —— 这四个不该命中，补黑名单不能把它们捎带上。
func TestMaskStillDoesNotOverreachAfterBlacklistGrowth(t *testing.T) {
	m := newMasker(nil)
	for _, k := range []string{"password_hash", "token_count", "phone_masked", "mobile_type"} {
		assert.False(t, m.hit(k), "既有裁决：不该命中 %q", k)
	}
}

// map[any]V 是合法的：实测 {1:…} 落盘成 {"1":…}，key 的动态类型决定成员名。
// 这条路径（maskMapKeyName 的 interface 拆箱）是本轮新增的，得有测试盯着。
func TestMaskRecursesIntoInterfaceKeyMap(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[any]mapCreds{1: {User: "alice", Password: "hunter2"}}))

	assert.NotContains(t, raw, "hunter2")
	assert.Equal(t, maskPlaceholder, subMap(t, raw, subMap(t, raw, m, "v"), "1")["password"])
}

// NaN / ±Inf 编不出 JSON 数值，json 报 unsupported value 而不落盘 ——
// 不能 panic，也不能自作主张把一条编不出来的记录改成能编出来的形状。
func TestMaskLeavesUnencodableFloatKeyMapAlone(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[float64]mapCreds{
		math.NaN(): {User: "alice", Password: "hunter2"},
	}))

	assert.NotContains(t, raw, "hunter2")
	assert.Contains(t, m, "vError", "落盘：%s", raw)
}
