package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type strictTLSConfig struct {
	Enabled bool `yaml:"enabled"`
}

type strictServerConfig struct {
	Addr string          `yaml:"addr"`
	TLS  strictTLSConfig `yaml:"tls"`
}

func TestEnvironmentBindRejectsUnknownPluginFields(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"web": map[string]any{
				"adrr": ":9090",
				"tls":  map[string]any{"enabeld": true},
			},
		},
	}, "")
	require.NoError(t, err)

	var config strictServerConfig
	err = env.Bind("plugins.web", &config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.web.adrr")
	assert.Contains(t, err.Error(), "plugins.web.tls.enabeld")
	assert.NotContains(t, err.Error(), "plugins.web.addr\n", "合法字段不能被误报")
}

type strictFrameworkConfig struct {
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

func TestEnvironmentBindRejectsUnknownFrameworkFieldFromYAML(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "application.yml")
	writeYAML(t, file, "xbc:\n  shutdown_timout: 5s\n")

	env, err := Load(Options{File: file})
	require.NoError(t, err)
	var config strictFrameworkConfig
	err = env.Bind("xbc", &config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc.shutdown_timout")
}

func TestEnvironmentBindWithOptionsAllowsFrameworkOwnedKey(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"web": map[string]any{
				"enabled": false,
				"addr":    ":9090",
			},
		},
	}, "")
	require.NoError(t, err)

	var strict strictServerConfig
	err = env.Bind("plugins.web", &strict)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.web.enabled")

	var allowed strictServerConfig
	err = env.BindWithOptions("plugins.web", &allowed, BindOptions{
		AllowedKeys: []string{"enabled"},
	})
	require.NoError(t, err)
	assert.Equal(t, ":9090", allowed.Addr)
	assert.Equal(t, false, env.Get("plugins.web.enabled"), "allowlist 不应删除或改写框架字段")
}

func TestEnvironmentBindAllowlistDoesNotHideOtherTypos(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"web": map[string]any{
				"enabled": false,
				"adrr":    ":9090",
			},
		},
	}, "")
	require.NoError(t, err)

	var config strictServerConfig
	err = env.BindWithOptions("plugins.web", &config, BindOptions{
		AllowedKeys: []string{"enabled"},
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "plugins.web.enabled")
	assert.Contains(t, err.Error(), "plugins.web.adrr")
}

type pointerPoolConfig struct {
	Addr  string `yaml:"addr"`
	Limit int    `yaml:"limit" default:"10" validate:"min=1"`
}

type pointerRootConfig struct {
	Pool *pointerPoolConfig `yaml:"pool"`
}

func TestBindPointerSubtreeFromFileAndAppliesNestedDefault(t *testing.T) {
	file := filepath.Join(t.TempDir(), "application.yml")
	writeYAML(t, file, "service:\n  pool:\n    addr: db:5432\n")
	env, err := Load(Options{File: file})
	require.NoError(t, err)

	var config pointerRootConfig
	require.NoError(t, env.Bind("service", &config))
	require.NotNil(t, config.Pool)
	assert.Equal(t, "db:5432", config.Pool.Addr)
	assert.Equal(t, 10, config.Pool.Limit)
	assert.Equal(t, 10, env.Get("service.pool.limit"))
}

func TestBindPointerSubtreeAllocatesOnlyWhenDefaultHits(t *testing.T) {
	type noDefaults struct {
		Name string `yaml:"name"`
	}
	type optionalRoot struct {
		Optional *noDefaults `yaml:"optional"`
	}

	empty, err := NewEnvironment(nil, "")
	require.NoError(t, err)
	var untouched optionalRoot
	require.NoError(t, empty.Bind("service", &untouched))
	assert.Nil(t, untouched.Optional, "无文件、ENV 或 default 命中时不能物化 nil 子树")
	assert.False(t, empty.Exists("service.optional.name"), "syncBack 也不能为 nil 子树合成零值路径")

	var withDefault pointerRootConfig
	require.NoError(t, empty.Bind("database", &withDefault))
	require.NotNil(t, withDefault.Pool, "子字段 default 命中时应按需分配父指针")
	assert.Equal(t, 10, withDefault.Pool.Limit)
}

func TestBindPointerSubtreeAllocatesWhenEnvHits(t *testing.T) {
	env, err := NewEnvironment(nil, "XBC_TEST_")
	require.NoError(t, err)
	t.Setenv("XBC_TEST_SERVICE_POOL_ADDR", "env-db:5432")

	var config pointerRootConfig
	require.NoError(t, env.Bind("service", &config))
	require.NotNil(t, config.Pool)
	assert.Equal(t, "env-db:5432", config.Pool.Addr)
	assert.Equal(t, 10, config.Pool.Limit)
}

func TestValidateUsesPointerSubtreeYAMLPath(t *testing.T) {
	config := pointerRootConfig{Pool: &pointerPoolConfig{Limit: 0}}
	err := Validate(&config, "plugins.database")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plugins.database.pool.limit")
}

type InlineNetworkConfig struct {
	Addr  string `yaml:"addr" default:":8080"`
	Token string `yaml:"token" validate:"required"`
}

type inlineServerConfig struct {
	InlineNetworkConfig `yaml:",inline"`
	Mode                string `yaml:"mode" default:"prod"`
}

func TestInlineAnonymousStructUsesFlatSchemaForFileDefaultAndEnv(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"server": map[string]any{"token": "file-token"},
	}, "XBC_TEST_")
	require.NoError(t, err)
	t.Setenv("XBC_TEST_SERVER_ADDR", ":9090")

	var config inlineServerConfig
	require.NoError(t, env.Bind("server", &config))
	assert.Equal(t, "file-token", config.Token)
	assert.Equal(t, ":9090", config.Addr)
	assert.Equal(t, "prod", config.Mode)
	assert.Equal(t, ":9090", env.Get("server.addr"))
	assert.False(t, env.Exists("server.InlineNetworkConfig.addr"))
}

func TestInlineAnonymousStructUsesFlatSchemaForStrictnessAndValidation(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"server": map[string]any{"addrr": ":9090"},
	}, "")
	require.NoError(t, err)

	var config inlineServerConfig
	err = env.Bind("server", &config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.addrr")
	assert.NotContains(t, err.Error(), "InlineNetworkConfig")

	validationErr := Validate(&inlineServerConfig{}, "server")
	require.Error(t, validationErr)
	assert.Contains(t, validationErr.Error(), "server.token")
	assert.NotContains(t, validationErr.Error(), "InlineNetworkConfig")
}

func TestBindRejectsInvalidOutputWithoutPanicking(t *testing.T) {
	env, err := NewEnvironment(nil, "")
	require.NoError(t, err)

	var nilConfig *strictServerConfig
	valueConfig := strictServerConfig{}
	integer := 1
	pointerToPointer := &nilConfig

	tests := []struct {
		name string
		out  any
	}{
		{name: "nil interface", out: nil},
		{name: "struct value", out: valueConfig},
		{name: "nil struct pointer", out: nilConfig},
		{name: "non-struct pointer", out: &integer},
		{name: "pointer to pointer", out: pointerToPointer},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				err := env.Bind("server", test.out)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "非 nil 的 struct 指针")
			})
		})
	}
}

func TestValidateRejectsInvalidOutputWithoutPanicking(t *testing.T) {
	var nilConfig *strictServerConfig
	require.NotPanics(t, func() {
		err := Validate(nilConfig, "server")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "非 nil 的 struct 指针")
	})
}

func TestStrictSchemaAllowsDynamicMapKeys(t *testing.T) {
	type dynamicConfig struct {
		Labels map[string]string `yaml:"labels"`
	}
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"labels": map[string]any{"region": "cn", "owner": "runtime"},
		},
	}, "")
	require.NoError(t, err)

	var config dynamicConfig
	require.NoError(t, env.Bind("service", &config))
	assert.Equal(t, map[string]string{"region": "cn", "owner": "runtime"}, config.Labels)
}

func TestStrictSchemaChecksStructValuesInsideDynamicMap(t *testing.T) {
	type endpoint struct {
		Addr string `yaml:"addr"`
	}
	type endpointsConfig struct {
		Endpoints map[string]endpoint `yaml:"endpoints"`
	}
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"endpoints": map[string]any{
				"primary": map[string]any{"adrr": "db:5432"},
			},
		},
	}, "")
	require.NoError(t, err)

	var config endpointsConfig
	err = env.Bind("service", &config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service.endpoints.primary.adrr")
}

type InlinePointerConfig struct {
	Value string `yaml:"value"`
}

type inlinePointerRoot struct {
	*InlinePointerConfig `yaml:",inline"`
	Mode                 string `yaml:"mode"`
}

func TestInlineAnonymousPointerAllocatesOnlyWhenInputMatches(t *testing.T) {
	empty, err := NewEnvironment(nil, "")
	require.NoError(t, err)
	var absent inlinePointerRoot
	require.NoError(t, empty.Bind("service", &absent))
	assert.Nil(t, absent.InlinePointerConfig)

	siblingOnly, err := NewEnvironment(map[string]any{
		"service": map[string]any{"mode": "prod"},
	}, "")
	require.NoError(t, err)
	var sibling inlinePointerRoot
	require.NoError(t, siblingOnly.Bind("service", &sibling))
	assert.Equal(t, "prod", sibling.Mode)
	assert.Nil(t, sibling.InlinePointerConfig, "同级字段不应永久物化未命中的 inline 指针")

	configured, err := NewEnvironment(map[string]any{
		"service": map[string]any{"value": "from-file"},
	}, "")
	require.NoError(t, err)
	var present inlinePointerRoot
	require.NoError(t, configured.Bind("service", &present))
	require.NotNil(t, present.InlinePointerConfig)
	assert.Equal(t, "from-file", present.Value)
}

func TestEnvironmentBindWithOptionsSupportsNestedAllowedPaths(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"runtime": map[string]any{
				"enabled": true,
				"typo":    true,
			},
		},
	}, "")
	require.NoError(t, err)

	var config struct{}
	err = env.BindWithOptions("service", &config, BindOptions{
		AllowedKeys: []string{"runtime.enabled"},
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "service.runtime.enabled")
	assert.Contains(t, err.Error(), "service.runtime.typo")
}

func TestPointerSubtreeValidationUsesSameInlineSchema(t *testing.T) {
	type nestedInline struct {
		InlineNetworkConfig `yaml:",inline"`
	}
	type root struct {
		Nested *nestedInline `yaml:"nested"`
	}
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"nested": map[string]any{"addr": ":9090"},
		},
	}, "")
	require.NoError(t, err)

	var config root
	require.NoError(t, env.Bind("service", &config))
	require.NotNil(t, config.Nested)
	assert.Equal(t, ":9090", config.Nested.Addr)

	validationErr := Validate(&config, "service")
	require.Error(t, validationErr)
	assert.Contains(t, validationErr.Error(), "service.nested.token")
	assert.NotContains(t, validationErr.Error(), "InlineNetworkConfig")
}

type strictCollectionItem struct {
	Name string `yaml:"name"`
}

type strictNestedCollectionConfig struct {
	Groups  [][]strictCollectionItem           `yaml:"groups"`
	Buckets map[string][]strictCollectionItem  `yaml:"buckets"`
	Arrays  [1]map[string]strictCollectionItem `yaml:"arrays"`
}

func TestStrictSchemaChecksStructsThroughNestedCollections(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"groups": []any{[]any{map[string]any{"naem": "nested"}}},
			"buckets": map[string]any{
				"primary": []any{map[string]any{"naem": "mapped"}},
			},
			"arrays": []any{map[string]any{
				"primary": map[string]any{"naem": "array-map"},
			}},
		},
	}, "")
	require.NoError(t, err)

	var config strictNestedCollectionConfig
	err = env.Bind("service", &config)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service.groups[0][0].naem")
	assert.Contains(t, err.Error(), "service.buckets.primary[0].naem")
	assert.Contains(t, err.Error(), "service.arrays[0].primary.naem")
}

func TestAllowedKeyCannotDisableStrictnessOfSchemaField(t *testing.T) {
	env, err := NewEnvironment(map[string]any{
		"service": map[string]any{
			"tls": map[string]any{"enabeld": true},
		},
	}, "")
	require.NoError(t, err)

	var config strictServerConfig
	err = env.BindWithOptions("service", &config, BindOptions{
		AllowedKeys: []string{"tls"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "service.tls.enabeld")
}
