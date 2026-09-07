package session

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xbcio/xbc/plugin"
)

const (
	BackendMemory = "memory"
	BackendRedis  = "redis"

	defaultCookieName = "xbc_session"
	defaultRedisKey   = "xbc:session:"
)

// Config is bound from plugins.session.
type Config struct {
	Backend          string        `yaml:"backend"           default:"memory"`
	RedisInstance    string        `yaml:"redis_instance"    default:"default"`
	RedisPrefix      string        `yaml:"redis_prefix"      default:"xbc:session:"`
	Name             string        `yaml:"name"              default:"xbc_session"`
	Path             string        `yaml:"path"              default:"/"`
	Domain           string        `yaml:"domain"`
	HTTPOnly         bool          `yaml:"http_only"         default:"true"`
	Secure           bool          `yaml:"secure"            default:"true"`
	SameSite         string        `yaml:"same_site"         default:"lax"`
	TTL              time.Duration `yaml:"ttl"               default:"24h" validate:"gt=0"`
	IdleTTL          time.Duration `yaml:"idle_ttl"          default:"30m" validate:"gt=0"`
	TouchInterval    time.Duration `yaml:"touch_interval"    default:"5m" validate:"gte=0"`
	CleanupInterval  time.Duration `yaml:"cleanup_interval"  default:"1m" validate:"gt=0"`
	OperationTimeout time.Duration `yaml:"operation_timeout" default:"2s" validate:"gt=0"`
	IDBytes          int           `yaml:"id_bytes"          default:"32" validate:"min=32,max=128"`
}

type normalizedConfig struct {
	backend          string
	redisInstance    string
	redisPrefix      string
	name             string
	path             string
	domain           string
	httpOnly         bool
	secure           bool
	sameSite         http.SameSite
	ttl              time.Duration
	idleTTL          time.Duration
	touchInterval    time.Duration
	cleanupInterval  time.Duration
	operationTimeout time.Duration
	idBytes          int
}

// DefaultConfig returns the bounded defaults applied when a field is unset.
func DefaultConfig() Config {
	return Config{
		Backend:          BackendMemory,
		RedisInstance:    "default",
		RedisPrefix:      defaultRedisKey,
		Name:             defaultCookieName,
		Path:             "/",
		HTTPOnly:         true,
		Secure:           true,
		SameSite:         "lax",
		TTL:              24 * time.Hour,
		IdleTTL:          30 * time.Minute,
		TouchInterval:    5 * time.Minute,
		CleanupInterval:  time.Minute,
		OperationTimeout: 2 * time.Second,
		IDBytes:          32,
	}
}

// Validate checks all security-sensitive configuration invariants.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// prepareConfig implements plugin.ConfigSpec.Prepare: it validates c without
// acquiring any resource and returns it unchanged so plan(cfg) can rely on
// already-defaulted, already-validated values.
func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	d := DefaultConfig()
	if strings.TrimSpace(c.Backend) == "" {
		c.Backend = d.Backend
	}
	if strings.TrimSpace(c.RedisInstance) == "" {
		c.RedisInstance = d.RedisInstance
	}
	if c.RedisPrefix == "" {
		c.RedisPrefix = d.RedisPrefix
	}
	if c.Name == "" {
		c.Name = d.Name
	}
	if c.Path == "" {
		c.Path = d.Path
	}
	if strings.TrimSpace(c.SameSite) == "" {
		c.SameSite = d.SameSite
	}
	if c.TTL == 0 {
		c.TTL = d.TTL
	}
	if c.IdleTTL == 0 {
		c.IdleTTL = d.IdleTTL
	}
	if c.TouchInterval == 0 {
		c.TouchInterval = d.TouchInterval
	}
	if c.CleanupInterval == 0 {
		c.CleanupInterval = d.CleanupInterval
	}
	if c.OperationTimeout == 0 {
		c.OperationTimeout = d.OperationTimeout
	}
	if c.IDBytes == 0 {
		c.IDBytes = d.IDBytes
	}

	backend := strings.ToLower(strings.TrimSpace(c.Backend))
	if backend != BackendMemory && backend != BackendRedis {
		return normalizedConfig{}, fmt.Errorf("session: backend must be memory or redis")
	}
	if !validHTTPToken(c.Name) {
		return normalizedConfig{}, fmt.Errorf("session: name must be a valid cookie name")
	}
	if !c.HTTPOnly {
		return normalizedConfig{}, fmt.Errorf("session: http_only must remain enabled")
	}
	if !validCookiePath(c.Path) {
		return normalizedConfig{}, fmt.Errorf("session: path must be an absolute cookie path without control characters or semicolons")
	}
	if !validCookieDomain(c.Domain) {
		return normalizedConfig{}, fmt.Errorf("session: domain contains unsafe characters")
	}
	if strings.HasPrefix(c.Name, "__Host-") && (!c.Secure || c.Path != "/" || c.Domain != "") {
		return normalizedConfig{}, fmt.Errorf("session: __Host- cookies require secure=true, path=/, and no domain")
	}
	if strings.HasPrefix(c.Name, "__Secure-") && !c.Secure {
		return normalizedConfig{}, fmt.Errorf("session: __Secure- cookies require secure=true")
	}
	sameSite, err := parseSameSite(c.SameSite)
	if err != nil {
		return normalizedConfig{}, err
	}
	if sameSite == http.SameSiteNoneMode && !c.Secure {
		return normalizedConfig{}, fmt.Errorf("session: SameSite=None requires secure=true")
	}
	if c.TTL <= 0 || c.IdleTTL <= 0 || c.IdleTTL > c.TTL {
		return normalizedConfig{}, fmt.Errorf("session: idle_ttl must be positive and no greater than ttl")
	}
	if c.TouchInterval < 0 || c.TouchInterval >= c.IdleTTL {
		return normalizedConfig{}, fmt.Errorf("session: touch_interval must be non-negative and less than idle_ttl")
	}
	if c.CleanupInterval <= 0 || c.OperationTimeout <= 0 {
		return normalizedConfig{}, fmt.Errorf("session: cleanup_interval and operation_timeout must be positive")
	}
	if c.IDBytes < 32 || c.IDBytes > 128 {
		return normalizedConfig{}, fmt.Errorf("session: id_bytes must be between 32 and 128")
	}
	redisInstance := plugin.NormalizeInstance(strings.TrimSpace(c.RedisInstance))
	if err := plugin.ValidateInstanceName(redisInstance); err != nil {
		return normalizedConfig{}, fmt.Errorf("session: invalid redis_instance: %w", err)
	}
	if backend == BackendRedis && (strings.TrimSpace(c.RedisPrefix) == "" || containsControl(c.RedisPrefix)) {
		return normalizedConfig{}, fmt.Errorf("session: redis_prefix cannot be empty or contain control characters")
	}
	return normalizedConfig{
		backend:          backend,
		redisInstance:    redisInstance,
		redisPrefix:      c.RedisPrefix,
		name:             c.Name,
		path:             c.Path,
		domain:           strings.TrimSpace(c.Domain),
		httpOnly:         c.HTTPOnly,
		secure:           c.Secure,
		sameSite:         sameSite,
		ttl:              c.TTL,
		idleTTL:          c.IdleTTL,
		touchInterval:    c.TouchInterval,
		cleanupInterval:  c.CleanupInterval,
		operationTimeout: c.OperationTimeout,
		idBytes:          c.IDBytes,
	}, nil
}

func parseSameSite(value string) (http.SameSite, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "default":
		return http.SameSiteDefaultMode, nil
	case "lax":
		return http.SameSiteLaxMode, nil
	case "strict":
		return http.SameSiteStrictMode, nil
	case "none":
		return http.SameSiteNoneMode, nil
	default:
		return 0, fmt.Errorf("session: same_site must be default, lax, strict, or none")
	}
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
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

func validCookiePath(value string) bool {
	if !strings.HasPrefix(value, "/") {
		return false
	}
	return !containsControl(value) && !strings.Contains(value, ";")
}

func validCookieDomain(value string) bool {
	if value == "" {
		return true
	}
	if containsControl(value) || strings.ContainsAny(value, " /:;,@\\") {
		return false
	}
	for _, label := range strings.Split(strings.TrimPrefix(value, "."), ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for i := range len(label) {
			ch := label[i]
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-') {
				return false
			}
		}
	}
	return true
}

func containsControl(value string) bool {
	for i := range len(value) {
		if value[i] < 0x20 || value[i] == 0x7f {
			return true
		}
	}
	return false
}
