package log

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// setNow 替换全包的时间钩子，返回还原函数。Task 8 的 Span 测试也用它。
func setNow(f func() time.Time) (restore func()) {
	old := nowFunc
	nowFunc = f
	return func() { nowFunc = old }
}

func TestDailyRotatorTriggersOnDayChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 23, 59, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 3, LocalTime: true})

	_, err := d.Write([]byte("day1\n"))
	require.NoError(t, err)

	fake = fake.Add(2 * time.Minute) // 跨天
	_, err = d.Write([]byte("day2\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "跨天后应有一个归档文件加一个当前文件")

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "day2\n", string(cur), "当前文件只含跨天之后的内容")
}

func TestDailyRotatorDoesNotRotateWithinSameDay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 8, 0, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, LocalTime: true})

	_, err := d.Write([]byte("a\n"))
	require.NoError(t, err)
	fake = fake.Add(13 * time.Hour) // 同一天内跨了大半天
	_, err = d.Write([]byte("b\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "同一天内不滚")

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "a\nb\n", string(cur))
}

func TestDailyRotatorConcurrentWritesRotateOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fake := time.Date(2026, 8, 24, 23, 59, 59, 0, time.Local)
	var mu sync.Mutex
	defer setNow(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fake
	})()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 10, LocalTime: true})
	_, err := d.Write([]byte("seed\n"))
	require.NoError(t, err)

	mu.Lock()
	fake = fake.Add(time.Second) // 全部 goroutine 同时看到跨天
	mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Write([]byte("x\n"))
		}()
	}
	wg.Wait()
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "32 个并发写只该触发一次滚动")
}

func TestDailyRotatorSyncIsNoop(t *testing.T) {
	d := newDailyRotator(&lumberjack.Logger{Filename: filepath.Join(t.TempDir(), "a.log")})
	assert.NoError(t, d.Sync(), "lumberjack 不缓冲，Sync 无事可做但必须存在以满足 WriteSyncer")
	assert.NoError(t, d.Close())
}

func TestDailyRotatorSkipsRotateOnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, nil, 0o600)) // 0 字节的当前文件

	fake := time.Date(2026, 8, 24, 23, 59, 0, 0, time.Local)
	defer setNow(func() time.Time { return fake })()

	d := newDailyRotator(&lumberjack.Logger{Filename: path, MaxBackups: 3, LocalTime: true})
	fake = fake.Add(2 * time.Minute) // 跨天
	_, err := d.Write([]byte("day2\n"))
	require.NoError(t, err)
	require.NoError(t, d.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "空文件跨天不该滚出 0 字节归档")
}

func TestDailyRotatorCreatesDirWith0750(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	d := newDailyRotator(&lumberjack.Logger{Filename: filepath.Join(dir, "app.log")})
	defer d.Close()

	fi, err := os.Stat(dir)
	require.NoError(t, err)
	// [SEC-INFO] lumberjack 自己建目录是 0755，必须由我们抢先建成 0750
	assert.Equal(t, os.FileMode(0o750), fi.Mode().Perm(), "日志目录权限")
}
