package log

import (
	"fmt"
	"path/filepath"
	"strings"
)

// 渲染格式。
const (
	FormatConsole = "console" // 对齐 + 着色，给人看
	FormatJSON    = "json"    // 每行一个 JSON 对象，给机器检索
)

// console 着色模式。
const (
	ColorAuto   = "auto"
	ColorAlways = "always"
	ColorNever  = "never"
)

// 文件滚动策略。
const (
	RotateDaily = "daily" // 跨天滚一次（本框架实现，lumberjack 本身只按大小滚）
	RotateSize  = "size"  // 只按 max_size 滚
)

// Config 是日志配置。
//
// 格式是 sink 级而非全局的 —— "终端 console + 文件 json" 是最常见的组合，
// 全局单一 format 表达不了。
type Config struct {
	Level      string `yaml:"level"      json:"level"`
	Caller     bool   `yaml:"caller"     json:"caller"`
	Stacktrace string `yaml:"stacktrace" json:"stacktrace"`

	Console ConsoleConfig `yaml:"console" json:"console"`
	File    FileConfig    `yaml:"file"    json:"file"`

	Sampling SamplingConfig `yaml:"sampling" json:"sampling"`

	// MaskFields 追加到内置脱敏黑名单。只能加，不能减 —— 内置项不可移除。
	MaskFields []string `yaml:"mask_fields" json:"mask_fields"`
}

type ConsoleConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Format  string `yaml:"format"  json:"format"`
	Color   string `yaml:"color"   json:"color"`
}

type FileConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Path    string `yaml:"path"    json:"path"`

	// Format 留空则按 Path 后缀推导：.jsonl/.json/.ndjson → json，其余 → console。
	Format string `yaml:"format" json:"format"`

	Rotate     string `yaml:"rotate"      json:"rotate"`
	MaxSize    int    `yaml:"max_size"    json:"max_size"`    // MB
	MaxAge     int    `yaml:"max_age"     json:"max_age"`     // 天
	MaxBackups int    `yaml:"max_backups" json:"max_backups"` // 个
	Compress   bool   `yaml:"compress"    json:"compress"`

	// ErrorPath 非空时额外开一个只收 error 及以上的 sink。
	ErrorPath string `yaml:"error_path" json:"error_path"`

	// errorFormat 由 Normalize 按 ErrorPath 后缀推导，不对外暴露。
	errorFormat string
}

type SamplingConfig struct {
	Initial    int `yaml:"initial"    json:"initial"`
	Thereafter int `yaml:"thereafter" json:"thereafter"`
}

// DefaultConfig 是本包的默认值真相源。
// 内核的配置插件把 YAML 反序列化进这个结构后调 Normalize 即可。
func DefaultConfig() Config {
	return Config{
		Level:      "info",
		Caller:     true,
		Stacktrace: "error",
		Console: ConsoleConfig{
			Enabled: true,
			Format:  FormatConsole,
			Color:   ColorAuto,
		},
		File: FileConfig{
			Enabled:    false,
			Path:       "logs/app.log",
			Rotate:     RotateDaily,
			MaxSize:    100,
			MaxAge:     30,
			MaxBackups: 30,
			Compress:   true,
		},
		Sampling: SamplingConfig{Initial: 100, Thereafter: 100},
	}
}

// Normalize 填默认值、推导格式、校验枚举。幂等。
func (c *Config) Normalize() error {
	if c.Level == "" {
		c.Level = "info"
	}
	if _, err := ParseLevel(c.Level); err != nil {
		return fmt.Errorf("log.level: %w", err)
	}
	if c.Stacktrace == "" {
		c.Stacktrace = "error"
	}
	if _, err := ParseLevel(c.Stacktrace); err != nil {
		return fmt.Errorf("log.stacktrace: %w", err)
	}

	if c.Console.Format == "" {
		c.Console.Format = FormatConsole
	}
	if err := checkFormat("log.console.format", c.Console.Format); err != nil {
		return err
	}
	if c.Console.Color == "" {
		c.Console.Color = ColorAuto
	}
	switch c.Console.Color {
	case ColorAuto, ColorAlways, ColorNever:
	default:
		return fmt.Errorf("log.console.color: 未知取值 %q，可选 auto/always/never", c.Console.Color)
	}

	if c.File.Enabled {
		if c.File.Path == "" {
			c.File.Path = "logs/app.log"
		}
		if c.File.Format == "" {
			c.File.Format = formatFromPath(c.File.Path)
		}
		if err := checkFormat("log.file.format", c.File.Format); err != nil {
			return err
		}
		if c.File.Rotate == "" {
			c.File.Rotate = RotateDaily
		}
		switch c.File.Rotate {
		case RotateDaily, RotateSize:
		default:
			return fmt.Errorf("log.file.rotate: 未知取值 %q，可选 daily/size", c.File.Rotate)
		}
		if c.File.MaxSize <= 0 {
			c.File.MaxSize = 100
		}
		if c.File.MaxAge < 0 {
			c.File.MaxAge = 0
		}
		if c.File.MaxBackups < 0 {
			c.File.MaxBackups = 0
		}
		if c.File.ErrorPath != "" {
			c.File.errorFormat = formatFromPath(c.File.ErrorPath)
		} else {
			c.File.errorFormat = ""
		}
	}

	if c.Sampling.Initial < 0 {
		c.Sampling.Initial = 0
	}
	if c.Sampling.Thereafter < 0 {
		c.Sampling.Thereafter = 0
	}
	return nil
}

func checkFormat(field, v string) error {
	switch v {
	case FormatConsole, FormatJSON:
		return nil
	default:
		return fmt.Errorf("%s: 未知取值 %q，可选 console/json", field, v)
	}
}

// formatFromPath 按文件后缀推导渲染格式。
// 文件后缀即格式声明：写 .jsonl 就是要机器读，写 .log 就是要人读。
func formatFromPath(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jsonl", ".json", ".ndjson":
		return FormatJSON
	default:
		return FormatConsole
	}
}
