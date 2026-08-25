// Package jwt is a minimal plugin fixture used only by xbc's own tests, to
// exercise deriveName's package-path parsing with a real, nested package
// path. It deliberately does NOT import github.com/xbcio/xbc (embedding
// xbc.Base would): xbc's internal ("package xbc") test files cannot import a
// package that itself imports xbc, because Go treats that as an import
// cycle even when the only path back to xbc runs through a _test.go file.
// Plugin identity only needs to be satisfied structurally -- a Name method
// is enough, no embedding required.
package jwt

// Plugin is a stand-in plugin type. Its Name method is never actually called
// by the tests that use this fixture -- they call xbc's unexported
// deriveName directly, which only inspects the type's package path via
// reflection and never invokes Name() at all.
type Plugin struct{}

func (p *Plugin) Name() string { return "" }
