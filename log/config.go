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

	// Trace overrides the field names under which trace context is emitted.
	// Empty names keep the built-in ones, so the zero value reproduces the
	// historical output exactly. See TraceConfig.
	Trace TraceConfig `yaml:"trace" json:"trace"`

	// MaskFields is appended to the built-in mask blacklist. Additive only -- built-in entries cannot be removed.
	MaskFields []string `yaml:"mask_fields" json:"mask_fields"`

	// Service is the process identity the runtime attaches before Init, so
	// every JSON-encoded sink stamps it onto each record. It is deliberately
	// not a configuration key: yaml:"-" keeps it out of the schema, console
	// sinks ignore it, and a caller that leaves it zero -- a library
	// embedding, log's own tests -- gets exactly the output it always had.
	Service ServiceIdentity `yaml:"-" json:"-"`
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

// TraceConfig overrides the field names under which trace context is emitted.
//
// An empty name keeps the built-in one (trace_id, span_id, span_name,
// request_id), so the zero value leaves both the JSON sinks and the console
// layout byte-for-byte unchanged. Names are applied by Init; they affect
// loggers derived afterwards (Extract, Span, and Ctx's slow path fix trace
// fields into a logger at derivation time).
//
// The console encoder keeps its fixed trace column and its ID-field exclusion
// pointed at the configured names, so renaming a field does not push a raw
// 128-bit ID into the human-readable KV section.
//
// A name must be non-blank and must not collide with another trace field or
// with a key the package's own JSON encoder writes (ts/level/msg/caller/stack);
// Normalize enforces both.
type TraceConfig struct {
	TraceID   string `yaml:"trace_id"   json:"trace_id"`
	SpanID    string `yaml:"span_id"    json:"span_id"`
	SpanName  string `yaml:"span_name"  json:"span_name"`
	RequestID string `yaml:"request_id" json:"request_id"`
}

const (
	defaultTraceIDField   = "trace_id"
	defaultSpanIDField    = "span_id"
	defaultSpanNameField  = "span_name"
	defaultRequestIDField = "request_id"
)

// defaultTraceConfig is the field layout this package emitted before the
// names were configurable. It is both the zero-value fill-in and the value
// Init publishes when a config leaves Trace untouched.
func defaultTraceConfig() TraceConfig {
	return TraceConfig{
		TraceID:   defaultTraceIDField,
		SpanID:    defaultSpanIDField,
		SpanName:  defaultSpanNameField,
		RequestID: defaultRequestIDField,
	}
}

// reservedEncoderKeys are the keys this package's own JSON encoders write: the
// five base keys jsonEncoderConfig sets, plus the three identityEncoder
// prepends when Config.Service is populated. A trace field reusing one would
// make the encoder emit a duplicate JSON object key.
var reservedEncoderKeys = map[string]struct{}{
	"ts":                  {},
	"level":               {},
	"msg":                 {},
	"caller":              {},
	"stack":               {},
	"service.name":        {},
	"service.version":     {},
	"service.instance.id": {},
}

// normalize fills built-in names and rejects a layout that cannot be encoded
// unambiguously. Idempotent.
func (c *TraceConfig) normalize() error {
	fields := []struct {
		name  string
		value *string
		def   string
	}{
		{"log.trace.trace_id", &c.TraceID, defaultTraceIDField},
		{"log.trace.span_id", &c.SpanID, defaultSpanIDField},
		{"log.trace.span_name", &c.SpanName, defaultSpanNameField},
		{"log.trace.request_id", &c.RequestID, defaultRequestIDField},
	}
	for _, f := range fields {
		if *f.value == "" {
			*f.value = f.def
			continue
		}
		if strings.TrimSpace(*f.value) == "" {
			return fmt.Errorf("%s: field name must not be blank", f.name)
		}
	}
	seen := make(map[string]string, len(fields))
	for _, f := range fields {
		if _, reserved := reservedEncoderKeys[*f.value]; reserved {
			return fmt.Errorf("%s: field name %q is reserved by the encoder", f.name, *f.value)
		}
		if prev, dup := seen[*f.value]; dup {
			return fmt.Errorf("%s: field name %q already used by %s", f.name, *f.value, prev)
		}
		seen[*f.value] = f.name
	}
	return nil
}

// hiddenUnderConsole reports whether name is one of the ID fields the console
// encoder keeps out of its KV section: the trace ID already occupies a fixed
// column, and the other two are long IDs that add no value to a human-readable
// line. The JSON sinks do not apply this exclusion -- those are for machine
// search.
func (c TraceConfig) hiddenUnderConsole(name string) bool {
	return name == c.TraceID || name == c.SpanID || name == c.RequestID
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
		Trace:    defaultTraceConfig(),
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
	if err := c.Trace.normalize(); err != nil {
		return err
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
