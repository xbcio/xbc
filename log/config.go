package log

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Render formats.
const (
	FormatConsole = "console" // aligned + colored, for humans
	FormatJSON    = "json"    // one JSON object per line, for machine retrieval
)

// Console color modes.
const (
	ColorAuto   = "auto"
	ColorAlways = "always"
	ColorNever  = "never"
)

// File rotation policies.
const (
	RotateDaily = "daily" // rotate once per day (implemented by this framework; lumberjack itself only rotates by size)
	RotateSize  = "size"  // rotate only by max_size
)

// Config is the log configuration.
//
// Format is per-sink rather than global -- "console for terminal + json for
// file" is the most common combination, and a single global format cannot
// express that.
type Config struct {
	Level      string `yaml:"level"      json:"level"`
	Caller     bool   `yaml:"caller"     json:"caller"`
	Stacktrace string `yaml:"stacktrace" json:"stacktrace"`

	Console ConsoleConfig `yaml:"console" json:"console"`
	File    FileConfig    `yaml:"file"    json:"file"`

	Sampling SamplingConfig `yaml:"sampling" json:"sampling"`

	// MaskFields is appended to the built-in mask blacklist. Additive only -- built-in entries cannot be removed.
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

	// Format is inferred from the Path suffix when left empty:
	// .jsonl/.json/.ndjson -> json, everything else -> console.
	Format string `yaml:"format" json:"format"`

	Rotate     string `yaml:"rotate"      json:"rotate"`
	MaxSize    int    `yaml:"max_size"    json:"max_size"`    // MB
	MaxAge     int    `yaml:"max_age"     json:"max_age"`     // days
	MaxBackups int    `yaml:"max_backups" json:"max_backups"` // count
	Compress   bool   `yaml:"compress"    json:"compress"`

	// ErrorPath, when non-empty, opens an additional sink that only receives error level and above.
	ErrorPath string `yaml:"error_path" json:"error_path"`

	// errorFormat is inferred by Normalize from the ErrorPath suffix and is not exposed externally.
	errorFormat string
}

type SamplingConfig struct {
	Initial    int `yaml:"initial"    json:"initial"`
	Thereafter int `yaml:"thereafter" json:"thereafter"`
}

// DefaultConfig is the source of truth for this package's default values.
// The kernel's config plugin can deserialize YAML into this struct and then call Normalize.
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

// Normalize fills in defaults, infers formats, and validates enums. Idempotent.
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
		return fmt.Errorf("log.console.color: unknown value %q, valid values: auto/always/never", c.Console.Color)
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
			return fmt.Errorf("log.file.rotate: unknown value %q, valid values: daily/size", c.File.Rotate)
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
		return fmt.Errorf("%s: unknown value %q, valid values: console/json", field, v)
	}
}

// formatFromPath infers the render format from the file suffix.
// The file suffix is itself a format declaration: writing .jsonl means it's meant for machines to read, writing .log means it's meant for humans to read.
func formatFromPath(p string) string {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".jsonl", ".json", ".ndjson":
		return FormatJSON
	default:
		return FormatConsole
	}
}
