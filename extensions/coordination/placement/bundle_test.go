package placement

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
)

// internalAppConfig writes the quiet framework section the application in this
// file needs.
func internalAppConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\n" +
		"xbc:\n  shutdown_timeout: 5s\n  pre_stop_timeout: 1s\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// TestTheBundleRefusesToRunWithoutAPlacement drives the loud failure a
// composition gets when the Bundle reaches the graph without New, the way an
// autoload-only process that never built a Placement would.
//
// It lives in the internal test package because the case it covers is precisely
// "nothing installed this process's placement", and only a same-package test
// can put the process back into that state. It runs a real application, because
// the failure has to be observable where an operator would meet it: at startup,
// not at a unit boundary.
func TestTheBundleRefusesToRunWithoutAPlacement(t *testing.T) {
	server, client := newMiniredis(t)
	_ = server
	if _, err := New(newRedisLocker(t, client)); err != nil {
		t.Fatalf("placement.New: %v", err)
	}
	previous := installedPlacement.Swap(nil)
	t.Cleanup(func() { installedPlacement.Store(previous) })

	app, err := xbc.New(xbc.WithBundles(Bundle()))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), []string{"--config", internalAppConfig(t)})

	require.Error(t, err)
	assert.Equal(t, 1, code)
	assert.Contains(t, err.Error(), "no Placement was installed")
	assert.Contains(t, err.Error(), "placement.New")
}
