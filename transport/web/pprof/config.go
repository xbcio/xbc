package pprof

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"path"
	"strings"
)

const (
	defaultPath   = "/debug/pprof"
	defaultHeader = "X-XBC-Pprof-Token"
)

// Config is bound from plugins.pprof. A token, when configured, must contain at
// least 32 bytes. Disabling loopback access requires such a token.
type Config struct {
	Enabled       bool   `yaml:"enabled"`
	Path          string `yaml:"path"           default:"/debug/pprof"`
	Header        string `yaml:"header"         default:"X-XBC-Pprof-Token"`
	Token         string `yaml:"token"`
	AllowLoopback bool   `yaml:"allow_loopback" default:"true"`
}

type settings struct {
	enabled       bool
	path          string
	header        string
	tokenDigest   [sha256.Size]byte
	hasToken      bool
	allowLoopback bool
}

func defaultConfig() Config {
	return Config{Path: defaultPath, Header: defaultHeader, AllowLoopback: true}
}

func normalizeConfig(cfg Config) (settings, error) {
	endpointPath := strings.TrimSpace(cfg.Path)
	if endpointPath == "" {
		endpointPath = defaultPath
	}
	if !strings.HasPrefix(endpointPath, "/") || strings.ContainsAny(endpointPath, ":*?#") {
		return settings{}, fmt.Errorf("pprof: path must be a static absolute HTTP path")
	}
	endpointPath = strings.TrimSuffix(path.Clean(endpointPath), "/")
	if endpointPath == "" || endpointPath == "/" {
		return settings{}, fmt.Errorf("pprof: path cannot be the root path")
	}

	header := strings.TrimSpace(cfg.Header)
	if header == "" {
		header = defaultHeader
	}
	if !validHeaderName(header) {
		return settings{}, fmt.Errorf("pprof: header is not a valid HTTP header name")
	}

	token := cfg.Token
	if token != "" && len([]byte(token)) < 32 {
		return settings{}, fmt.Errorf("pprof: token must contain at least 32 bytes")
	}
	if cfg.Enabled && token == "" && !cfg.AllowLoopback {
		return settings{}, fmt.Errorf("pprof: enabled endpoint requires a token or allow_loopback")
	}

	out := settings{
		enabled:       cfg.Enabled,
		path:          endpointPath,
		header:        http.CanonicalHeaderKey(header),
		hasToken:      token != "",
		allowLoopback: cfg.AllowLoopback,
	}
	if token != "" {
		out.tokenDigest = sha256.Sum256([]byte(token))
	}
	return out, nil
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
