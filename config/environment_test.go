// config/environment_test.go
package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type envServerLeaf struct {
	Addr        string        `yaml:"addr" default:":8080"`
	ReadTimeout time.Duration `yaml:"read_timeout" default:"10s"`
}

func TestNewEnvironmentGetExistsSub(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"app": map[string]any{"feature_x": true},
	}, "")
	require.NoError(t, err)

	require.True(t, env.Exists("app.feature_x"))
	require.Equal(t, true, env.Get("app.feature_x"))
	require.Equal(t, map[string]any{"feature_x": true}, env.Sub("app"))
}

func TestEnvironmentGetReturnsRecursiveDefensiveCopy(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"app": map[string]any{
			"nested": map[string]any{"labels": []string{"original"}},
			"items":  []any{map[string]any{"enabled": true}},
		},
	}, "")
	require.NoError(t, err)

	first, ok := env.Get("app").(map[string]any)
	require.True(t, ok)
	first["added"] = "caller-only"
	nested := first["nested"].(map[string]any)
	nested["labels"].([]string)[0] = "mutated"
	first["items"].([]any)[0].(map[string]any)["enabled"] = false

	again := env.Get("app").(map[string]any)
	assert.NotContains(t, again, "added")
	assert.Equal(t, []string{"original"}, again["nested"].(map[string]any)["labels"])
	assert.Equal(t, true, again["items"].([]any)[0].(map[string]any)["enabled"])
}

func TestEnvironmentSubReturnsRecursiveDefensiveCopy(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"demo": map[string]any{
				"hosts":  []string{"a", "b"},
				"nested": map[string]any{"limit": 3},
			},
		},
	}, "")
	require.NoError(t, err)

	first := env.Sub("plugins.demo")
	first["hosts"].([]string)[0] = "changed"
	first["nested"].(map[string]any)["limit"] = 99
	first["extra"] = true

	again := env.Sub("plugins.demo")
	assert.Equal(t, []string{"a", "b"}, again["hosts"])
	assert.Equal(t, 3, again["nested"].(map[string]any)["limit"])
	assert.NotContains(t, again, "extra")
}

func TestCloneCollectionsPreservesConcreteCollectionTypes(t *testing.T) {
	type labels map[string][]int
	original := labels{"ids": {1, 2}}

	cloned, ok := cloneCollections(original).(labels)
	require.True(t, ok)
	cloned["ids"][0] = 99

	assert.Equal(t, labels{"ids": {1, 2}}, original)
}

func TestNewEnvironmentSubOnAbsentPathReturnsNil(t *testing.T) {
	env, err := NewEnvironment(nil, "")
	require.NoError(t, err)
	require.Nil(t, env.Sub("does.not.exist"))
}

func TestNewEnvironmentSubOnScalarPathReturnsNil(t *testing.T) {
	env, err := NewEnvironment(map[string]any{"server": map[string]any{"addr": ":8080"}}, "")
	require.NoError(t, err)
	require.Nil(t, env.Sub("server.addr"), "标量路径不是 map，Sub 应返回 nil 而不是 panic")
}

func TestNewEnvironmentGetOnAbsentPathReturnsNil(t *testing.T) {
	env, err := NewEnvironment(nil, "")
	require.NoError(t, err)
	require.Nil(t, env.Get("does.not.exist"))
}

func TestNilEnvironmentIsDefensive(t *testing.T) {
	var env *Environment
	require.Nil(t, env.Get("x"))
	require.False(t, env.Exists("x"))
	require.Nil(t, env.Sub("x"))
}

// TestEnvironmentBindSyncsDefaultsBackIntoEnvironment pins syncBack: after
// Bind fills in a `default` tag because neither the file nor ENV set that
// leaf, Get must be able to read the very same value back out of the
// environment, not just off the out struct Bind wrote into directly. If
// syncBack were ever removed, this assertion goes red.
func TestEnvironmentBindSyncsDefaultsBackIntoEnvironment(t *testing.T) {
	env, err := NewEnvironment(nil, "")
	require.NoError(t, err)

	var sc envServerLeaf
	require.NoError(t, env.Bind("server", &sc))

	require.Equal(t, ":8080", sc.Addr)
	require.Equal(t, 10*time.Second, sc.ReadTimeout)

	require.True(t, env.Exists("server.addr"), "default tag 填充的值也要能被 Exists 看见")
	require.Equal(t, ":8080", env.Get("server.addr"))
	require.True(t, env.Exists("server.read_timeout"))
	require.Equal(t, 10*time.Second, env.Get("server.read_timeout"))
}

func TestEnvironmentBindAppliesEnvOverride(t *testing.T) {
	t.Setenv("XBC_SERVER_ADDR", ":9999")

	env, err := NewEnvironment(nil, "")
	require.NoError(t, err)

	var sc envServerLeaf
	require.NoError(t, env.Bind("server", &sc))

	require.Equal(t, ":9999", sc.Addr, "ENV 覆盖应当生效")
	require.Equal(t, ":9999", env.Get("server.addr"), "ENV 覆盖同样要回写进 environment")
}

func TestEnvironmentBindOnUninitializedEnvironmentIsError(t *testing.T) {
	var env *Environment
	var sc envServerLeaf
	err := env.Bind("server", &sc)
	require.Error(t, err, "Environment 还没被 Load/NewEnvironment 初始化时调用 Bind 必须报错，不能拿着 nil 的 koanf 去 panic")
}

func TestLoadWithoutFileUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	env, err := Load(Options{})
	require.NoError(t, err)
	require.NotNil(t, env)
	require.False(t, env.Exists("server.addr"), "没有 Bind 之前，freeform 路径不应该被合成出来")
}

func TestLoadDefaultsEnvPrefixWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XBC_SERVER_ADDR", ":7777")

	env, err := Load(Options{})
	require.NoError(t, err)

	var sc envServerLeaf
	require.NoError(t, env.Bind("server", &sc), "EnvPrefix 为空时应默认落到 XBC_")
	require.Equal(t, ":7777", sc.Addr)
}

func TestNewEnvironmentDefensivelyCopiesInputCollections(t *testing.T) {
	values := map[string]any{
		"app": map[string]any{
			"labels": []string{"original"},
		},
	}
	env, err := NewEnvironment(values, "")
	require.NoError(t, err)

	values["app"].(map[string]any)["labels"].([]string)[0] = "mutated"
	values["app"].(map[string]any)["extra"] = true

	app := env.Sub("app")
	assert.Equal(t, []string{"original"}, app["labels"])
	assert.NotContains(t, app, "extra")
}
