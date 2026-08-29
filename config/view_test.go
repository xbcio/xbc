package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopeRestrictsPathsAndPreservesEmptyPathSemantics(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"web": map[string]any{
				"addr": ":8080",
				"tls":  map[string]any{"enabled": true},
			},
			"database": map[string]any{"dsn": "secret"},
		},
	}, "")
	require.NoError(t, err)

	view := Scope(env, "plugins.web")
	assert.Equal(t, ":8080", view.Get("addr"))
	assert.True(t, view.Exists("tls.enabled"))
	assert.Equal(t, map[string]any{"enabled": true}, view.Sub("tls"))

	root, ok := view.Get("").(map[string]any)
	require.True(t, ok)
	assert.Equal(t, ":8080", root["addr"])
	assert.True(t, view.Exists(""), "Empty path should point to scoped root")
	assert.Equal(t, root, view.Sub(""))

	assert.Nil(t, view.Get("plugins.database.dsn"), "Relative path cannot escape to same-level or global section")
	assert.False(t, view.Exists("plugins.database.dsn"))
}

func TestScopeKeepsRecursiveDefensiveCopies(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"web": map[string]any{
				"nested": map[string]any{
					"hosts": []string{"a", "b"},
					"items": []any{map[string]any{"enabled": true}},
				},
			},
		},
	}, "")
	require.NoError(t, err)
	view := Scope(env, "plugins.web")

	first := view.Sub("")
	first["nested"].(map[string]any)["hosts"].([]string)[0] = "changed"
	first["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["enabled"] = false
	first["extra"] = true

	again := view.Sub("")
	assert.Equal(t, []string{"a", "b"}, again["nested"].(map[string]any)["hosts"])
	assert.Equal(t, true, again["nested"].(map[string]any)["items"].([]any)[0].(map[string]any)["enabled"])
	assert.NotContains(t, again, "extra")
}

func TestScopeCanBeNestedAndEmptyPrefixKeepsRoot(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"web": map[string]any{"tls": map[string]any{"enabled": true}},
		},
	}, "")
	require.NoError(t, err)

	pluginView := Scope(env, "plugins.web")
	tlsView := Scope(pluginView, "tls")
	assert.Equal(t, true, tlsView.Get("enabled"))
	assert.Equal(t, map[string]any{"enabled": true}, tlsView.Sub(""))

	rootView := Scope(env, "")
	assert.Equal(t, env.Sub("plugins"), rootView.Sub("plugins"))
}

func TestScopeHandlesNilView(t *testing.T) {
	view := Scope(nil, "plugins.web")
	assert.Nil(t, view.Get(""))
	assert.False(t, view.Exists(""))
	assert.Nil(t, view.Sub(""))
}

type leakyView struct {
	value map[string]any
}

func (v *leakyView) Get(string) any            { return v.value }
func (v *leakyView) Exists(string) bool        { return true }
func (v *leakyView) Sub(string) map[string]any { return v.value }

func TestScopeEnforcesDefensiveCopyForAnyViewImplementation(t *testing.T) {
	source := &leakyView{value: map[string]any{
		"nested": map[string]any{"items": []string{"original"}},
	}}
	view := Scope(source, "prefix")

	got := view.Get("").(map[string]any)
	got["nested"].(map[string]any)["items"].([]string)[0] = "changed"
	got["extra"] = true

	assert.Equal(t, "original", source.value["nested"].(map[string]any)["items"].([]string)[0])
	assert.NotContains(t, source.value, "extra")
}
