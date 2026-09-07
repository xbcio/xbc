package pprof

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeConfigFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "public without token", cfg: Config{Enabled: true}, want: "requires a token"},
		{name: "short token", cfg: Config{Enabled: true, Token: "short", AllowLoopback: true}, want: "at least 32 bytes"},
		{name: "root path", cfg: Config{Path: "/", AllowLoopback: true}, want: "root path"},
		{name: "dynamic path", cfg: Config{Path: "/debug/:name", AllowLoopback: true}, want: "static absolute"},
		{name: "bad header", cfg: Config{Header: "bad header", AllowLoopback: true}, want: "valid HTTP header"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeConfig(tc.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestNormalizeConfigAcceptsTokenProtectedRemoteAccess(t *testing.T) {
	cfg, err := normalizeConfig(Config{
		Enabled:       true,
		Path:          "/ops/pprof/",
		Header:        "x-profile-token",
		Token:         "0123456789abcdef0123456789abcdef",
		AllowLoopback: false,
	})
	require.NoError(t, err)
	assert.Equal(t, "/ops/pprof", cfg.path)
	assert.Equal(t, "X-Profile-Token", cfg.header)
	assert.True(t, cfg.hasToken)
}
