package log

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	lumberjack "gopkg.in/natefinch/lumberjack.v2"
)

// nowFunc 是全包共用的时间钩子，只在测试里替换。
var nowFunc = time.Now

// dailyRotator 给 lumberjack 补上按日滚动。
//
// lumberjack 本身只按文件大小滚，但它的 Rotate() 是导出的 —— 所以这里
// 只做一件事：写之前检查是否跨天，跨了就触发一次 Rotate()。
// 清理、压缩、backup 数量全部由 lumberjack 按原有配置处理。
//
// 已知行为：跨天滚出的归档文件名带的是触发时刻的时间戳
// （app-2026-08-25T00-00-03.000.log），而内容是前一天的。
// 这是 lumberjack 的既定命名规则，max_age 也按这个时间戳算。
type dailyRotator struct {
	lj *lumberjack.Logger

	mu  sync.Mutex
	day string
}

func newDailyRotator(lj *lumberjack.Logger) *dailyRotator {
	// [SEC-INFO] 日志目录必须是 0750。lumberjack 自己建目录时硬编码 0755
	// （实测 -rwxr-xr-x），违反安全规范，所以抢先建好：os.MkdirAll 对已存在
	// 的目录直接返回 nil、不改权限，之后 lumberjack 那次调用就成了 no-op。
	// 这里忽略错误 —— 真建不出来，lumberjack 首次写盘会报出真正的原因。
	_ = os.MkdirAll(filepath.Dir(lj.Filename), 0o750)
	return &dailyRotator{lj: lj, day: nowFunc().Format(time.DateOnly)}
}

func (d *dailyRotator) Write(p []byte) (int, error) {
	d.mu.Lock()
	if today := nowFunc().Format(time.DateOnly); today != d.day {
		d.day = today
		// 空文件不滚：Rotate() 对 0 字节的当前文件照样产出一个 0 字节归档
		// （实测），进程在新的一天首启时就会滚出这种垃圾，长期累积。
		if fi, err := os.Stat(d.lj.Filename); err != nil || fi.Size() > 0 {
			// 滚动失败不能阻塞写入：日志滚不动是运维问题，日志丢了是事故。
			_ = d.lj.Rotate()
		}
	}
	d.mu.Unlock()

	// lumberjack.Write 自带锁，放在 d.mu 之外，别把锁粒度放大到整个写盘。
	return d.lj.Write(p)
}

// Sync 满足 zapcore.WriteSyncer。lumberjack 直写 fd 不缓冲，无事可做。
//
// 注意 lumberjack.Logger **没有** Sync 方法（方法集只有 Close/Rotate/Write），
// 别去转发一个不存在的方法。
func (d *dailyRotator) Sync() error { return nil }

func (d *dailyRotator) Close() error { return d.lj.Close() }
