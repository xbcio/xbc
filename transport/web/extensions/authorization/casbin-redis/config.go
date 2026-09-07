package casbinredis

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultAddress      = "127.0.0.1:6379"
	defaultChannel      = "/casbin"
	defaultDialTimeout  = 5 * time.Second
	defaultReadTimeout  = 3 * time.Second
	defaultWriteTimeout = 3 * time.Second
)

// Mode selects the Redis deployment topology used by a watcher.
type Mode string

const (
	// ModeStandalone connects each watcher client to exactly one Redis server.
	ModeStandalone Mode = "standalone"
	// ModeCluster connects each watcher client to the configured cluster seeds.
	ModeCluster Mode = "cluster"
)

// Config configures one Casbin Redis watcher factory. XBC binds it from one
// named section below plugins.casbin-redis.
//
// Addrs contains exactly one host:port in standalone mode and one or more seed
// host:port values in cluster mode. Redis clients are not opened while Config
// is prepared or while the Factory is constructed; Factory.Open owns that I/O.
type Config struct {
	Mode  Mode     `yaml:"mode" default:"standalone" validate:"oneof=standalone cluster"`
	Addrs []string `yaml:"addrs" default:"127.0.0.1:6379" validate:"required,min=1,dive,required,hostname_port"`

	Username string `yaml:"username"`
	Password string `yaml:"password" mask:"true"`
	DB       int    `yaml:"db" default:"0" validate:"min=0"`

	DialTimeout  time.Duration `yaml:"dial_timeout" default:"5s" validate:"gt=0"`
	ReadTimeout  time.Duration `yaml:"read_timeout" default:"3s" validate:"gt=0"`
	WriteTimeout time.Duration `yaml:"write_timeout" default:"3s" validate:"gt=0"`

	Channel    string `yaml:"channel" default:"/casbin" validate:"required"`
	IgnoreSelf bool   `yaml:"ignore_self" default:"true"`
}

// DefaultConfig returns a fresh configuration with production-safe defaults.
func DefaultConfig() Config {
	return Config{
		Mode:         ModeStandalone,
		Addrs:        []string{defaultAddress},
		DialTimeout:  defaultDialTimeout,
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
		Channel:      defaultChannel,
		IgnoreSelf:   true,
	}
}

// Validate checks topology and connection settings without opening Redis
// connections. Password is never included in an error.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

type normalizedConfig struct {
	mode         Mode
	addrs        []string
	username     string
	password     string
	db           int
	dialTimeout  time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration
	channel      string
	ignoreSelf   bool
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	mode := Mode(strings.TrimSpace(string(c.Mode)))
	if mode == "" {
		mode = ModeStandalone
	}
	if mode != ModeStandalone && mode != ModeCluster {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: mode must be %q or %q, got %q", ModeStandalone, ModeCluster, mode)
	}

	if len(c.Addrs) == 0 {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: addrs must contain at least one Redis host:port")
	}
	if mode == ModeStandalone && len(c.Addrs) != 1 {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: standalone mode requires exactly one address, got %d", len(c.Addrs))
	}

	addrs := make([]string, len(c.Addrs))
	seen := make(map[string]struct{}, len(c.Addrs))
	for i, raw := range c.Addrs {
		addr := strings.TrimSpace(raw)
		if err := validateAddress(addr); err != nil {
			return normalizedConfig{}, fmt.Errorf("casbin-redis: addrs[%d]: %w", i, err)
		}
		identity := strings.ToLower(addr)
		if _, duplicate := seen[identity]; duplicate {
			return normalizedConfig{}, fmt.Errorf("casbin-redis: addrs[%d] duplicates %q", i, addr)
		}
		seen[identity] = struct{}{}
		addrs[i] = addr
	}

	if c.DB < 0 {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: db cannot be negative")
	}
	if mode == ModeCluster && c.DB != 0 {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: db must be 0 in cluster mode")
	}
	if c.DialTimeout <= 0 {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: dial_timeout must be positive")
	}
	if c.ReadTimeout <= 0 {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: read_timeout must be positive")
	}
	if c.WriteTimeout <= 0 {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: write_timeout must be positive")
	}

	channel := strings.TrimSpace(c.Channel)
	if channel == "" {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: channel cannot be empty")
	}
	if containsControl(channel) {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: channel cannot contain control characters")
	}
	if containsControl(c.Username) {
		return normalizedConfig{}, fmt.Errorf("casbin-redis: username cannot contain control characters")
	}

	return normalizedConfig{
		mode:         mode,
		addrs:        addrs,
		username:     c.Username,
		password:     c.Password,
		db:           c.DB,
		dialTimeout:  c.DialTimeout,
		readTimeout:  c.ReadTimeout,
		writeTimeout: c.WriteTimeout,
		channel:      channel,
		ignoreSelf:   c.IgnoreSelf,
	}, nil
}

func validateAddress(addr string) error {
	if addr == "" {
		return fmt.Errorf("address cannot be empty")
	}
	host, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q must be host:port: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("address %q has an empty host", addr)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("address %q has an invalid port", addr)
	}
	if !validHost(host) {
		return fmt.Errorf("address %q has an invalid host", addr)
	}
	return nil
}

func validHost(host string) bool {
	ipHost := host
	if zoneAt := strings.LastIndexByte(ipHost, '%'); zoneAt >= 0 {
		if zoneAt == len(ipHost)-1 || containsControl(ipHost[zoneAt+1:]) {
			return false
		}
		ipHost = ipHost[:zoneAt]
	}
	if net.ParseIP(ipHost) != nil {
		return true
	}

	name := strings.TrimSuffix(host, ".")
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || !isAlphaNumeric(label[0]) || !isAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			if !isAlphaNumeric(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func isAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func containsControl(value string) bool {
	return strings.IndexFunc(value, unicode.IsControl) >= 0
}
