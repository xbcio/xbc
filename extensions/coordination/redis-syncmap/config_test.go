package redissyncmap

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	xbcconfig "github.com/xbcio/xbc/config"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

func TestConfigDefaults(t *testing.T) {
	config := bindConfig(t, map[string]any{})

	if config.RedisInstance != "default" {
		t.Fatalf("RedisInstance = %q, want default", config.RedisInstance)
	}
	if config.Namespace != "xbc" {
		t.Fatalf("Namespace = %q, want xbc", config.Namespace)
	}
	if config.Persist != "none" {
		t.Fatalf("Persist = %q, want none", config.Persist)
	}
	if config.TTL != 0 || config.MaxSize != 0 {
		t.Fatalf("unexpected defaults: %+v", config)
	}
}

func TestConfigBindsEveryOption(t *testing.T) {
	config := bindConfig(t, map[string]any{
		"redis_instance": "cache",
		"namespace":      "cluster-one",
		"persist":        "ttl",
		"ttl":            "2m",
		"max_size":       256,
	})

	if config.RedisInstance != "cache" || config.Namespace != "cluster-one" {
		t.Fatalf("identity = %+v", config)
	}
	if config.Persist != "ttl" || config.TTL != 2*time.Minute || config.MaxSize != 256 {
		t.Fatalf("behavior = %+v", config)
	}
}

func TestPrepareConfigRejectsInvalidSections(t *testing.T) {
	for name, values := range map[string]map[string]any{
		"ttl mode without a ttl": {"persist": "ttl"},
		"unknown persist mode":   {"persist": "forever"},
		"negative max size":      {"max_size": -1},
		"invalid namespace":      {"namespace": "Has Space"},
		"invalid redis instance": {"redis_instance": "Cache"},
		"negative ttl":           {"persist": "ttl", "ttl": "-1s"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := bindConfigWithoutPrepare(values); err == nil {
				t.Fatalf("config %v was accepted", values)
			}
		})
	}
}

func TestDefaultConfigMatchesTheBoundDefaults(t *testing.T) {
	bound := bindConfig(t, map[string]any{})
	defaults := DefaultConfig()
	if bound.RedisInstance != defaults.RedisInstance ||
		bound.Namespace != defaults.Namespace ||
		bound.Persist != defaults.Persist ||
		bound.TTL != defaults.TTL ||
		bound.MaxSize != defaults.MaxSize {
		t.Fatalf("bound defaults %+v differ from DefaultConfig %+v", bound, defaults)
	}
}

// bindConfig runs one plugins.redis-syncmap.default section through binding,
// tag validation, and the Definition's own Prepare.
func bindConfig(t *testing.T, values map[string]any) Config {
	t.Helper()
	config, err := bindConfigWithoutPrepare(values)
	if err != nil {
		t.Fatalf("bind config: %v", err)
	}
	prepared, err := prepareThroughDefinition(config)
	if err != nil {
		t.Fatalf("prepare config: %v", err)
	}
	return prepared
}

func bindConfigWithoutPrepare(values map[string]any) (Config, error) {
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis-syncmap": map[string]any{
				"default": values,
			},
		},
	}, "XBC_REDIS_SYNCMAP_TEST_")
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := environment.Bind("plugins.redis-syncmap.default", &config); err != nil {
		return Config{}, err
	}
	if err := xbcconfig.Validate(&config, "plugins.redis-syncmap.default"); err != nil {
		return Config{}, err
	}
	if _, err := prepareConfig(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

// prepareThroughDefinition runs Prepare from the Definition rather than calling
// prepareConfig directly, so a ConfigSpec that stopped wiring it fails here.
func prepareThroughDefinition(config Config) (Config, error) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(definition))
	if !ok || descriptor.Config == nil || descriptor.Config.Prepare == nil {
		return Config{}, fmt.Errorf("the definition declares no configuration Prepare")
	}
	prepared, err := descriptor.Config.Prepare(config)
	if err != nil {
		return Config{}, err
	}
	converted, ok := prepared.(Config)
	if !ok {
		return Config{}, fmt.Errorf("Prepare returned %T, want Config", prepared)
	}
	return converted, nil
}

func TestNamespaceAppearsInTheRedisKeys(t *testing.T) {
	server := miniredis.RunT(t)
	first := newTestInstance(t, server, Config{Namespace: "tenant-a", Persist: "none"})
	firstMap := first.mapFor(t, "rules")
	first.start(t)

	concrete := firstMap.(*syncMap)
	if !strings.Contains(concrete.hashKey, "tenant-a") || !strings.Contains(concrete.channel, "tenant-a") {
		t.Fatalf("hash %q and channel %q must both carry the namespace", concrete.hashKey, concrete.channel)
	}
}
