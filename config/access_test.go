package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilesMergeInOrderAndSkipMissing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "a", "application.yml"), "x:\n  one: a\n  two: a\n")
	writeYAML(t, filepath.Join(dir, "b", "application.yml"), "x:\n  two: b\n")

	env, err := Load(Options{Files: []string{
		filepath.Join(dir, "a", "application.yml"),
		filepath.Join(dir, "missing.yml"),
		filepath.Join(dir, "b", "application.yml"),
	}})
	require.NoError(t, err)
	assert.Equal(t, "a", env.String("x.one"))
	assert.Equal(t, "b", env.String("x.two"), "a later file overrides an earlier one")
	assert.Len(t, env.Files(), 2)
}

func TestTypedGettersAndAll(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"),
		"f:\n  n: 5\n  on: true\n  r: 0.25\n  s: [a, b]\n  name: v\n")
	env, err := Load(Options{})
	require.NoError(t, err)
	assert.Equal(t, 5, env.Int("f.n"))
	assert.True(t, env.Bool("f.on"))
	assert.InDelta(t, 0.25, env.Float64("f.r"), 1e-9)
	assert.Equal(t, []string{"a", "b"}, env.Strings("f.s"))
	assert.Equal(t, "v", env.String("f.name"))
	assert.Equal(t, "", env.String("f.absent"))
	assert.Empty(t, env.Strings("f.absent"))
	assert.Equal(t, "v", env.All()["f.name"])
}

func TestDecodeIsLenient(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"),
		"db:\n  max_conns: \"7\"\n  unknown: 1\n")
	env, err := Load(Options{})
	require.NoError(t, err)

	var out struct{ MaxConns int }
	require.NoError(t, env.Decode("db", &out, DecodeOptions{
		WeaklyTyped: true,
		MatchName: func(k, f string) bool {
			return strings.EqualFold(strings.ReplaceAll(k, "_", ""), f)
		},
	}))
	assert.Equal(t, 7, out.MaxConns)
}

func TestNewMatchesLoad(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "a.yml"), "k:\n  v: 1\n")
	envPath := writeEnvFile(t, "k.w=2\n")

	env, err := New(
		WithFiles(filepath.Join(dir, "a.yml")),
		WithEnvFile(envPath),
		WithEnvKeys(dottedEnvKeys),
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, env.Int("k.v"))
	assert.Equal(t, 2, env.Int("k.w"))
}
