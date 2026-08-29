package plugin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateNameRejectsReservedCharacters(t *testing.T) {
	cases := []string{"a.b", "a[b]", "a]b", "a b", "ABC", "User"}
	for _, s := range cases {
		assert.Error(t, ValidateName(s), "Name %q should be rejected", s)
	}
}

func TestValidateNameAcceptsPlainLowercase(t *testing.T) {
	for _, s := range []string{"jwt", "gorm_v2", "rate-limit", "a1"} {
		assert.NoError(t, ValidateName(s), "Name %q should be valid", s)
	}
}

func TestValidateNameRejectsEmpty(t *testing.T) {
	assert.Error(t, ValidateName(""), "Empty name is invalid")
}

// TestValidateInstanceNameSharesValidateNameRule pins that ValidateInstanceName
// is not a weaker cousin of ValidateName -- both go through validateIdentifier,
// so an instance name is held to exactly the same character-set rule as a
// plugin key.
func TestValidateInstanceNameSharesValidateNameRule(t *testing.T) {
	for _, s := range []string{"a.b", "a[b]", "a]b", "a b", "ABC", "User", ""} {
		assert.Error(t, ValidateInstanceName(s), "Instance name %q should be rejected", s)
	}
	for _, s := range []string{"default", "readonly", "read_only", "read-only", "a1"} {
		assert.NoError(t, ValidateInstanceName(s), "Instance name %q should be valid", s)
	}
}

// TestValidateInstanceNameErrorMentionsInstanceNotPluginKey pins that the
// error text says "instance name", not "plugin key" -- reusing ValidateName's message
// verbatim for an instance-name failure would misdirect a user staring at a
// plugins.gorm.<instance> typo toward looking at the Definition key instead.
func TestValidateInstanceNameErrorMentionsInstanceNotPluginKey(t *testing.T) {
	err := ValidateInstanceName("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instance name")
	assert.NotContains(t, err.Error(), "plugin key")
}

// basePlugin embeds Base so bindBase and the Ctx()/Log()/Name() accessors
// have a realistic embedding to operate on.
type basePlugin struct{ Base }

func (p *basePlugin) actualName() string { return p.Name() }

func TestBaseCtxIsNilBeforeBind(t *testing.T) {
	p := &basePlugin{}
	assert.Nil(t, p.Ctx(), "Ctx() must be nil before bindBase runs, instead of dereference crash")
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
	require.True(t, ok, "basePlugin embeds Base, bindBase must succeed")
	assert.Same(t, ctx, p.Ctx())
	assert.Equal(t, "demo", p.actualName())
}

type noBasePlugin struct{}

func TestBindBaseReturnsFalseWithoutEmbeddedBase(t *testing.T) {
	p := &noBasePlugin{}
	ok := bindBase(p, &Context{}, "no-base")
	assert.False(t, ok, "Plugins not embedding Base have no bindable anchor")
}

// TestBindRuntimeContextForwardsToBindBase pins that the exported BindRuntimeContext
// entry point (host.go) is a real forward to bindBase, not a separate
// reimplementation: it must observe the exact same wiring and the exact
// same false-for-no-Base result.
func TestBindRuntimeContextForwardsToBindBase(t *testing.T) {
	p := &basePlugin{}
	ctx := &Context{id: Identity{Plugin: "demo"}}
	ok := BindRuntimeContext(p, ctx, "demo")
	require.True(t, ok, "basePlugin embeds Base, BindRuntimeContext must succeed")
	assert.Same(t, ctx, p.Ctx())
	assert.Equal(t, "demo", p.actualName())

	np := &noBasePlugin{}
	assert.False(t, BindRuntimeContext(np, &Context{}, "no-base"), "BindRuntimeContext must also return false when not embedding Base")
}

type nilEmbeddedBasePlugin struct{ *Base }

func TestBindBaseTreatsNilEmbeddedBaseAsAbsent(t *testing.T) {
	p := nilEmbeddedBasePlugin{}
	assert.NotPanics(t, func() {
		assert.False(t, bindBase(p, &Context{}, "nil-base"),
			"Nil optional *Base cannot be bound, but cannot let valid marker Plugin panic")
	})
}
