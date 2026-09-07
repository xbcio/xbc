package gzip

import (
	compressgzip "compress/gzip"
	"fmt"
	"mime"
	"strings"
)

var defaultContentTypes = []string{
	"text/*",
	"application/json",
	"application/javascript",
	"application/xml",
	"application/xhtml+xml",
	"application/wasm",
	"image/svg+xml",
	"font/ttf",
	"font/otf",
	"application/vnd.ms-fontobject",
}

// Config is bound from plugins.gzip.
type Config struct {
	Level        int      `yaml:"level"          default:"-1" validate:"min=-2,max=9"`
	MinLength    int      `yaml:"min_length"     default:"1024" validate:"min=0"`
	ContentTypes []string `yaml:"content_types"  default:"text/*,application/json,application/javascript,application/xml,application/xhtml+xml,application/wasm,image/svg+xml,font/ttf,font/otf,application/vnd.ms-fontobject"`
	ExcludePaths []string `yaml:"exclude_paths"`
}

// DefaultConfig returns the same defaults applied by XBC's config binder.
func DefaultConfig() Config {
	return Config{
		Level:        compressgzip.DefaultCompression,
		MinLength:    1024,
		ContentTypes: append([]string(nil), defaultContentTypes...),
	}
}

// Validate checks compression and matcher settings.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type pathRule struct {
	value  string
	prefix bool
}

type mediaRule struct {
	typeName string
	subtype  string
}

type normalizedConfig struct {
	level        int
	minLength    int
	contentTypes []mediaRule
	excludePaths []pathRule
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	if c.Level != compressgzip.DefaultCompression && c.Level != compressgzip.HuffmanOnly &&
		(c.Level < compressgzip.NoCompression || c.Level > compressgzip.BestCompression) {
		return normalizedConfig{}, fmt.Errorf("gzip: level must be -2, -1, or between 0 and 9")
	}
	if c.MinLength < 0 {
		return normalizedConfig{}, fmt.Errorf("gzip: min_length cannot be negative")
	}
	if len(c.ContentTypes) == 0 {
		return normalizedConfig{}, fmt.Errorf("gzip: content_types cannot be empty")
	}
	mediaRules := make([]mediaRule, 0, len(c.ContentTypes))
	for _, raw := range c.ContentTypes {
		value := strings.ToLower(strings.TrimSpace(raw))
		parts := strings.Split(value, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "*" || strings.Contains(parts[0], "*") || (parts[1] != "*" && strings.Contains(parts[1], "*")) {
			return normalizedConfig{}, fmt.Errorf("gzip: invalid content type matcher %q", raw)
		}
		if _, _, err := mime.ParseMediaType(strings.Replace(value, "*", "plain", 1)); err != nil {
			return normalizedConfig{}, fmt.Errorf("gzip: invalid content type matcher %q", raw)
		}
		mediaRules = append(mediaRules, mediaRule{typeName: parts[0], subtype: parts[1]})
	}
	paths := make([]pathRule, 0, len(c.ExcludePaths))
	for _, raw := range c.ExcludePaths {
		value := strings.TrimSpace(raw)
		if value == "" || value[0] != '/' || strings.ContainsAny(value, "?#\r\n") {
			return normalizedConfig{}, fmt.Errorf("gzip: invalid excluded path %q", raw)
		}
		prefix := strings.HasSuffix(value, "*")
		if prefix {
			value = strings.TrimSuffix(value, "*")
		}
		if strings.Contains(value, "*") {
			return normalizedConfig{}, fmt.Errorf("gzip: wildcard is only allowed at the end of excluded path %q", raw)
		}
		paths = append(paths, pathRule{value: value, prefix: prefix})
	}
	return normalizedConfig{level: c.Level, minLength: c.MinLength, contentTypes: mediaRules, excludePaths: paths}, nil
}

func (c normalizedConfig) matchesContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	parts := strings.SplitN(strings.ToLower(mediaType), "/", 2)
	if len(parts) != 2 {
		return false
	}
	for _, rule := range c.contentTypes {
		if rule.typeName == parts[0] && (rule.subtype == "*" || rule.subtype == parts[1]) {
			return true
		}
	}
	return false
}

func (c normalizedConfig) excludedPath(path string) bool {
	for _, rule := range c.excludePaths {
		if (!rule.prefix && path == rule.value) || (rule.prefix && strings.HasPrefix(path, rule.value)) {
			return true
		}
	}
	return false
}
