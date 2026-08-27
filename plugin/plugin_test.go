package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateNameRejectsReservedCharacters(t *testing.T) {
	cases := []string{"a.b", "a[b]", "a]b", "a b", "ABC", "用户"}
	for _, s := range cases {
		assert.Error(t, ValidateName(s), "名字 %q 应当被拒绝", s)
	}
}

func TestValidateNameAcceptsPlainLowercase(t *testing.T) {
	for _, s := range []string{"jwt", "gorm_v2", "rate-limit", "a1"} {
		assert.NoError(t, ValidateName(s), "名字 %q 应当合法", s)
	}
}

func TestValidateNameRejectsEmpty(t *testing.T) {
	assert.Error(t, ValidateName(""), "空名字不合法")
}

// TestValidateInstanceNameSharesValidateNameRule pins that ValidateInstanceName
// is not a weaker cousin of ValidateName -- both go through validateIdentifier,
// so an instance name is held to exactly the same character-set rule as a
// plugin key.
func TestValidateInstanceNameSharesValidateNameRule(t *testing.T) {
	for _, s := range []string{"a.b", "a[b]", "a]b", "a b", "ABC", "用户", ""} {
		assert.Error(t, ValidateInstanceName(s), "实例名 %q 应当被拒绝", s)
	}
	for _, s := range []string{"default", "readonly", "read_only", "read-only", "a1"} {
		assert.NoError(t, ValidateInstanceName(s), "实例名 %q 应当合法", s)
	}
}

// TestValidateInstanceNameErrorMentionsInstanceNotPluginKey pins that the
// error text says "实例名", not "插件 key" -- reusing ValidateName's message
// verbatim for an instance-name failure would misdirect a user staring at a
// plugins.gorm.<instance> typo toward looking at the Definition key instead.
func TestValidateInstanceNameErrorMentionsInstanceNotPluginKey(t *testing.T) {
	err := ValidateInstanceName("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "实例名")
	assert.NotContains(t, err.Error(), "插件 key")
}

// basePlugin embeds Base so bindBase and the Ctx()/Log()/Name() accessors
// have a realistic embedding to operate on.
type basePlugin struct{ Base }

func (p *basePlugin) actualName() string { return p.Name() }

func TestBaseCtxIsNilBeforeBind(t *testing.T) {
	p := &basePlugin{}
	assert.Nil(t, p.Ctx(), "bindBase 尚未运行时 Ctx() 必须是 nil，而不是解引用崩溃")
}

// TestBaseLogFallsBackToGlobalLoggerBeforeBind pins that Base.Log() is safe
// to call before bindBase has wired in a Context -- it must not panic on a
// nil ctx, and instead falls back to the process-wide logger.
func TestBaseLogFallsBackToGlobalLoggerBeforeBind(t *testing.T) {
	p := &basePlugin{}
	assert.NotPanics(t, func() { p.Log() })
}

// TestBindBaseWiresContextAndKey pins that bindBase actually mutates the
// embedded Base in place, and that a plugin without an embedded Base
// reports back false rather than panicking.
func TestBindBaseWiresContextAndKey(t *testing.T) {
	p := &basePlugin{}
	ctx := &Context{id: Identity{Plugin: "demo"}}
	ok := bindBase(p, ctx, "demo")
	require.True(t, ok, "basePlugin 嵌入了 Base，bindBase 必须成功")
	assert.Same(t, ctx, p.Ctx())
	assert.Equal(t, "demo", p.actualName())
}

type noBasePlugin struct{}

func TestBindBaseReturnsFalseWithoutEmbeddedBase(t *testing.T) {
	p := &noBasePlugin{}
	ok := bindBase(p, &Context{}, "no-base")
	assert.False(t, ok, "未嵌入 Base 的插件没有可绑定的锚点")
}

// TestBindRuntimeContextForwardsToBindBase pins that the exported BindRuntimeContext
// entry point (host.go) is a real forward to bindBase, not a separate
// reimplementation: it must observe the exact same wiring and the exact
// same false-for-no-Base result.
func TestBindRuntimeContextForwardsToBindBase(t *testing.T) {
	p := &basePlugin{}
	ctx := &Context{id: Identity{Plugin: "demo"}}
	ok := BindRuntimeContext(p, ctx, "demo")
	require.True(t, ok, "basePlugin 嵌入了 Base，BindRuntimeContext 必须成功")
	assert.Same(t, ctx, p.Ctx())
	assert.Equal(t, "demo", p.actualName())

	np := &noBasePlugin{}
	assert.False(t, BindRuntimeContext(np, &Context{}, "no-base"), "未嵌入 Base 时 BindRuntimeContext 也必须返回 false")
}

type nilEmbeddedBasePlugin struct{ *Base }

func TestBindBaseTreatsNilEmbeddedBaseAsAbsent(t *testing.T) {
	p := nilEmbeddedBasePlugin{}
	assert.NotPanics(t, func() {
		assert.False(t, bindBase(p, &Context{}, "nil-base"),
			"nil 的可选 *Base 无法绑定，但不能让合法 marker Plugin panic")
	})
}
