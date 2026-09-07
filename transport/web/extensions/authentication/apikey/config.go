package apikey

import (
	"fmt"
	"strings"
)

const (
	defaultHeader      = "X-API-Key"
	defaultAppIDHeader = "X-App-ID"
	defaultScheme      = "Bearer"
	defaultMinKeyBytes = 32
)

// Config is bound from plugins.apikey. Static is convenient for bootstrap and
// small installations; production systems can inject a Repository with
// WithRepository. Static entries contain SHA-256 hex digests, never plaintext.
type Config struct {
	Header       string             `yaml:"header"        default:"X-API-Key"`
	AppIDHeader  string             `yaml:"app_id_header" default:"X-App-ID"`
	AllowBearer  bool               `yaml:"allow_bearer"  default:"true"`
	BearerScheme string             `yaml:"bearer_scheme" default:"Bearer"`
	RequireAppID bool               `yaml:"require_app_id"`
	MinKeyBytes  int                `yaml:"min_key_bytes"  default:"32" validate:"min=16,max=4096"`
	Static       []StaticCredential `yaml:"static"`
}

// StaticCredential is a configuration-safe credential. SHA256 must be the
// 64-character lowercase or uppercase hexadecimal SHA-256 digest produced by
// HashKey(key). Plaintext keys are intentionally not part of this API.
type StaticCredential struct {
	ID         string         `yaml:"id"`
	AppID      string         `yaml:"app_id"`
	Subject    string         `yaml:"subject" validate:"required"`
	SHA256     string         `yaml:"sha256" validate:"required,len=64,hexadecimal"`
	Attributes map[string]any `yaml:"attributes"`
	Disabled   bool           `yaml:"disabled"`
}

type normalizedConfig struct {
	header       string
	appIDHeader  string
	allowBearer  bool
	bearerScheme string
	requireAppID bool
	minKeyBytes  int
}

// DefaultConfig returns the secure extraction defaults used by XBC's
// configuration binder and by direct construction with New.
func DefaultConfig() Config {
	return Config{
		Header:       defaultHeader,
		AppIDHeader:  defaultAppIDHeader,
		AllowBearer:  true,
		BearerScheme: defaultScheme,
		MinKeyBytes:  defaultMinKeyBytes,
	}
}

// Validate checks settings that are independent of an injected Repository.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	header := strings.TrimSpace(c.Header)
	if header == "" {
		header = defaultHeader
	}
	appIDHeader := strings.TrimSpace(c.AppIDHeader)
	if appIDHeader == "" {
		appIDHeader = defaultAppIDHeader
	}
	scheme := strings.TrimSpace(c.BearerScheme)
	if scheme == "" {
		scheme = defaultScheme
	}
	minKeyBytes := c.MinKeyBytes
	if minKeyBytes == 0 {
		minKeyBytes = defaultMinKeyBytes
	}
	if !validToken(header) {
		return normalizedConfig{}, fmt.Errorf("apikey: header must be a valid HTTP field name")
	}
	if !validToken(appIDHeader) {
		return normalizedConfig{}, fmt.Errorf("apikey: app_id_header must be a valid HTTP field name")
	}
	if strings.EqualFold(header, appIDHeader) || strings.EqualFold(header, "Authorization") || strings.EqualFold(appIDHeader, "Authorization") {
		return normalizedConfig{}, fmt.Errorf("apikey: credential, app ID, and Authorization headers must be distinct")
	}
	if !validToken(scheme) {
		return normalizedConfig{}, fmt.Errorf("apikey: bearer_scheme must be a valid HTTP authentication scheme")
	}
	if minKeyBytes < 16 || minKeyBytes > 4096 {
		return normalizedConfig{}, fmt.Errorf("apikey: min_key_bytes must be between 16 and 4096")
	}
	return normalizedConfig{
		header:       header,
		appIDHeader:  appIDHeader,
		allowBearer:  c.AllowBearer,
		bearerScheme: scheme,
		requireAppID: c.RequireAppID,
		minKeyBytes:  minKeyBytes,
	}, nil
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		switch ch {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}
