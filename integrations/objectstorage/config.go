package objectstorage

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	BackendLocal = "local"
	BackendS3    = "s3"

	defaultMaxObjectBytes int64 = 64 << 20
	defaultMaxListItems         = 1000
)

// Config configures one named object Store below plugins.objectstorage.
type Config struct {
	Backend        string      `yaml:"backend"          default:"local" validate:"required,oneof=local s3"`
	MaxObjectBytes int64       `yaml:"max_object_bytes" default:"67108864" validate:"gt=0"`
	MaxListItems   int         `yaml:"max_list_items"   default:"1000" validate:"gt=0,lte=10000"`
	Local          LocalConfig `yaml:"local"`
	S3             S3Config    `yaml:"s3"`
}

// LocalConfig selects the directory confined by os.OpenRoot. It is created
// with owner/group-only permissions when absent.
type LocalConfig struct {
	Directory string `yaml:"directory" default:"./data/objectstorage" validate:"required"`
}

// S3Config configures an S3-compatible service without exposing SDK types.
// Endpoint may be host[:port] or an http(s) URL without a path. TLS controls
// the scheme and must agree with a URL's explicit scheme. SkipVerify should be
// used only for isolated development endpoints.
type S3Config struct {
	Endpoint     string `yaml:"endpoint"`
	TLS          bool   `yaml:"tls"          default:"true"`
	SkipVerify   bool   `yaml:"skip_verify"  default:"false"`
	Region       string `yaml:"region"       default:"us-east-1"`
	Bucket       string `yaml:"bucket"`
	PathStyle    bool   `yaml:"path_style"   default:"true"`
	AccessKeyID  string `yaml:"access_key_id"`
	SecretKey    string `yaml:"secret_key"`
	SessionToken string `yaml:"session_token"`
	Prefix       string `yaml:"prefix"`

	DialTimeout           time.Duration `yaml:"dial_timeout"            default:"5s" validate:"gt=0"`
	TLSHandshakeTimeout   time.Duration `yaml:"tls_handshake_timeout"   default:"5s" validate:"gt=0"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout" default:"15s" validate:"gt=0"`
	RequestTimeout        time.Duration `yaml:"request_timeout"         default:"1m" validate:"gt=0"`
	IdleConnTimeout       time.Duration `yaml:"idle_conn_timeout"       default:"1m30s" validate:"gt=0"`
}

func defaultConfig() Config {
	return Config{
		Backend:        BackendLocal,
		MaxObjectBytes: defaultMaxObjectBytes,
		MaxListItems:   defaultMaxListItems,
		Local: LocalConfig{
			Directory: "./data/objectstorage",
		},
		S3: S3Config{
			TLS:                   true,
			Region:                "us-east-1",
			PathStyle:             true,
			DialTimeout:           5 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			RequestTimeout:        time.Minute,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// Validate checks backend-specific settings and cross-field constraints that
// cannot be expressed through XBC's struct-tag validation alone.
func (c Config) Validate() error { return validateConfig(c) }

func validateConfig(cfg Config) error {
	if cfg.MaxObjectBytes <= 0 {
		return fmt.Errorf("objectstorage: max_object_bytes must be positive")
	}
	if cfg.MaxListItems <= 0 || cfg.MaxListItems > 10000 {
		return fmt.Errorf("objectstorage: max_list_items must be between 1 and 10000")
	}
	switch cfg.Backend {
	case BackendLocal:
		if strings.TrimSpace(cfg.Local.Directory) == "" {
			return fmt.Errorf("objectstorage: local.directory is required")
		}
	case BackendS3:
		if _, err := normalizedS3Config(cfg.S3); err != nil {
			return err
		}
	default:
		return fmt.Errorf("objectstorage: unsupported backend %q", cfg.Backend)
	}
	return nil
}

func normalizedS3Config(cfg S3Config) (S3Config, error) {
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Bucket = strings.TrimSpace(cfg.Bucket)
	cfg.Prefix = strings.TrimRight(cfg.Prefix, "/")

	if cfg.Region == "" {
		return cfg, fmt.Errorf("objectstorage: s3.region is required")
	}
	if cfg.Bucket == "" || strings.ContainsAny(cfg.Bucket, "/\\\x00") {
		return cfg, fmt.Errorf("objectstorage: s3.bucket must be a non-empty bucket name")
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretKey == "") {
		return cfg, fmt.Errorf("objectstorage: s3.access_key_id and s3.secret_key must be configured together")
	}
	if cfg.AccessKeyID == "" {
		return cfg, fmt.Errorf("objectstorage: s3 credentials are required")
	}
	if err := validatePrefix(cfg.Prefix); err != nil {
		return cfg, fmt.Errorf("objectstorage: invalid s3.prefix: %w", err)
	}
	if cfg.DialTimeout <= 0 || cfg.TLSHandshakeTimeout <= 0 || cfg.ResponseHeaderTimeout <= 0 || cfg.RequestTimeout <= 0 || cfg.IdleConnTimeout <= 0 {
		return cfg, fmt.Errorf("objectstorage: all s3 timeouts must be positive")
	}
	if _, err := endpointURL(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func endpointURL(cfg S3Config) (string, error) {
	if cfg.Endpoint == "" {
		if !cfg.TLS {
			return "", fmt.Errorf("objectstorage: s3.tls=false requires a custom endpoint")
		}
		return "", nil
	}
	raw := cfg.Endpoint
	if !strings.Contains(raw, "://") {
		if cfg.TLS {
			raw = "https://" + raw
		} else {
			raw = "http://" + raw
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("objectstorage: invalid s3.endpoint %q", cfg.Endpoint)
	}
	if (parsed.Scheme == "https") != cfg.TLS {
		return "", fmt.Errorf("objectstorage: s3.endpoint scheme and s3.tls disagree")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("objectstorage: s3.endpoint must not contain credentials, path, query, or fragment")
	}
	return strings.TrimSuffix(raw, "/"), nil
}

func tlsConfig(cfg S3Config) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.SkipVerify} //nolint:gosec // explicitly configured for development endpoints
}
