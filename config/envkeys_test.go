package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dottedEnvKeys maps every variable that contains a dot to itself, the way a
// caller whose variable names are already configuration paths would.
func dottedEnvKeys(name string) (string, bool) {
	return name, strings.Contains(name, ".")
}

func TestEnvKeysBuildsSchemalessEnvironmentLayer(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "mysql:\n  default:\n    dns: from-file\n")
	t.Setenv("mysql.default.dns", "from-env")
	t.Setenv("PLAIN_VARIABLE", "ignored")

	env, err := Load(Options{EnvKeys: dottedEnvKeys})
	require.NoError(t, err)
	assert.Equal(t, "from-env", env.Get("mysql.default.dns"), "the environment layer must override the file")
	assert.False(t, env.Exists("PLAIN_VARIABLE"), "a variable the function rejects must not enter the tree")
}

func TestEnvKeysFeedsFromEnvFileBelowProcessEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	envPath := writeEnvFile(t, "redis.default.password=from-dotenv\nredis.default.addr=from-dotenv\n")
	t.Setenv("redis.default.password", "from-process")

	env, err := Load(Options{EnvKeys: dottedEnvKeys, EnvFile: envPath})
	require.NoError(t, err)
	assert.Equal(t, "from-process", env.Get("redis.default.password"))
	assert.Equal(t, "from-dotenv", env.Get("redis.default.addr"))
}

func TestEnvFileStillRequiresAPathRule(t *testing.T) {
	envPath := writeEnvFile(t, "A=1\n")
	_, err := Load(Options{EnvFile: envPath})
	require.Error(t, err)
}
