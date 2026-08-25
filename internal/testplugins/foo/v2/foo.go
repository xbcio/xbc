// Package foo, nested under a "v2" directory, is a minimal plugin fixture
// used only by xbc's own tests, to exercise deriveName's major-version-
// suffix stripping (".../foo/v2" -> "foo"). See the sibling jwt fixture for
// why this package doesn't import xbc.
package foo

// Plugin is a stand-in plugin type; see jwt.Plugin's comment for why a bare
// Name method is enough here.
type Plugin struct{}

func (p *Plugin) Name() string { return "" }
