package objectstorage

import (
	"strings"
	"testing"
	"time"

	xbcconfig "github.com/xbcio/xbc/config"
)

func TestConfigDefaults(t *testing.T) {
	cfg := bindObjectStorageConfig(t, map[string]any{})
	if cfg.Backend != BackendLocal || cfg.MaxObjectBytes != 64<<20 || cfg.MaxListItems != 1000 {
		t.Fatalf("top-level defaults = %+v", cfg)
	}
	if cfg.Local.Directory != "./data/objectstorage" {
		t.Fatalf("local directory = %q", cfg.Local.Directory)
	}
	if !cfg.S3.TLS || !cfg.S3.PathStyle || cfg.S3.SkipVerify || cfg.S3.Region != "us-east-1" {
		t.Fatalf("S3 identity defaults = %+v", cfg.S3)
	}
	if cfg.S3.DialTimeout != 5*time.Second || cfg.S3.TLSHandshakeTimeout != 5*time.Second ||
		cfg.S3.ResponseHeaderTimeout != 15*time.Second || cfg.S3.RequestTimeout != time.Minute || cfg.S3.IdleConnTimeout != 90*time.Second {
		t.Fatalf("S3 timeout defaults = %+v", cfg.S3)
	}
}

func TestConfigBindsS3Options(t *testing.T) {
	cfg := bindObjectStorageConfig(t, map[string]any{
		"backend":          "s3",
		"max_object_bytes": 12345,
		"max_list_items":   77,
		"local": map[string]any{
			"directory": "/unused",
		},
		"s3": map[string]any{
			"endpoint":                "http://127.0.0.1:9000",
			"tls":                     false,
			"skip_verify":             true,
			"region":                  "test-region-1",
			"bucket":                  "objects",
			"path_style":              false,
			"access_key_id":           "access",
			"secret_key":              "secret",
			"session_token":           "token",
			"prefix":                  "tenant/assets",
			"dial_timeout":            "1s",
			"tls_handshake_timeout":   "2s",
			"response_header_timeout": "3s",
			"request_timeout":         "4s",
			"idle_conn_timeout":       "5s",
		},
	})
	if cfg.Backend != BackendS3 || cfg.MaxObjectBytes != 12345 || cfg.MaxListItems != 77 {
		t.Fatalf("top-level config = %+v", cfg)
	}
	if cfg.S3.Endpoint != "http://127.0.0.1:9000" || cfg.S3.TLS || !cfg.S3.SkipVerify || cfg.S3.PathStyle {
		t.Fatalf("S3 endpoint config = %+v", cfg.S3)
	}
	if cfg.S3.AccessKeyID != "access" || cfg.S3.SecretKey != "secret" || cfg.S3.SessionToken != "token" || cfg.S3.Prefix != "tenant/assets" {
		t.Fatalf("S3 credentials/prefix = %+v", cfg.S3)
	}
	if cfg.S3.DialTimeout != time.Second || cfg.S3.TLSHandshakeTimeout != 2*time.Second ||
		cfg.S3.ResponseHeaderTimeout != 3*time.Second || cfg.S3.RequestTimeout != 4*time.Second || cfg.S3.IdleConnTimeout != 5*time.Second {
		t.Fatalf("S3 timeout config = %+v", cfg.S3)
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatalf("validateConfig() error = %v", err)
	}
}

func TestConfigSchemaValidationAndStrictBinding(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		field  string
	}{
		{name: "backend", values: map[string]any{"backend": "filesystem"}, field: ".backend"},
		{name: "object limit", values: map[string]any{"max_object_bytes": 0}, field: ".max_object_bytes"},
		{name: "list limit", values: map[string]any{"max_list_items": 10001}, field: ".max_list_items"},
		{name: "local directory", values: map[string]any{"local": map[string]any{"directory": ""}}, field: ".local.directory"},
		{name: "request timeout", values: map[string]any{"s3": map[string]any{"request_timeout": "0s"}}, field: ".s3.request_timeout"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := bindObjectStorageConfigWithoutValidation(test.values)
			if err != nil {
				t.Fatalf("Bind() error = %v", err)
			}
			err = xbcconfig.Validate(&cfg, "plugins.objectstorage.default")
			if err == nil || !strings.Contains(err.Error(), "plugins.objectstorage.default"+test.field) {
				t.Fatalf("Validate() error = %v, want field %s", err, test.field)
			}
		})
	}
	if _, err := bindObjectStorageConfigWithoutValidation(map[string]any{"s3": map[string]any{"access_key": "typo"}}); err == nil {
		t.Fatal("unknown field Bind() error = nil")
	}
}

func TestBackendSpecificConfigValidation(t *testing.T) {
	valid := defaultConfig()
	valid.Backend = BackendS3
	valid.S3.Endpoint = "http://127.0.0.1:9000"
	valid.S3.TLS = false
	valid.S3.Bucket = "bucket"
	valid.S3.AccessKeyID = "access"
	valid.S3.SecretKey = "secret"
	if err := validateConfig(valid); err != nil {
		t.Fatalf("valid S3 config error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "bucket", mutate: func(c *Config) { c.S3.Bucket = "" }, want: "bucket"},
		{name: "partial credentials", mutate: func(c *Config) { c.S3.SecretKey = "" }, want: "configured together"},
		{name: "missing credentials", mutate: func(c *Config) { c.S3.AccessKeyID, c.S3.SecretKey = "", "" }, want: "credentials are required"},
		{name: "unsafe prefix", mutate: func(c *Config) { c.S3.Prefix = "../escape" }, want: "prefix"},
		{name: "endpoint path", mutate: func(c *Config) { c.S3.Endpoint = "http://127.0.0.1:9000/base" }, want: "must not contain"},
		{name: "scheme TLS mismatch", mutate: func(c *Config) { c.S3.Endpoint = "https://127.0.0.1:9000" }, want: "disagree"},
		{name: "non-TLS AWS endpoint", mutate: func(c *Config) { c.S3.Endpoint = "" }, want: "requires a custom endpoint"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateConfig() error = %v, want %q", err, test.want)
			}
		})
	}
}

func bindObjectStorageConfig(t *testing.T, values map[string]any) Config {
	t.Helper()
	cfg, err := bindObjectStorageConfigWithoutValidation(values)
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := xbcconfig.Validate(&cfg, "plugins.objectstorage.default"); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	return cfg
}

func bindObjectStorageConfigWithoutValidation(values map[string]any) (Config, error) {
	env, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"objectstorage": map[string]any{
				"default": values,
			},
		},
	}, "XBC_OBJECTSTORAGE_TEST_")
	if err != nil {
		return Config{}, err
	}
	cfg := defaultConfig()
	if err := env.Bind("plugins.objectstorage.default", &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
