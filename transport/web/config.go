package web

import (
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	defaultAddr                = ":8080"
	defaultBasePath            = "/"
	defaultReadTimeout         = 10 * time.Second
	defaultReadHeaderTimeout   = 5 * time.Second
	defaultWriteTimeout        = 30 * time.Second
	defaultIdleTimeout         = 60 * time.Second
	defaultMaxHeaderBytes      = 1 << 20 // 1 MiB
	defaultMaxRequestBodyBytes = 10 << 20
	defaultMaxMultipartMemory  = 8 << 20
)

// Config is Web's root-level "web" configuration section. The process-wide
// shutdown timeout deliberately does not live here: it governs the whole
// application, while Server.Stop drains only for the deadline supplied by the
// runtime.
//
// TrustedProxies is empty by default, which makes Gin ignore forwarded client
// addresses unless the deployment explicitly names trusted reverse proxies.
// MaxRequestBodyBytes is a transport-wide hard ceiling. MaxMultipartMemory only
// controls how much parsed multipart data remains in memory; the request-body
// ceiling still limits the complete request.
type Config struct {
	Addr                string        `yaml:"addr"                   default:":8080"   validate:"required"`
	BasePath            string        `yaml:"base_path"              default:"/"       validate:"required,startswith=/"`
	ReadTimeout         time.Duration `yaml:"read_timeout"           default:"10s"     validate:"gt=0"`
	ReadHeaderTimeout   time.Duration `yaml:"read_header_timeout"    default:"5s"      validate:"gt=0"`
	WriteTimeout        time.Duration `yaml:"write_timeout"          default:"30s"     validate:"gt=0"`
	IdleTimeout         time.Duration `yaml:"idle_timeout"           default:"60s"     validate:"gt=0"`
	MaxHeaderBytes      int           `yaml:"max_header_bytes"       default:"1048576" validate:"gt=0"`
	MaxRequestBodyBytes int64         `yaml:"max_request_body_bytes" default:"10485760" validate:"gt=0"`
	MaxMultipartMemory  int64         `yaml:"max_multipart_memory"   default:"8388608" validate:"gt=0"`
	TrustedProxies      []string      `yaml:"trusted_proxies"                          validate:"dive,required"`
}

// DefaultConfig returns the production-safe defaults used by New and by the
// configuration binder.
func DefaultConfig() Config {
	return Config{
		Addr:                defaultAddr,
		BasePath:            defaultBasePath,
		ReadTimeout:         defaultReadTimeout,
		ReadHeaderTimeout:   defaultReadHeaderTimeout,
		WriteTimeout:        defaultWriteTimeout,
		IdleTimeout:         defaultIdleTimeout,
		MaxHeaderBytes:      defaultMaxHeaderBytes,
		MaxRequestBodyBytes: defaultMaxRequestBodyBytes,
		MaxMultipartMemory:  defaultMaxMultipartMemory,
	}
}

// Validate checks constraints that cannot be expressed by configuration tags.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Addr) == "" {
		return fmt.Errorf("web: addr cannot be empty")
	}
	if c.BasePath == "" || c.BasePath[0] != '/' || strings.ContainsAny(c.BasePath, "?#\r\n") {
		return fmt.Errorf("web: base_path must be an absolute HTTP path without a query or fragment")
	}
	if c.ReadTimeout <= 0 || c.ReadHeaderTimeout <= 0 || c.WriteTimeout <= 0 || c.IdleTimeout <= 0 {
		return fmt.Errorf("web: server timeouts must be greater than zero")
	}
	if c.MaxHeaderBytes <= 0 || c.MaxRequestBodyBytes <= 0 || c.MaxMultipartMemory <= 0 {
		return fmt.Errorf("web: request size limits must be greater than zero")
	}
	for _, proxy := range c.TrustedProxies {
		if proxy == "" || proxy != strings.TrimSpace(proxy) {
			return fmt.Errorf("web: trusted_proxies contains an empty or whitespace-padded address")
		}
		if strings.Contains(proxy, "/") {
			if _, _, err := net.ParseCIDR(proxy); err != nil {
				return fmt.Errorf("web: trusted proxy %q is not a valid CIDR: %w", proxy, err)
			}
			continue
		}
		if net.ParseIP(proxy) == nil {
			return fmt.Errorf("web: trusted proxy %q is not a valid IP address", proxy)
		}
	}
	return nil
}

// normalizeConfig supplies defaults for direct Server construction. Assembly
// binding already applies the same defaults and rejects explicitly configured
// zero values before Init runs, but direct tests and embedding hosts should be
// safe without having to reproduce the binder lifecycle.
func normalizeConfig(c Config) (Config, error) {
	defaults := DefaultConfig()
	if c.Addr == "" {
		c.Addr = defaults.Addr
	}
	if c.BasePath == "" {
		c.BasePath = defaults.BasePath
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = defaults.ReadTimeout
	}
	if c.ReadHeaderTimeout == 0 {
		c.ReadHeaderTimeout = defaults.ReadHeaderTimeout
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = defaults.WriteTimeout
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = defaults.IdleTimeout
	}
	if c.MaxHeaderBytes == 0 {
		c.MaxHeaderBytes = defaults.MaxHeaderBytes
	}
	if c.MaxRequestBodyBytes == 0 {
		c.MaxRequestBodyBytes = defaults.MaxRequestBodyBytes
	}
	if c.MaxMultipartMemory == 0 {
		c.MaxMultipartMemory = defaults.MaxMultipartMemory
	}
	c.TrustedProxies = append([]string(nil), c.TrustedProxies...)
	return c, c.Validate()
}
