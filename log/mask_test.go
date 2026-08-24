package log

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
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

// This test proves the value of the Core-layer approach: fields derived via
// With are intercepted the same way.
func TestMaskAppliesToWithFields(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.With(zap.String("access_token", "abc.def.ghi")).Info("call upstream")

	require.Len(t, logs.All(), 1)
	assert.Equal(t, maskPlaceholder, logs.All()[0].ContextMap()["access_token"])
}

// Regression test: when wrapping zapcore.Core, forgetting to override Check
// lets CheckedEntry hang onto the inner Core, and Write bypasses masking
// entirely.
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
	// There's no API to "configure away" a built-in entry; this verifies that
	// appending doesn't affect the built-ins.
	m := newMasker([]string{"salary", "  ", ""})
	assert.True(t, m.hit("salary"), "配置项生效")
	assert.True(t, m.hit("password"), "内置黑名单始终生效")
	assert.True(t, m.hit("private_key"))
	assert.True(t, m.hit("id_card"))
	assert.True(t, m.hit("bank_card"))
}

func TestMaskCoversOrgMandatedBlacklist(t *testing.T) {
	m := newMasker(nil)
	// The absolute blacklist listed in the org security policy -- not one can
	// be missing
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
	// Must use Same (identical address) rather than Equal -- Equal takes the
	// DeepEqual path for pointers, whose semantics are "same address OR the
	// pointed-to values are deeply equal", so an unconditional copy would pass
	// just as well.
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
// Fix 1: nested values no longer bypass masking
//
// The tests below always go through the full zap.New(core).Info() path --
// calling m.hit() directly can only verify the decision function, not whether
// the defense is actually wired into the write path.
// ---------------------------------------------------------------------------

// loginObj implements zapcore.ObjectMarshaler, exercising the zap.Object /
// zap.Inline path.
type loginObj struct {
	User     string
	Password string
}

func (r loginObj) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("user", r.User)
	enc.AddString("password", r.Password)
	return nil
}

// loginReq implements no marshaler, so zap.Any hands it to the reflection
// path (ReflectType).
type loginReq struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// plainReq has no tags; field names come directly from the Go field names.
type plainReq struct {
	User     string
	Password string
}

// zap.Inline produces a Field with an empty Key; AddTo flattens the object's
// fields into the top-level namespace -- the final output is byte-for-byte
// indistinguishable from zap.String("password", ...), yet there is no key to
// look up at all.
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

// Types implementing json.Marshaler / TextMarshaler / Stringer must not be
// taken apart -- otherwise time.Time would be split into its three unexported
// fields wall/ext/loc, wrecking the output entirely.
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

// OpenNamespace only forwards as-is: keys inside the namespace still go
// through the filtering encoder's Add*.
func TestMaskFiltersInsideNamespace(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("ns", zap.Namespace("nested"), zap.String("password", "hunter2"))

	require.Len(t, logs.All(), 1)
	sub, ok := logs.All()[0].ContextMap()["nested"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["nested"])
	assert.Equal(t, maskPlaceholder, sub["password"])
}

// zap.Objects produces ArrayMarshalerType, whose elements are written out via
// AppendObject -- if only ObjectEncoder is wrapped and ArrayEncoder is
// missed, this whole path runs completely unprotected.
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

// nestObj is a marshaler that nests itself, used to hit the filtering
// encoder's depth cap.
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

// A type that implements MarshalJSON only on the pointer: encoding/json won't
// call it when the value is logged by value -- if we judged it as
// "self-serializing" and skipped traversal, json.Marshal would still flatten
// it field by field, opening a bypass path.
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

// stringerOnlyCreds implements only String(). encoding/json's encoder only
// checks for json.Marshaler and encoding.TextMarshaler, and never looks at
// String() at all -- judging it "self-serializing" and skipping traversal
// would leave it flattened field by field by json anyway; this is a pure
// omission.
type stringerOnlyCreds struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func (c stringerOnlyCreds) String() string { return "redacted" }

func TestMaskTraversesStringerFieldsInsideStruct(t *testing.T) {
	// Only covers Stringer fields that are wrapped inside an outer struct. A
	// Stringer / error value passed directly to zap.Any goes through zap's
	// StringerType / ErrorType branch, and what gets written to disk is the
	// result of String() / Error() -- it never enters reflection traversal
	// at all. That falls under boundary #4 of the package doc comment; it's
	// an intentional exemption, so don't add an assertion here to "fix" it.
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

// maxMaskDepth counts structural nesting levels, not reflection indirection
// levels. If pointer/interface dereferencing were counted too, map[string]any
// would consume 2 depth units per level, and the cap of 8 would really only
// leave 4 levels -- the meaning of this constant would become unpredictable.
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
// Anonymous embedded fields are flattened following encoding/json's rules
//
// The shape only changes when something hits -- which is exactly when the log
// shape needs to be correct the most. {"Base":{"password":"***"}} would break
// search rules targeting .password.
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

// maskUnexportedBase is an unexported embedded type. encoding/json still
// promotes its exported fields, so not flattening it would amount to letting
// it through -- reflect allows reading its exported sub-fields.
type maskUnexportedBase struct {
	Password string `json:"password"`
}

type embedUnexportedReq struct {
	maskUnexportedBase
	User string `json:"user"`
}

// MaskInt is a non-struct embedded type; encoding/json treats it as a regular
// field named after the type.
type MaskInt int

type embedNonStructReq struct {
	MaskInt
	Password string `json:"password"`
}

type embedConflictReq struct {
	MaskBase
	Region string `json:"region"` // same name as the embedded struct; outer wins
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

// An anonymous embed with a json tag is treated as a regular named field, not
// flattened.
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

// A non-struct embedded type becomes a regular field named after the type.
func TestMaskKeepsNonStructEmbeddedAsNamedField(t *testing.T) {
	l, logs := maskedLogger(nil)
	l.Info("any", zap.Any("req", embedNonStructReq{MaskInt: 7, Password: "hunter2"}))

	require.Len(t, logs.All(), 1)
	req, ok := logs.All()[0].ContextMap()["req"].(map[string]any)
	require.True(t, ok, "实际 %T", logs.All()[0].ContextMap()["req"])
	assert.Equal(t, maskPlaceholder, req["password"])
	assert.Equal(t, MaskInt(7), req["MaskInt"], "非 struct 的嵌入按类型名成为普通字段")
}

// The outer field wins when field names collide.
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

// When the hit happens on a named field at this level, an embedded struct
// that didn't hit must still be carried over in its flattened shape as-is --
// it must not be dropped wholesale, nor fall back to type-name nesting.
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

// When the hit happens in a later embedded struct, embedded structs before it
// must be carried into the result as well.
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
// Fix 2: the inner core must not be pulled out via reflection
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
// Fix 5: suffix word group matching
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
// Fix 1's performance boundary: reflected values without a hit must not be
// copied
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
// Fix 6-10: byte-level assertions on the written output
//
// The final effect of reflective masking is decided by encoding/json; the
// observer's ContextMap is only an intermediate representation -- it won't
// tell you whether map[int]X can actually be serialized. All tests below go
// through a real JSON encoder.
// ---------------------------------------------------------------------------

// maskedJSONLoggerWith assembles a real logging pipeline that writes into an
// in-memory buffer; masker appends extra on top of the built-in blacklist.
func maskedJSONLoggerWith(extra []string) (*zap.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	enc := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		MessageKey:  "msg",
		LevelKey:    "lvl",
		EncodeLevel: zapcore.LowercaseLevelEncoder,
	})
	core := zapcore.NewCore(enc, zapcore.AddSync(buf), zapcore.DebugLevel)
	return zap.New(newMaskCore(core, newMasker(extra))), buf
}

// logJSONWith writes one log entry and returns the bytes written both as raw
// text and as a parsed result.
func logJSONWith(t *testing.T, extra []string, fs ...zapcore.Field) (string, map[string]any) {
	t.Helper()
	l, buf := maskedJSONLoggerWith(extra)
	l.Info("m", fs...)
	raw := buf.String()
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &parsed), "落盘字节应是合法 JSON：%s", raw)
	return raw, parsed
}

// logJSON is the common form of logJSONWith: it uses only the built-in
// blacklist.
func logJSON(t *testing.T, fs ...zapcore.Field) (string, map[string]any) {
	t.Helper()
	return logJSONWith(t, nil, fs...)
}

// subMap extracts one nested layer of an object, printing the raw written
// bytes if it fails.
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

// 14: passing a non-string-key map directly. map[int64]Order is the most
// common ID-indexed form; encoding/json stringifies the key in decimal and
// serializes the value as usual -- not recursing into it means it lands on
// disk in plaintext.
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

// 15: a non-string-key map as a struct field -- the nested position must be
// masked the same way.
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

// 16: same level has one key that hits and one non-string-key map.
// This is the ugliest shape -- a log entry that's half masked, half
// plaintext, which can look like "the defense is working".
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

// maskHeaderKey is a key type with its own MarshalText: the underlying type is
// int, but encoding/json uses MarshalText's result as the member name, so the
// member name is a meaningful English word.
type maskHeaderKey int

func (maskHeaderKey) MarshalText() ([]byte, error) { return []byte("authorization"), nil }

// 17: the stringified key must be checked against the blacklist once,
// uniformly.
func TestMaskChecksStringifiedMapKey(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[maskHeaderKey]string{7: "Bearer topsecret"}))

	assert.NotContains(t, raw, "topsecret")
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["authorization"])
}

// 18: float key vs. bool key.
//
// Measured (Go 1.27, encoding/json implemented by json/v2): a float key IS
// supported, serializing to {"1.5":…}; a bool key reports unsupported value,
// which zap records as vError -- no plaintext hits disk. Neither case must
// panic.
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

// sMaskHidden is an unexported embedded type. With a json name tag, it isn't
// flattened -- it's output nested as a regular named field, because
// encoding/json's typeFields skip condition for anonymous fields is
// "unexported AND non-struct type", so it's still collected.
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

// 19: unexported anonymous embed + json name tag, embedded by value.
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

// 20: same as above, but embedded by pointer.
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

// 21: the unexported anonymous embed itself has no hit, but a sibling field
// at the same level does.
//
// This is the regression test for the Interface() pitfall: a sibling hit
// triggers "carry this level's named fields into the map as-is", and the
// reflect.Value of an unexported anonymous embedded field can't get
// Interface() (only the field itself can't -- its exported sub-fields can
// still be read), so copying it as-is would panic.
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

// sMaskBadTag's json tag name is invalid (contains a quote). encoding/json
// won't adopt it as-is, so looking it up in the blacklist necessarily misses
// -- yet the field's value is a genuine, live password.
type sMaskBadTag struct {
	Password string `json:"pass\"word"`
	User     string `json:"user"`
}

// 22: invalid tag name -- the dual-name lookup kicks in.
func TestMaskChecksGoFieldNameWhenTagNameIsInvalid(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskBadTag{Password: "hunter2", User: "alice"}))
	assert.NotContains(t, raw, "hunter2", "tag 名查不中时要退回 Go 字段名，落盘：%s", raw)
}

type sMaskRenamed struct {
	Token string `json:"count"`
}

// 23: the tag name is harmless but the Go field name is a credential -- either
// one hitting triggers masking.
func TestMaskChecksBothTagAndGoFieldName(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskRenamed{Token: "abc.def"}))
	assert.NotContains(t, raw, "abc.def")
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["count"])
}

type sMaskTokenCount struct {
	TokenCount int `json:"n"`
}

// 24: over-reach regression -- the dual-name lookup must not drag a counting
// field like TokenCount down with it.
func TestMaskDoubleNameLookupDoesNotOverreach(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskTokenCount{TokenCount: 42}))
	assert.EqualValues(t, 42, subMap(t, raw, m, "v")["n"], "计数字段不是凭据，落盘：%s", raw)
}

type sMaskConfirm struct {
	PasswordConfirm string `json:"passwordConfirm"`
	Password2       string `json:"password2"`
	PasswordRepeat  string `json:"password_repeat"`
}

// 25: for password-confirmation fields, the value is the plaintext password
// itself, not metadata about the password.
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

// 26: regression for an existing ruling -- these four must not hit; growing
// the blacklist must not sweep them in.
func TestMaskStillDoesNotOverreachAfterBlacklistGrowth(t *testing.T) {
	m := newMasker(nil)
	for _, k := range []string{"password_hash", "token_count", "phone_masked", "mobile_type"} {
		assert.False(t, m.hit(k), "既有裁决：不该命中 %q", k)
	}
}

// map[any]V is legal: measured that {1:…} lands on disk as {"1":…}; the key's
// dynamic type decides the member name. This path (the interface unboxing in
// maskMapKeyName) is new this round and needs a test watching it.
func TestMaskRecursesIntoInterfaceKeyMap(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[any]mapCreds{1: {User: "alice", Password: "hunter2"}}))

	assert.NotContains(t, raw, "hunter2")
	assert.Equal(t, maskPlaceholder, subMap(t, raw, subMap(t, raw, m, "v"), "1")["password"])
}

// NaN / ±Inf can't be encoded as JSON numbers; json reports unsupported value
// and doesn't write anything to disk -- it must not panic, nor take it upon
// itself to reshape a record that can't be encoded into one that can.
func TestMaskLeavesUnencodableFloatKeyMapAlone(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", map[float64]mapCreds{
		math.NaN(): {User: "alice", Password: "hunter2"},
	}))

	assert.NotContains(t, raw, "hunter2")
	assert.Contains(t, m, "vError", "落盘：%s", raw)
}

// ---------------------------------------------------------------------------
// Fix 11-12: name candidates for tags take a union; the ECMAScript boundary
// for float member names
//
// In the tags below, the name portion is truncated by a reserved character
// (one of \ ' " `). encoding/json v2 won't adopt such a name as-is: it
// re-derives a valid identifier from the start of the tag (parseFieldOptions
// -> consumeTagOption), while v1 falls back to the Go field name entirely.
// What the member ends up being called on disk doesn't matter; what matters
// is that the value is a genuine, live password -- only taking the union of
// several candidates can block it.
//
// Every test here goes through the full zap.New(core).Info() path and asserts
// on the bytes written to disk, rather than calling hitFieldName directly:
// the decision function being correct while the defense isn't actually wired
// up must be something the test can detect.
// ---------------------------------------------------------------------------

// sMaskCutQuote: the tag name is truncated by a quote; v2 writes the member as
// password on disk, and the value is a plaintext password.
type sMaskCutQuote struct {
	Foo string `json:"password\"x"`
}

// sMaskCutDash: candidate 1's suffix word groups are extra / tokenextra /
// accesstokenextra, none of which hit; the word window accesstoken in the
// replacement name access_token-extra_y does hit.
type sMaskCutDash struct {
	Foo string `json:"access_token-extra\\y"`
}

type sMaskCutBackslash struct {
	Foo string `json:"token\\x"`
}

type sMaskCutBacktick struct {
	Foo string "json:\"secret`x\""
}

// sMaskCutDot: the garbage lands after the sensitive word. Candidate 1's
// suffix word groups x / passwordx / dbpasswordx all miss; it relies on the
// word window password in the replacement name db_password_x.
type sMaskCutDot struct {
	Foo string `json:"db.password\"x"`
}

// sMaskCutDigit: the tag's first character is a digit, so v2's
// consumeTagOption errors out and the member name falls back to the Go field
// name -- candidate 2 catches it.
type sMaskCutDigit struct {
	Password string `json:"2fa\"x"`
}

// sMaskCutQuoted: single-quoted names aren't allowed in struct tags
// (allowQuoted=false), so this also falls back to the Go field name.
type sMaskCutQuoted struct {
	Token string `json:"'password'"`
}

// sMaskCutCount: over-reach regression -- none of candidate 1, the Go name
// Count, or any word window of the replacement name count-extra_y should hit.
type sMaskCutCount struct {
	Count int `json:"count-extra\\y"`
}

// 27: tag name truncated by a quote -- v2's written member is literally
// called password.
func TestMaskChecksV2TruncatedTagName(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskCutQuote{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`password"x`], "落盘：%s", raw)
}

// 28: neither candidate 1 nor the Go name hits; it relies on the replacement
// name's word window.
func TestMaskChecksV2NameWhenTagPrefixMisses(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskCutDash{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`access_token-extra\y`], "落盘：%s", raw)
}

// 29: backslash and backtick are reserved characters too.
func TestMaskChecksV2NameForOtherReservedChars(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskCutBackslash{Foo: "hunter2"}))
	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`token\x`], "落盘：%s", raw)

	raw, m = logJSON(t, zap.Any("v", sMaskCutBacktick{Foo: "hunter2"}))
	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["secret`x"], "落盘：%s", raw)
}

// 30: the mirror image of 28 (the garbage is after the sensitive word),
// relying on the replacement name's word window as well.
func TestMaskChecksTruncatedTagNameWhenV2NameMisses(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskCutDot{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`db.password"x`], "落盘：%s", raw)
}

// 31: a digit-leading tag; v2 falls back to the Go field name -- candidate 2
// catches it.
func TestMaskFallsBackToGoNameForDigitLeadingTag(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskCutDigit{Password: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`2fa"x`], "落盘：%s", raw)
}

// 32: a single-quoted tag also falls back to the Go field name.
func TestMaskFallsBackToGoNameForQuotedTag(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskCutQuoted{Token: "abc.def"}))

	assert.NotContains(t, raw, "abc.def", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["'password'"], "落盘：%s", raw)
}

// 33: over-reach regression -- adding more candidates must not drag a
// counting field down with it. When none of the candidates hit, the field is
// handed to encoding/json as-is, and it decides the member name on its own
// (v2 gives count, v1 gives Count), so this only asserts on the value and the
// placeholder, not the member name.
func TestMaskExtraNameCandidatesDoNotOverreach(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskCutCount{Count: 42}))

	assert.NotContains(t, raw, maskPlaceholder, "计数字段不是凭据，落盘：%s", raw)
	assert.Contains(t, raw, "42", "落盘：%s", raw)
}

// assertFloatKeyName asserts that the member name written on disk for a float
// map key matches encoding/json byte-for-byte. The expected value is cut
// directly out of json.Marshal's raw bytes (the shape is always fixed as
// {"<name>":1}), without going through Unmarshal -- decoding would restore
// escapes, which would no longer be a byte-for-byte comparison.
func assertFloatKeyName[T float32 | float64](t *testing.T, v T) {
	t.Helper()
	b, err := json.Marshal(map[T]int{v: 1})
	require.NoError(t, err)
	s := string(b)
	require.True(t, strings.HasPrefix(s, `{"`) && strings.HasSuffix(s, `":1}`), "意外的形状：%s", s)
	want := s[2 : len(s)-4]

	raw, m := logJSON(t, zap.Any("v", map[T]mapCreds{v: {User: "alice", Password: "hunter2"}}))
	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Contains(t, raw, `"`+want+`":{`, "成员名要与 encoding/json 逐字节相同（%v），落盘：%s", v, raw)
	assert.Equal(t, maskPlaceholder,
		subMap(t, raw, subMap(t, raw, m, "v"), want)["password"], "落盘：%s", raw)
}

// 34: the ECMAScript boundary for float member names.
//
// maskFormatFloat replicates the rule encoding/json uses to write numbers
// (ECMAScript's Number::toString): 'f' is used when |x| falls in
// [1e-6, 1e21), otherwise 'e' is used and e-09 is collapsed to e-9. Using
// strconv's 'g' directly writes 1e+20 around the 1e20 mark, while json writes
// 100000000000000000000 -- the member name wouldn't match, and search rules
// targeting that member would stop working. The existing 1.5 / NaN tests
// don't touch these two turning points, so this one fills the gap.
func TestMaskFloatMapKeyNameMatchesJSON(t *testing.T) {
	for _, v := range []float64{
		1e-7, 1e-6, 1e20, 1e21, math.Copysign(0, -1), 5e-324, 1.5, 1e-9, -1e-7,
	} {
		assertFloatKeyName(t, v)
	}
	for _, v := range []float32{1e-7, 1e-6, 1e20, 1e21} {
		assertFloatKeyName(t, v)
	}
}

// ---------------------------------------------------------------------------
// A reserved character lands **before** the sensitive head word
//
// Same family as the previous group, direction reversed: there the garbage
// landed after the sensitive word, here it lands before. Together with the
// previous group and the "sandwiched from both sides" group below, they show
// that enumerating candidates by position doesn't work -- what blocks all of
// them is the same rule: word-window matching on the replacement name.
//
// The criterion is always "the sensitive head word appears somewhere in the
// tag text, so block it": a corrupted tag doesn't make the value any less
// sensitive -- a field like `Password string `json:"db\"password"`` still
// holds a plaintext password.
//
// Every test here goes through the full zap.New(core).Info() path and asserts
// on the bytes written to disk, rather than calling hitFieldName directly.
// ---------------------------------------------------------------------------

// sMaskPreQuote: the Go name Foo isn't sensitive, and candidate 1's
// db"password is a single word (a quote isn't a separator) -- it relies on
// the word window of the replacement name db_password.
type sMaskPreQuote struct {
	Foo string `json:"db\"password"`
}

type sMaskPreBackslash struct {
	Foo string `json:"db\\password"`
}

type sMaskPreBacktick struct {
	Foo string "json:\"db`password\""
}

type sMaskPreSecret struct {
	Foo string `json:"svc\\secret"`
}

// sMaskPreUnderscore: after replacement it's _x_private_key, with the words
// x / private / key; the suffix word group privatekey hits -- this verifies
// the replacement name still goes through the suffix-word-group rule, not a
// whole-string comparison.
type sMaskPreUnderscore struct {
	Foo string `json:"_x\"private_key"`
}

// sMaskPreTokenizer: the replacement name tokenizer_x's suffix word groups are
// x / tokenizerx, neither of which should hit -- the fifth candidate must not
// drag a word like tokenizer down with it.
type sMaskPreTokenizer struct {
	Foo string `json:"tokenizer\"x"`
}

// sMaskCJKTag: candidate 1's suffix word groups don't hit; it relies on the
// word window 密码 in the replacement name 密码-extra_y.
// splitMaskKey splits words byte by byte and doesn't interpret non-ASCII
// bytes, so a CJK head word still forms its own word.
// The built-in blacklist is all English, so 密码 must be appended to masker
// explicitly -- otherwise this test would stay green even if the wording were
// changed to ASCII.
type sMaskCJKTag struct {
	Foo string `json:"密码-extra\"y"`
}

// 35: a reserved character lands before the sensitive head word -- what used
// to land on disk was {"db":"hunter2"}, plaintext all the way through.
func TestMaskChecksReplacedNameWhenReservedCharPrecedesKeyword(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskPreQuote{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`db"password`], "落盘：%s", raw)
}

// 36: backslash and backtick are the same family of shape.
func TestMaskChecksReplacedNameForOtherReservedChars(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskPreBackslash{Foo: "hunter2"}))
	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`db\password`], "落盘：%s", raw)

	raw, m = logJSON(t, zap.Any("v", sMaskPreBacktick{Foo: "hunter2"}))
	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["db`password"], "落盘：%s", raw)
}

// 37: swap the head word for secret and the prefix for svc -- this isn't
// effective for the word password alone.
func TestMaskChecksReplacedNameForSecretKeyword(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskPreSecret{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`svc\secret`], "落盘：%s", raw)
}

// 38: after replacement it still follows the suffix-word-group rule --
// _x"private_key -> _x_private_key, words x / private / key, and the suffix
// word group privatekey hits (a standalone key is not in the blacklist).
func TestMaskReplacedNameStillUsesSuffixWordGroups(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskPreUnderscore{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")[`_x"private_key`], "落盘：%s", raw)
}

// 39: over-reach regression, a strengthened version of #33 -- none of the
// fifth candidate count-extra_y's suffix word groups y / extray / countextray
// should hit. The member name on disk is decided by encoding/json itself
// (v2 gives count, v1 gives Count), so this only asserts on the value and the
// placeholder.
func TestMaskReplacedNameCandidateDoesNotOverreach(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskCutCount{Count: 42}))

	assert.NotContains(t, raw, maskPlaceholder, "计数字段不是凭据，落盘：%s", raw)
	assert.Contains(t, raw, "42", "落盘：%s", raw)
}

// 40: tokenizer and token only share a prefix; the head word isn't a
// credential -- the fifth candidate must not over-reach here.
func TestMaskReplacedNameDoesNotOverreachOnPrefixLookalike(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskPreTokenizer{Foo: "visible"}))

	assert.NotContains(t, raw, maskPlaceholder, "落盘：%s", raw)
	assert.Contains(t, raw, "visible", "落盘：%s", raw)
}

// 41: CJK tag. The built-in blacklist is all English, so newMasker must be
// used to append "密码" -- otherwise this test would stay green no matter how
// the implementation changed, pinning down nothing.
func TestMaskChecksV2NameForCJKTag(t *testing.T) {
	raw, m := logJSONWith(t, []string{"密码"}, zap.Any("v", sMaskCJKTag{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["密码-extra\"y"], "落盘：%s", raw)
}

// ---------------------------------------------------------------------------
// Sandwiched from both sides: garbage lands on both sides of the sensitive
// head word at once
//
// This is the fourth shape missed by enumerating candidates by "position"
// (before / middle / after), and it's why the replacement name's
// suffix-word-group matching was swapped for word-window matching. A word
// window is position-agnostic, covering all four shapes in one go.
// ---------------------------------------------------------------------------

// sMaskSandwichPassword: the replacement name db_password_x has three words,
// db / password / x; the suffix word groups x / passwordx / dbpasswordx all
// miss, and both the truncated name and the v2 name are just db. Only the
// word window can reach the password sandwiched in the middle.
type sMaskSandwichPassword struct {
	Foo string `json:"db\\password\\x"`
}

// sMaskSandwichCompound: the head word itself is a compound word, so the
// window must be able to reach the concatenation of two adjacent words.
type sMaskSandwichCompound struct {
	Foo string `json:"svc\\secret_key\\extra"`
}

// sMaskSandwichMixed: three kinds of reserved characters mixed together, with
// access_token as the head word.
type sMaskSandwichMixed struct {
	Foo string `json:"user'access_token\"extra"`
}

// sMaskAfterComma: the reserved character lands after ,omitempty. Candidate 1
// truncates everything after the comma, so only the replacement name, which
// walks the whole tag, can reach it.
type sMaskAfterComma struct {
	Foo string `json:"db,omitempty\"password"`
}

// sMaskSandwichBenign: over-reach guard -- a benign head word sandwiched by
// the same kind of garbage should not be masked.
type sMaskSandwichBenign struct {
	Count int `json:"svc\\count-extra\\y"`
}

func TestMaskChecksWindowForSandwichedTag(t *testing.T) {
	raw, m := logJSON(t, zap.Any("v", sMaskSandwichPassword{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
	assert.Equal(t, maskPlaceholder, subMap(t, raw, m, "v")["db\\password\\x"], "落盘：%s", raw)
}

func TestMaskChecksWindowForSandwichedCompoundWord(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskSandwichCompound{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
}

func TestMaskChecksWindowForMixedReservedChars(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskSandwichMixed{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
}

func TestMaskChecksWholeTagAfterComma(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskAfterComma{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
}

func TestMaskWindowDoesNotOverreachBenignSandwich(t *testing.T) {
	// v2's member name is truncated at the first reserved character, so the
	// member written on disk is svc; keeping the value as-is is correct here.
	raw, m := logJSON(t, zap.Any("v", sMaskSandwichBenign{Count: 7}))

	assert.Equal(t, float64(7), subMap(t, raw, m, "v")["svc"], "落盘：%s", raw)
}

// sMaskReservedThenComma: a reserved character comes first, then a comma. A
// comma is not one of splitMaskKey's separators, so if it isn't also
// translated to `_`, password,omitempty would stick together as one word and
// the window couldn't reach it.
type sMaskReservedThenComma struct {
	Foo string `json:"x\"password,omitempty"`
}

func TestMaskChecksWindowAcrossCommaOption(t *testing.T) {
	raw, _ := logJSON(t, zap.Any("v", sMaskReservedThenComma{Foo: "hunter2"}))

	assert.NotContains(t, raw, "hunter2", "落盘：%s", raw)
}
