package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeYAML(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func TestLoadFindsExplicitConfigFile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := filepath.Join(dir, "custom.yml")
	writeYAML(t, p, "server:\n  addr: \":9000\"\n")

	k, _, err := loadKoanf(Options{File: p})
	require.NoError(t, err)
	require.Equal(t, ":9000", k.String("server.addr"))
}

func TestLoadFindsApplicationYMLInCurrentDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":9001\"\n")

	k, _, err := loadKoanf(Options{})
	require.NoError(t, err)
	require.Equal(t, ":9001", k.String("server.addr"))
}

func TestLoadFindsConfigsApplicationYML(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "configs", "application.yml"), "server:\n  addr: \":9002\"\n")

	k, _, err := loadKoanf(Options{})
	require.NoError(t, err)
	require.Equal(t, ":9002", k.String("server.addr"))
}

func TestLoadExplicitFileMissingIsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, _, err := loadKoanf(Options{File: filepath.Join(dir, "not-there.yml")})
	require.Error(t, err, "Explicitly specified --config path not existing must error (ruling R8), cannot silently run with empty config")
}

func TestLoadWithNoFileAnywhereIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	k, _, err := loadKoanf(Options{})
	require.NoError(t, err, "It is valid to proceed with an empty configuration when no configuration files are present in three locations (ruling R8)")
	require.False(t, k.Exists("server.addr"))
}

func TestLoadMergesProfileOverlay(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n  base_path: /\n")
	writeYAML(t, filepath.Join(dir, "application-prod.yml"), "server:\n  addr: \":80\"\n")

	k, _, err := loadKoanf(Options{Profile: "prod"})
	require.NoError(t, err)
	require.Equal(t, ":80", k.String("server.addr"), "Same key in profile should override the main file")
	require.Equal(t, "/", k.String("server.base_path"), "Key not mentioned in profile should retain the value from the main file")
}

func TestLoadProfileMissingSiblingIsSilentlySkipped(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")

	k, _, err := loadKoanf(Options{Profile: "does-not-exist"})
	require.NoError(t, err, "Missing profile overlay file is not an error, only the explicit absence of the main file path is an error")
	require.Equal(t, ":8080", k.String("server.addr"))
}

// TestLoadOverridesWinOverFileAndProfile pins the embedder's Overrides above
// both file layers. It is deliberately not a statement about the top of the
// stack: loadKoanf is called with no Universe here, so no environment layer is
// applied, and the environment sits above Overrides when one is present.
func TestLoadOverridesWinOverFileAndProfile(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")
	writeYAML(t, filepath.Join(dir, "application-prod.yml"), "server:\n  addr: \":80\"\n")

	k, _, err := loadKoanf(Options{
		Profile:   "prod",
		Overrides: map[string]any{"server.addr": ":9999"},
	})
	require.NoError(t, err)
	require.Equal(t, ":9999", k.String("server.addr"), "Overrides must win over both the base file and the profile overlay")
}
