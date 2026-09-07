package gracefulshutdown

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"path"
	"strings"
)

const (
	defaultPath   = "/-/shutdown"
	defaultHeader = "X-XBC-Shutdown-Token"
)

// Config is bound from plugins.gracefulshutdown. The HTTP endpoint is disabled
// by default. Once enabled, callers must either connect directly from a
// loopback address (when AllowLoopback is true) or present Token in Header.
// Forwarded headers are deliberately ignored for the loopback decision.
type Config struct {
	HTTP HTTPConfig `yaml:"http"`
}

// HTTPConfig controls the optional operator endpoint.
type HTTPConfig struct {
	Enabled       bool   `yaml:"enabled"        default:"false"`
	Path          string `yaml:"path"           default:"/-/shutdown"`
	Header        string `yaml:"header"         default:"X-XBC-Shutdown-Token"`
	Token         string `yaml:"token"          mask:"true"`
	AllowLoopback bool   `yaml:"allow_loopback" default:"true"`
}

type endpointConfig struct {
	enabled       bool
	path          string
	header        string
	tokenDigest   [sha256.Size]byte
	hasToken      bool
	allowLoopback bool
}

func defaultConfig() Config {
	return Config{HTTP: HTTPConfig{
		Path:          defaultPath,
		Header:        defaultHeader,
		AllowLoopback: true,
	}}
}

func normalizeConfig(cfg Config) (endpointConfig, error) {
	endpointPath := strings.TrimSpace(cfg.HTTP.Path)
	if endpointPath == "" {
		endpointPath = defaultPath
	}
	if !strings.HasPrefix(endpointPath, "/") || strings.ContainsAny(endpointPath, ":*") {
		return endpointConfig{}, fmt.Errorf("gracefulshutdown: http.path must be a static absolute path")
	}
	endpointPath = path.Clean(endpointPath)
	if endpointPath == "/" {
		return endpointConfig{}, fmt.Errorf("gracefulshutdown: http.path cannot be the root path")
	}

	header := strings.TrimSpace(cfg.HTTP.Header)
	if header == "" {
		header = defaultHeader
	}
	if !validHeaderName(header) {
		return endpointConfig{}, fmt.Errorf("gracefulshutdown: http.header is not a valid HTTP header name")
	}

	token := cfg.HTTP.Token
	if token != "" && len([]byte(token)) < 32 {
		return endpointConfig{}, fmt.Errorf("gracefulshutdown: http.token must contain at least 32 bytes")
	}
	if cfg.HTTP.Enabled && token == "" && !cfg.HTTP.AllowLoopback {
		return endpointConfig{}, fmt.Errorf("gracefulshutdown: enabled HTTP endpoint requires a token or allow_loopback")
	}

	normalized := endpointConfig{
		enabled:       cfg.HTTP.Enabled,
		path:          endpointPath,
		header:        http.CanonicalHeaderKey(header),
		hasToken:      token != "",
		allowLoopback: cfg.HTTP.AllowLoopback,
	}
	if token != "" {
		normalized.tokenDigest = sha256.Sum256([]byte(token))
	}
	return normalized, nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			continue
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
			continue
		default:
			return false
		}
	}
	return true
}
