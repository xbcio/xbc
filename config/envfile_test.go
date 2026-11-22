package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeEnvFile writes a dotenv file next to the configuration under test and
// returns its path.
func writeEnvFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestEnvFileFeedsEnvironmentLayer(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")
	envPath := writeEnvFile(t, "XBC_SERVER_ADDR=:7000\nXBC_SERVER_BASE_PATH=/from-dotenv\n")

	env, err := Load(Options{Universe: testUniverse(t), EnvFile: envPath})
	require.NoError(t, err)
	assert.Equal(t, ":7000", env.Get("server.addr"), "a dotenv entry must override the configuration file, like an exported variable")
	assert.Equal(t, "/from-dotenv", env.Get("server.base_path"))
	assert.Contains(t, env.Files(), envPath, "a dotenv file is a source a reload must watch")
}

func TestEnvFileFeedsBind(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	envPath := writeEnvFile(t, "XBC_SERVER_ADDR=:7100\nXBC_SERVER_POOL_MAX_IDLE=7\n")

	env, err := Load(Options{Universe: testUniverse(t), EnvFile: envPath})
	require.NoError(t, err)

	var server serverSection
	require.NoError(t, env.Bind("server", &server))
	assert.Equal(t, ":7100", server.Addr)
	assert.Equal(t, 7, server.Pool.MaxIdle)
}

func TestEnvFileLosesToProcessEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XBC_SERVER_ADDR", ":9999")
	envPath := writeEnvFile(t, "XBC_SERVER_ADDR=:7000\n")

	env, err := Load(Options{Universe: testUniverse(t), EnvFile: envPath})
	require.NoError(t, err)
	assert.Equal(t, ":9999", env.Get("server.addr"), "an inherited variable must win over the dotenv file")

	var server serverSection
	require.NoError(t, env.Bind("server", &server))
	assert.Equal(t, ":9999", server.Addr, "Bind must apply the same precedence the merged environment promises")
}

func TestEnvFileDoesNotMutateProcessEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	envPath := writeEnvFile(t, "XBC_SERVER_BASE_PATH=/dotenv-only\n")

	env, err := Load(Options{Universe: testUniverse(t), EnvFile: envPath})
	require.NoError(t, err)
	assert.Equal(t, "/dotenv-only", env.Get("server.base_path"))

	_, exists := os.LookupEnv("XBC_SERVER_BASE_PATH")
	assert.False(t, exists, "the dotenv file must be parsed into the Environment, never loaded into the process environment")
}

func TestEnvFileMissingIsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, err := Load(Options{Universe: testUniverse(t), EnvFile: filepath.Join(dir, "missing.env")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist", "a named dotenv file is an explicit promise, like --config")
}

func TestEnvFileRequiresUniverse(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	envPath := writeEnvFile(t, "XBC_SERVER_ADDR=:7000\n")

	_, err := Load(Options{EnvFile: envPath})
	require.Error(t, err, "without a Universe there is no schema to resolve environment names against")
	assert.Contains(t, err.Error(), "requires Options.Universe")
}

func TestEnvFileNamesOnlyDeclaredLeaves(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	envPath := writeEnvFile(t, "XBC_SERVER_NO_SUCH_FIELD=x\n")

	_, err := Load(Options{Universe: testUniverse(t), EnvFile: envPath})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "XBC_SERVER_NO_SUCH_FIELD")
}

func TestEnvFileIgnoresNamesOutsideThePrefix(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	envPath := writeEnvFile(t, "UNRELATED_SECRET=ignored\nXBC_SERVER_ADDR=:7200\n")

	env, err := Load(Options{Universe: testUniverse(t), EnvFile: envPath})
	require.NoError(t, err, "a name outside the prefix is not a configuration variable and must not fail the load")
	assert.Equal(t, ":7200", env.Get("server.addr"))
}
