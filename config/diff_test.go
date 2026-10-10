package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChangedPathsReportsChangedAddedAndRemovedLeaves(t *testing.T) {
	cases := []struct {
		name   string
		before map[string]any
		after  map[string]any
		want   []string
	}{
		{
			name:   "an unchanged tree has no changed paths",
			before: map[string]any{"app": map[string]any{"a": 1, "b": "x"}},
			after:  map[string]any{"app": map[string]any{"a": 1, "b": "x"}},
			want:   nil,
		},
		{
			name:   "a changed scalar is reported by its own path",
			before: map[string]any{"app": map[string]any{"a": 1}},
			after:  map[string]any{"app": map[string]any{"a": 2}},
			want:   []string{"app.a"},
		},
		{
			name:   "a changed type is a change",
			before: map[string]any{"app": map[string]any{"a": "1"}},
			after:  map[string]any{"app": map[string]any{"a": 1}},
			want:   []string{"app.a"},
		},
		{
			name:   "a changed element reports the whole leaf",
			before: map[string]any{"app": map[string]any{"list": []any{1, 2}}},
			after:  map[string]any{"app": map[string]any{"list": []any{1, 3}}},
			want:   []string{"app.list"},
		},
		{
			name:   "an added leaf is reported",
			before: map[string]any{"app": map[string]any{"a": 1}},
			after:  map[string]any{"app": map[string]any{"a": 1, "b": 2}},
			want:   []string{"app.b"},
		},
		{
			name:   "a removed leaf is reported",
			before: map[string]any{"app": map[string]any{"a": 1, "b": 2}},
			after:  map[string]any{"app": map[string]any{"a": 1}},
			want:   []string{"app.b"},
		},
		{
			name:   "a whole section that appears is reported by its leaves",
			before: map[string]any{},
			after:  map[string]any{"plugins": map[string]any{"web": map[string]any{"enabled": true}}},
			want:   []string{"plugins.web.enabled"},
		},
		{
			name:   "a whole section that disappears is reported by its leaves",
			before: map[string]any{"plugins": map[string]any{"web": map[string]any{"enabled": true}}},
			after:  map[string]any{},
			want:   []string{"plugins.web.enabled"},
		},
		{
			name:   "an empty section is a leaf of its own",
			before: map[string]any{"a": map[string]any{"b": 1}},
			after:  map[string]any{"a": map[string]any{}},
			want:   []string{"a", "a.b"},
		},
		{
			name:   "several changes come back sorted",
			before: map[string]any{"a": map[string]any{"z": 0}},
			after:  map[string]any{"b": map[string]any{"y": 1}, "a": map[string]any{"x": 1}},
			want:   []string{"a.x", "a.z", "b.y"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := newTestEnvironment(t, tc.before)
			after := newTestEnvironment(t, tc.after)
			assert.Equal(t, tc.want, ChangedPaths(before, after))
		})
	}
}

func TestChangedPathsTreatsANilEnvironmentAsEmpty(t *testing.T) {
	env := newTestEnvironment(t, map[string]any{"app": map[string]any{"a": 1}})
	assert.Equal(t, []string{"app.a"}, ChangedPaths(nil, env))
	assert.Equal(t, []string{"app.a"}, ChangedPaths(env, nil))
	assert.Empty(t, ChangedPaths(nil, nil))
}

// TestChangedPathsComparesTheLoadedTreeNotTheBoundView pins the reason the
// comparison does not read the live trees: Bind syncs bound, defaulted and
// zero values back into an Environment, so a bound tree carries leaves its
// sources never set. A reload re-loads without re-binding, and every one of
// those leaves -- the defaulted ones this fixture binds, most of all -- would
// read as a removal. The comparison therefore runs on the trees the sources
// produced, and a bind that changes nothing about the sources changes nothing
// about the diff.
func TestChangedPathsComparesTheLoadedTreeNotTheBoundView(t *testing.T) {
	type boundSection struct {
		Level string `yaml:"level" default:"info"`
		Port  int    `yaml:"port" default:"8080"`
	}

	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "app:\n  a: 1\n")

	before, err := Load(Options{File: base})
	require.NoError(t, err)
	var bound boundSection
	require.NoError(t, before.Bind("xbc", &bound))
	assert.Equal(t, "info", bound.Level, "the bind must have materialized the defaults first")

	after, err := Load(Options{File: base})
	require.NoError(t, err)

	assert.Empty(t, ChangedPaths(before, after),
		"what Bind wrote back into the tree is not a change the sources made")
	assert.True(t, before.Exists("xbc.level"),
		"and the bound leaf really is visible on the bound environment (this is the trap the snapshot avoids)")
}

// A reload after a source change still sees the change, snapshot and all.
func TestChangedPathsStillReportsAChangeUnderABoundSection(t *testing.T) {
	type boundSection struct {
		Level string `yaml:"level" default:"info"`
	}

	dir := t.TempDir()
	base := filepath.Join(dir, "application.yml")
	writeYAML(t, base, "xbc:\n  level: info\n")

	before, err := Load(Options{File: base})
	require.NoError(t, err)
	var bound boundSection
	require.NoError(t, before.Bind("xbc", &bound))

	writeYAML(t, base, "xbc:\n  level: debug\n")
	after, err := Load(Options{File: base})
	require.NoError(t, err)

	assert.Equal(t, []string{"xbc.level"}, ChangedPaths(before, after))
}

func newTestEnvironment(t *testing.T, values map[string]any) *Environment {
	t.Helper()
	env, err := NewEnvironment(values, DefaultEnvPrefix)
	require.NoError(t, err)
	return env
}
