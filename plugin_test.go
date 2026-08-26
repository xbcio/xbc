package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	foo "github.com/xbcio/xbc/internal/testplugins/foo/v2"
	"github.com/xbcio/xbc/internal/testplugins/jwt"
)

// deriveName's package-path parsing is the one piece of this file that
// genuinely needs a real, nested package path to exercise properly -- but
// this test file is "package xbc" (an internal test, per the plan's global
// constraint), so any type defined right here would derive "xbc" itself,
// telling us nothing about path parsing. The two fixtures under
// internal/testplugins/ exist purely to give deriveName a realistic,
// multi-segment path to chew on; see their own doc comments for why they
// don't embed xbc.Base.
func TestDeriveNameFromPackagePath(t *testing.T) {
	name, err := deriveName(&jwt.Plugin{})
	require.NoError(t, err)
	assert.Equal(t, "jwt", name, "取包路径末段")
}

func TestDeriveNameStripsMajorVersionSuffix(t *testing.T) {
	name, err := deriveName(&foo.Plugin{})
	require.NoError(t, err)
	assert.Equal(t, "foo", name, "末段形如 v+数字时取前一段")
}

// TestIsMajorVersionSegmentRejectsVPrefixedWords pins the rejection branch of
// isMajorVersionSegment: a package path segment that merely starts with "v"
// (a real plugin package named vault, view, or validator is exactly this
// shape) must NOT be mistaken for a Go major-version element like "v2" or
// "v10". Before this test, flipping the inner loop's "return false" to
// "return true" -- which would treat every v-prefixed, length->=2 string as
// a version segment regardless of what follows "v" -- made the entire suite
// pass, because nothing exercised this branch with a non-numeric case.
func TestIsMajorVersionSegmentRejectsVPrefixedWords(t *testing.T) {
	cases := []string{"vault", "view", "validator", "vx1", "v1a", "value2"}
	for _, s := range cases {
		assert.False(t, isMajorVersionSegment(s), "%q 不是版本号段，不应被误判", s)
	}
}

func TestIsMajorVersionSegmentAcceptsRealVersionSegments(t *testing.T) {
	for _, s := range []string{"v2", "v10", "v999"} {
		assert.True(t, isMajorVersionSegment(s), "%q 是合法的 Go 主版本路径段", s)
	}
}

func TestIsMajorVersionSegmentRejectsTooShortOrWrongPrefix(t *testing.T) {
	for _, s := range []string{"", "v", "2", "a2"} {
		assert.False(t, isMajorVersionSegment(s), "%q 既不够长也不是以 v 开头的纯数字，不是版本号段", s)
	}
}

// notAPointerPlugin and nonStructPlugin don't need a realistic package path
// -- deriveName rejects them before it ever looks at PkgPath -- so they're
// defined right here instead of as fixtures.
type notAPointerPlugin struct{}

func (notAPointerPlugin) Name() string { return "value" }

type nonStructPlugin int

func (p *nonStructPlugin) Name() string { return "nonstruct" }

func TestDeriveNameRejectsNonPointer(t *testing.T) {
	_, err := deriveName(notAPointerPlugin{})
	assert.Error(t, err, "非指针插件不能自动推导名字")
}

func TestDeriveNameRejectsNonStructPointer(t *testing.T) {
	var p nonStructPlugin
	_, err := deriveName(&p)
	assert.Error(t, err, "指向非结构体的指针不能自动推导名字")
}

func TestValidateNameRejectsReservedCharacters(t *testing.T) {
	cases := []string{"a.b", "a[b]", "a]b", "a b", "ABC", "用户"}
	for _, s := range cases {
		assert.Error(t, validateName(s), "名字 %q 应当被拒绝", s)
	}
}

func TestValidateNameAcceptsPlainLowercase(t *testing.T) {
	for _, s := range []string{"jwt", "gorm_v2", "rate-limit", "a1"} {
		assert.NoError(t, validateName(s), "名字 %q 应当合法", s)
	}
}

func TestValidateNameRejectsEmpty(t *testing.T) {
	assert.Error(t, validateName(""), "空名字不合法")
}

// TestValidateInstanceNameSharesValidateNameRule pins that validateInstanceName
// is not a weaker cousin of validateName -- both go through validateIdentifier,
// so an instance name is held to exactly the same character-set rule as a
// plugin name (see stage_expand.go's expandMulti, which is validateInstanceName's
// only production caller).
func TestValidateInstanceNameSharesValidateNameRule(t *testing.T) {
	for _, s := range []string{"a.b", "a[b]", "a]b", "a b", "ABC", "用户", ""} {
		assert.Error(t, validateInstanceName(s), "实例名 %q 应当被拒绝", s)
	}
	for _, s := range []string{"default", "readonly", "read_only", "read-only", "a1"} {
		assert.NoError(t, validateInstanceName(s), "实例名 %q 应当合法", s)
	}
}

// TestValidateInstanceNameErrorMentionsInstanceNotPlugin pins that the error
// text says "实例名", not "插件名" -- reusing validateName's message verbatim
// for an instance-name failure would misdirect a user staring at a
// plugins.gorm.<instance> typo toward looking at the plugin name instead.
func TestValidateInstanceNameErrorMentionsInstanceNotPlugin(t *testing.T) {
	err := validateInstanceName("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "实例名")
	assert.NotContains(t, err.Error(), "插件名")
}
