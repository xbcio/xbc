package pprof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeConfigPropagatesCustomPath(t *testing.T) {
	cfg, err := normalizeConfig(Config{Enabled: true, Path: "/ops/pprof"})
	require.NoError(t, err)
	assert.Equal(t, "/ops/pprof", cfg.path, "plugins.pprof.path must reach the registered route")
}

func TestNormalizeConfigFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "root path", cfg: Config{Path: "/"}, want: "root path"},
		{name: "dynamic path", cfg: Config{Path: "/debug/:name"}, want: "static absolute"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeConfig(tc.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
