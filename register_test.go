package xbc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withCleanRegistry isolates a test from the package-level `registered`
// slice, which is process-global mutable state shared by every test in this
// binary (Go runs all tests in a package in one process). Without this, a
// plugin left behind by an earlier test would still be sitting in
// `registered` when New() seeds a later test's App, and two tests that both
// happen to derive the same name (very likely here, since every fixture in
// this file lives in the same package and therefore derives the same
// package-path name) would spuriously collide.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	old := registered
	registered = nil
	t.Cleanup(func() { registered = old })
}

// overriddenNamePlugin writes its own Name(), so it must win over whatever
// deriveName would have produced -- Go's method resolution never even calls
// Base's promoted Name() here.
type overriddenNamePlugin struct{ Base }

func (p *overriddenNamePlugin) Name() string { return "custom-name" }

// noBasePlugin doesn't embed Base at all. It's still a legal plugin: the
// Plugin interface only requires Name(), and it implements that itself.
type noBasePlugin struct{}

func (p *noBasePlugin) Name() string { return "standalone" }

// autoNamedPlugin embeds Base and never overrides Name(), so its name must
// come from deriveName. Its concrete type lives directly in this package
// (root package xbc), so the derived name is this package's own last path
// segment -- see TestDeriveNameFromPackagePath and
// TestDeriveNameStripsMajorVersionSuffix above for the "real nested package
// path" cases, which is why they use external fixtures instead of a type
// defined here.
type autoNamedPlugin struct{ Base }

// conflictPluginA and conflictPluginB both embed Base without overriding
// Name(), so both derive this package's name -- a deliberate, guaranteed
// collision used to test the conflict check itself.
type conflictPluginA struct{ Base }
type conflictPluginB struct{ Base }

// multiCapablePlugin implements MultiInstancer, so newEntry must record
// entry.multi = true for it.
type multiCapablePlugin struct{ Base }

func (p *multiCapablePlugin) MultiInstance() bool { return true }

// emptyNameNoBasePlugin satisfies Plugin (Name() is implemented) but returns
// an empty string and does not embed Base. newEntry falls back to
// deriveName in the empty-string branch, but bindBase then has nothing to
// bind into -- there is no embedded Base to reach through baseAnchor -- so
// this must be rejected rather than silently accepted.
type emptyNameNoBasePlugin struct{}

func (p *emptyNameNoBasePlugin) Name() string { return "" }

func TestRegisterRespectsOverriddenName(t *testing.T) {
	app := New()
	app.Register(&overriddenNamePlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, "custom-name", app.entries[0].name,
		"插件自己实现的 Name() 必须遮蔽框架推导的名字")
	assert.Equal(t, sourceRegister, app.entries[0].src)
}

func TestRegisterAcceptsPluginWithoutBase(t *testing.T) {
	app := New()
	app.Register(&noBasePlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, "standalone", app.entries[0].name, "不嵌 Base 的插件照样合法")
}

func TestRegisterDerivesNameFromBaseWhenNotOverridden(t *testing.T) {
	app := New()
	app.Register(&autoNamedPlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, "xbc", app.entries[0].name,
		"嵌入 Base 且未覆盖 Name() 时，框架从包路径推导")
}

func TestPackageRegisterMarksSourceImport(t *testing.T) {
	withCleanRegistry(t)
	Register(&noBasePlugin{})
	require.Len(t, registered, 1)
	assert.Equal(t, sourceImport, registered[0].src, "包级 Register 必须标记为 sourceImport")
	assert.Equal(t, "standalone", registered[0].name)
}

func TestAppRegisterMarksSourceRegister(t *testing.T) {
	app := New()
	app.Register(&noBasePlugin{})
	require.Len(t, app.entries, 1)
	assert.Equal(t, sourceRegister, app.entries[0].src, "App.Register 必须标记为 sourceRegister")
}

func TestAppRegisterRejectsNameConflict(t *testing.T) {
	app := New()
	app.Register(&conflictPluginA{})
	assert.PanicsWithError(t,
		`xbc: 插件名 "xbc" 冲突：*xbc.conflictPluginA 与 *xbc.conflictPluginB 推导出同一个名字，请给其中一个显式实现 Name() 改名`,
		func() { app.Register(&conflictPluginB{}) },
		"两个插件推导出同名必须在 Register 时就报错",
	)
}

func TestPackageRegisterRejectsNameConflict(t *testing.T) {
	withCleanRegistry(t)
	Register(&conflictPluginA{})
	assert.PanicsWithError(t,
		`xbc: 插件名 "xbc" 冲突：*xbc.conflictPluginA 与 *xbc.conflictPluginB 推导出同一个名字，请给其中一个显式实现 Name() 改名`,
		func() { Register(&conflictPluginB{}) },
	)
}

func TestNewEntryDetectsMultiInstancer(t *testing.T) {
	app := New()
	app.Register(&multiCapablePlugin{})
	require.Len(t, app.entries, 1)
	assert.True(t, app.entries[0].multi,
		"实现 MultiInstancer 且返回 true 时 entry.multi 必须为 true")
}

func TestNewSeedsFromPackageLevelRegistrations(t *testing.T) {
	withCleanRegistry(t)
	Register(&noBasePlugin{})
	app := New()
	require.Len(t, app.entries, 1, "New() 必须把包级 Register 的插件带入 App")
	assert.Equal(t, sourceImport, app.entries[0].src)
}

func TestAppRegisterRejectsEmptyNameWithoutBase(t *testing.T) {
	app := New()
	assert.PanicsWithError(t,
		`xbc: 插件 *xbc.emptyNameNoBasePlugin 的 Name() 返回空字符串，且未嵌入 xbc.Base，无法确定插件名`,
		func() { app.Register(&emptyNameNoBasePlugin{}) },
		"Name() 返回空字符串且未嵌入 Base 时必须 panic，而不是静默接受",
	)
}
