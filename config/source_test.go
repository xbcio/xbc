// config/source_test.go
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

	k, err := loadKoanf(Options{File: p})
	require.NoError(t, err)
	require.Equal(t, ":9000", k.String("server.addr"))
}

func TestLoadFindsApplicationYMLInCurrentDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":9001\"\n")

	k, err := loadKoanf(Options{})
	require.NoError(t, err)
	require.Equal(t, ":9001", k.String("server.addr"))
}

func TestLoadFindsConfigsApplicationYML(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "configs", "application.yml"), "server:\n  addr: \":9002\"\n")

	k, err := loadKoanf(Options{})
	require.NoError(t, err)
	require.Equal(t, ":9002", k.String("server.addr"))
}

func TestLoadExplicitFileMissingIsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	_, err := loadKoanf(Options{File: filepath.Join(dir, "not-there.yml")})
	require.Error(t, err, "显式指定的 --config 路径不存在必须报错（裁决 R8），不能静默按空配置跑")
}

func TestLoadWithNoFileAnywhereIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	k, err := loadKoanf(Options{})
	require.NoError(t, err, "三个位置都没有配置文件时，按空配置继续是合法的（裁决 R8）")
	require.False(t, k.Exists("server.addr"))
}

func TestLoadMergesProfileOverlay(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n  base_path: /\n")
	writeYAML(t, filepath.Join(dir, "application-prod.yml"), "server:\n  addr: \":80\"\n")

	k, err := loadKoanf(Options{Profile: "prod"})
	require.NoError(t, err)
	require.Equal(t, ":80", k.String("server.addr"), "profile 里同 key 应当覆盖主文件")
	require.Equal(t, "/", k.String("server.base_path"), "profile 没提到的 key 应当保留主文件的值")
}

func TestLoadProfileMissingSiblingIsSilentlySkipped(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")

	k, err := loadKoanf(Options{Profile: "does-not-exist"})
	require.NoError(t, err, "profile 叠加文件缺失不是错误，只有主文件的显式路径缺失才是错误")
	require.Equal(t, ":8080", k.String("server.addr"))
}

func TestLoadOverridesWinOverEverything(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeYAML(t, filepath.Join(dir, "application.yml"), "server:\n  addr: \":8080\"\n")
	writeYAML(t, filepath.Join(dir, "application-prod.yml"), "server:\n  addr: \":80\"\n")

	k, err := loadKoanf(Options{
		Profile:   "prod",
		Overrides: map[string]any{"server.addr": ":9999"},
	})
	require.NoError(t, err)
	require.Equal(t, ":9999", k.String("server.addr"), "flag 覆盖必须是最高优先级")
}
